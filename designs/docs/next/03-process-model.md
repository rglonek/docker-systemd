# 03 — Process model: supervision and PID tracking without privileges

This is the core document. Everything else in the design is ordinary systems
programming; this is the part that is genuinely hard, and the part the current
implementation gets wrong.

---

## 1. The problem statement

A service manager must answer four questions about each unit, continuously:

1. **Membership** — which processes currently belong to this unit?
2. **Liveness** — is the unit still running, and when exactly did it stop?
3. **Attribution** — when a process exits, which unit's exit status is that?
4. **Termination** — how do I stop *all* of the unit's processes, including ones it
   spawned that I never saw?

On a normal Linux host, systemd answers all four with **cgroups**: every unit gets a
cgroup, membership is exact and kernel-enforced, `cgroup.events` signals emptiness, and
`cgroup.kill` terminates the set atomically. There is no way to escape a cgroup without
privileges.

Inside an unprivileged container we have none of that. Measured:

```
$ mkdir /sys/fs/cgroup/probe-test
mkdir: cannot create directory: Read-only file system
```

Docker mounts the cgroup hierarchy read-only unless the container is privileged or the
runtime is explicitly configured to delegate. **The design's hard requirement is that it
work with plain `docker run`**, so cgroups can only ever be an optional accelerator.

Worse, the naive fallback — "the unit is the process I spawned" — fails for the single
most common pattern in real service software: **daemonisation**. A daemonising process
forks, the parent exits immediately, and the surviving grandchild is reparented to
whatever the nearest reaper is. On a host that is systemd; in a container it is PID 1
— *our* PID 1 — at which point the parent/child relationship that told us which unit
the process belonged to is gone.

## 2. What the current implementation does, and why it does not work

v0.5.x injects a shared object into every process in the container via
`/etc/ld.so.preload`, interposes `fork()` and `execve()`, and streams parent/child edges
to PID 1 so the fork graph can be reconstructed and intersected with "processes seen
with `getppid() == 1`". Full description in [01 §3](01-current-system-review.md#3-how-pid-tracking-works-today).

The idea is sound in the abstract. Symbol interposition is not a sound *foundation*,
for a reason that is structural rather than incidental: **interposition only sees calls
that go through the PLT**. Calls made inside libc to libc's own internal aliases
(`__fork`, `__execve`) never do. So the very functions that exist to daemonise are
precisely the ones that bypass the hook.

Measured with the shipped `preload.c`, compiled as-is, inside an unprivileged container:

| Child-creation path | events observed |
|---|---|
| `fork(3)` | 1 ✅ |
| `system(3)` | 1 ✅ |
| `vfork(3)` | 0 ❌ |
| `posix_spawn(3)` | 0 ❌ |
| **`daemon(3)`** | **0 ❌** |
| static binary (no glibc PLT at all) | 0 ❌ |

Additional structural limits, independent of the above:

* `/etc/ld.so.preload` is a **glibc** feature. musl ignores it, so Alpine-based images
  are entirely untracked.
* For `AT_SECURE` (setuid) binaries the loader rejects preload paths containing a
  slash, so setuid daemons are untracked.
* Statically linked binaries — Go, Rust, `busybox --static` — have no dynamic loader.
* The pid graph is never pruned, so PID 1's memory grows by one entry per `exec` in the
  container forever, and PID reuse silently aliases old edges onto new processes.
* Every `exec` anywhere in the container pays two `access(2)` calls plus a
  socket round-trip to PID 1.

**Conclusion: the mechanism must be replaced, not repaired.**

## 3. Options considered

| Option | Privileges needed | Completeness | Verdict |
|---|---|---|---|
| **cgroup v2** | writable/delegated `/sys/fs/cgroup` — **not available** under plain `docker run` | perfect | Optional accelerator only (§5) |
| **`PR_SET_CHILD_SUBREAPER`** | **none** | complete: a subreaper's subtree is closed, nothing can escape upward | **Chosen** (§4, §6) |
| netlink `PROC_EVENT_FORK/EXEC/EXIT` connector | `CAP_NET_ADMIN` | perfect, push-based | Rejected: needs a capability |
| per-unit PID namespace (`CLONE_NEWPID`) | `CAP_SYS_ADMIN`; also blocked by Docker's default seccomp | perfect | Rejected: needs privileges |
| `ptrace` with `PTRACE_O_TRACEFORK|VFORK|CLONE|EXEC` | none for own descendants, but seccomp/`yama` dependent | perfect | Rejected: monopolises the single tracer slot per process (breaks `gdb`/`strace` **inside** the container), materially slows every fork/exec, and turns a tracer crash into a mass `SIGKILL` |
| process groups (`setpgid` + `kill(-pgid)`) | none | **insufficient** — measured below | Used only as a cheap *supplement* |
| session id (`setsid` + match `sid`) | none | insufficient — daemonising *is* calling `setsid()` | Rejected |
| `/proc` polling from PID 1 alone | none | races on short-lived processes; cannot attribute an orphan to a unit | Used only as reconciliation |
| inherited "liveness" pipe fd | none | detects "tree empty" cheaply, but daemons that `closefrom()` break it | Optional hint, not truth |
| `LD_PRELOAD` interposition | none | **incomplete, measured** | Rejected (ADR-1) |

## 4. Empirical results

All probes run inside an unprivileged container (kernel 6.17, glibc, default Docker
seccomp), reproduced by the harness in [11 §6](11-testing.md#6-mechanism-probes).

### 4.1 Subreaper adopts an escaped daemon

Target: the classic double-fork daemon (fork → parent exits → `setsid` → fork → middle
exits → grandchild is the daemon, which itself forks a worker).

```
supervisor pid 531 subreaper=on
direct child (pgid leader): 536
L1 parent 536 exiting, child 537
  REAPED pid=536 status=0
L2 mid 537 exiting, grandchild 538
  REAPED pid=537 status=0
DAEMON pid=538 worker=539 ppid=537
live tree rooted at supervisor: [539 538]
   pid=539 comm=sleep      ppid=538
   pid=538 comm=daemonize  ppid=531     <- reparented to the SUPERVISOR, not to PID 1
```

### 4.2 The same, for `daemon(3)` — the case the shim scores 0 on

```
supervisor pid 742 subreaper=on
direct child (pgid leader): 747
  REAPED pid=747 status=0
live tree rooted at supervisor: [748]
   pid=748 comm=gend ppid=742
```

### 4.3 Process-group kill is **not** sufficient

```
--- killing whole pgid 747 ---
tree after pgid TERM: [748]          <- still alive
--- killing survivors individually ---
tree after individual KILL: []
```

Daemonising means `setsid()`, which creates a new session *and* a new process group.
Anything that stops a service by signalling the original process group will leave real
daemons running. This is why the termination ladder in §8 enumerates and signals tree
members individually.

### 4.4 `pidfd` works on non-children

```
grandchild (non-child of us): 583
pidfd_open OK fd=5
poll returned n=1 revents=0x1 after 2s   <- exit notification for a NON-child
pidfd_send_signal(SIGKILL) err: <nil>
```

`pidfd_open(2)` (Linux ≥ 5.3) and `pidfd_send_signal(2)` (≥ 5.1) are permitted by
Docker's default seccomp profile. This gives race-free waiting on and signalling of
adopted PIDs — no `(pid, starttime)` re-validation dance, no risk of signalling a
recycled PID.

## 5. Backend selection

Probe once at boot, in order, and log the outcome prominently:

```
BACKEND-SELECT:
  1. cgroup-v2      if  /sys/fs/cgroup is cgroup2 AND a subdirectory can be created
                        under the container's own cgroup AND cgroup.procs is writable
  2. subreaper      if  prctl(PR_SET_CHILD_SUBREAPER, 1) succeeds in a probe child
  3. degraded       otherwise  (direct children only; log a loud warning)
```

Probe 2 is expected to succeed on every Linux ≥ 3.4, i.e. always. Probe 1 is expected to
fail under plain `docker run` and to succeed under `--privileged`, under Podman with
cgroup delegation, and on hosts that explicitly delegate.

The **backend is an implementation detail behind one interface**; the supervisor logic
in §6–§8 is written once against it:

```
interface UnitCgroupBackend:
    attach(unit) -> handle         # called in the child, before exec, or by the supervisor on itself
    members(handle) -> set[ProcRef]
    is_empty(handle) -> bool
    wait_empty(handle, deadline) -> bool     # blocking, event-driven where possible
    kill_all(handle, signal)
    release(handle)
```

* `cgroup-v2`: `members` reads `cgroup.procs`; `wait_empty` uses `inotify` on
  `cgroup.events`; `kill_all` writes `cgroup.kill` (or iterates for non-`SIGKILL`).
* `subreaper`: as specified below.
* `degraded`: `members` = the direct children only.

**The rest of this document specifies the `subreaper` backend**, since it is the one
that must always work.

## 6. The supervisor model

### 6.1 Topology

```
PID 1  init  (subreaper by definition; reaper of last resort)
  │  control socket, unit registry, dependency graph, job scheduler
  │
  ├── supervisor(nginx)      <- PR_SET_CHILD_SUBREAPER=1
  │     └── nginx master     <- reparented here when it daemonised
  │           ├── worker
  │           └── worker
  ├── supervisor(cron)
  │     └── cron
  └── supervisor(postfix)
        ├── master
        ├── qmgr
        └── pickup
```

One supervisor process per **active** unit. The supervisor is the same binary re-exec'd
as `<self> --supervise` (`/proc/self/exe`), which means:

* no extra artefact to ship, no `.so`, no C;
* the supervisor may be written in a memory-safe, multi-threaded language, because
  everything privileged happens *after* `exec`, not between `fork` and `exec`
  ([10 §3](10-language-choice.md#3-the-fork-without-exec-question));
* a supervisor crash is contained: it kills one unit, not the container.

### 6.2 Why this solves the problem

`PR_SET_CHILD_SUBREAPER` makes a process act as `init` for its own descendants: when
any descendant is orphaned, it is reparented to the **nearest ancestor marked as a
subreaper**, not to PID 1.

Two properties follow, and they are the whole design:

> **P1 (closure).** No descendant of a subreaper can leave its subtree. Reparenting only
> ever moves a process *up* to the nearest subreaper ancestor; there is no operation
> that moves it further. Therefore the set of processes belonging to a unit is exactly
> the set of processes whose ppid-chain reaches that unit's supervisor.

> **P2 (attribution).** Every exit inside the subtree is eventually reported to the
> supervisor via `wait`, because an orphan's parent becomes the supervisor. The
> supervisor learns of exits it never spawned.

Compare with interposition, which can only observe what it is lucky enough to hook, and
with PID 1 adoption, which loses attribution the moment the middle process dies.

### 6.3 Supervisor startup sequence

```
supervisor(unit, config_fd, control_fd, log_fd):
    prctl(PR_SET_CHILD_SUBREAPER, 1)          # BEFORE spawning anything
    read unit configuration from config_fd     # already parsed by PID 1; no file access
    install SIGCHLD handling (signalfd / self-pipe / dedicated wait thread)
    if Type == notify:
        create $NOTIFY_SOCKET (SOCK_DGRAM, mode 0600) and export it
    state = ACTIVATING; report(state)
    run ExecCondition* ; on nonzero -> state = INACTIVE(condition-failed); exit 0
    run ExecStartPre*  ; on failure -> FAILED
    spawn ExecStart    ; per §6.4
    determine MainPID  ; per §7
    run ExecStartPost*
    state = ACTIVE; report(state)
    loop: serve control_fd, reap children, watch tree, handle Restart=
```

`ExecStartPre`/`ExecStartPost`/`ExecStop*` children are also inside the subtree and thus
tracked, but they are *not* the unit's main process; they are waited on synchronously
with `TimeoutStartSec`/`TimeoutStopSec` enforcement.

### 6.4 Spawning a unit process

Executed in the child between `fork` and `exec` — or via the platform's equivalent
pre-exec hook (see [10 §3](10-language-choice.md#3-the-fork-without-exec-question)) —
using only async-signal-safe operations:

```
child:
    setsid()                            # new session: detaches from init's controlling tty
    setpgid(0, 0)                       # own process group (cheap supplement for §8 step 3)
    if User=/Group= set:
        setgroups(supplementary groups of User=)
        setgid(gid); setuid(uid)        # in this order
    apply UMask=, Nice=, OOMScoreAdjust=, RLIMIT_* (see 05 §7)
    chdir(WorkingDirectory= or "/")
    redirect stdin  <- /dev/null   (or StandardInput=)
    redirect stdout -> log pipe    (or StandardOutput=)
    redirect stderr -> log pipe    (or StandardError=)
    close all other descriptors above 2
    execve(argv[0], argv, envp)         # NO SHELL - see 05 §4
```

`setsid()` is deliberate: it makes the unit's processes immune to terminal signals
delivered to init's session, which is what allows `docker attach` + Ctrl-C to reach
init without stray-killing services.

### 6.5 Tree enumeration

```
enumerate_tree(root_pid) -> set[ProcRef]:
    # one pass over /proc; O(number of processes in the container), typically 10-200
    parent = {}; start = {}; pgid = {}
    for each numeric entry pid in /proc:
        line = read("/proc/<pid>/stat")               # single read, no allocation churn
        # field 2 is comm in parentheses and may contain spaces and ')';
        # split after the LAST ')' to reach fields 3+
        tail = line[last_index_of(line, ')')+2 :]
        f = split_ws(tail)
        parent[pid] = int(f[1])       # ppid   (field 4)
        pgid[pid]   = int(f[2])       # pgrp   (field 5)
        start[pid]  = int(f[19])      # starttime (field 22)
    result = {}
    for pid in parent:
        p = pid
        for hops in 0..MAX_DEPTH:                     # MAX_DEPTH = 64, cycle guard
            pp = parent[p]
            if pp <= 1: break                          # reached init or reaped
            if pp == root_pid: result.add(ProcRef(pid, start[pid], pgid[pid])); break
            p = pp
    return result
```

A `ProcRef` carries `(pid, starttime)` so that any later use can detect PID reuse; where
`pidfd_open` is available the supervisor upgrades a `ProcRef` to a pidfd on first use
and never touches the raw pid again.

Correctness relies on **P1**: no member of the unit can have a ppid chain that leaves
the supervisor, so a single `/proc` pass is exhaustive. Races are one-sided: a process
forked *during* the scan may be missed, which is why §8 re-enumerates in a loop.

### 6.6 When the tree is walked

Never in a hot loop. Triggers only:

| Trigger | Frequency |
|---|---|
| `SIGCHLD` when the direct child of a `Type=forking` unit exits | once per start |
| `sd_notify MAINPID=` received | once |
| `systemctl status` on this unit | on demand |
| each iteration of the termination ladder | ~20 times per stop, 50 ms apart |
| heartbeat, **only while the unit has any non-direct-child members** | 1/s, configurable |

For the overwhelmingly common case — `Type=simple`, one process, no escapees — the
supervisor never walks `/proc` at all: `waitid()` on the direct child is sufficient and
is a blocking, zero-cost wait.

### 6.7 Failure modes

| Failure | Behaviour |
|---|---|
| Supervisor is killed (OOM, bug) | Its descendants reparent to the next subreaper up = **PID 1**. PID 1 detects the supervisor's abnormal exit, marks the unit `failed`, and runs *orphan recovery* (§6.8) before restarting per `Restart=` |
| PID 1 is killed | The container dies. Nothing to do |
| `prctl` unavailable (kernel < 3.4) | `degraded` backend, loud warning at boot |
| `/proc` unreadable (`hidepid`) | `degraded` backend, loud warning |
| PID exhaustion during the ladder | Ladder retries with backoff; global stop deadline still applies |
| A unit process is `SIGSTOP`ped | It stays in the tree, `SIGTERM` is queued but not acted on; the ladder escalates to `SIGKILL` at the deadline, which is not blockable. Match systemd: send `SIGCONT` after `SIGTERM` when `KillMode` is not `none` |

### 6.8 Orphan recovery

Belt-and-braces for the supervisor-death case, and for units that were running before a
`daemon-reexec`:

1. PID 1 injects `MANAGED_BY_UNIT=<unit>` and `MANAGED_BY_INVOCATION=<uuid>` into every
   unit process's environment (the current code already does something similar with
   `SYSTEMD_SERVICE_NAME`, and it works, because `environ` is inherited across `fork`
   and preserved across `exec` by the spawning process).
2. On supervisor loss, PID 1 scans `/proc/*/environ` for that invocation id, and hands
   the resulting pid set to the replacement supervisor, which adopts them: it cannot
   become their parent, but it can `pidfd_open` each one to wait on it and signal it.
3. Adopted-by-recovery processes are flagged, because P1 does not hold for them — their
   own future children may escape to PID 1. The replacement supervisor therefore also
   re-scans by environment marker on its heartbeat while any recovered pid is alive.

This is a strictly better use of the environment-marker idea than the current code's,
which uses it as a *primary* mechanism rather than a recovery one.

## 7. Main-PID determination

`MainPID` matters for `systemctl status`, for `ExecReload`'s default `SIGHUP` target,
for `Restart=on-failure` exit-code evaluation, and for `KillMode=process`.

Resolved in strict priority order:

| `Type=` | Rule |
|---|---|
| `simple`, `exec`, `idle` | the direct child. `exec` additionally waits for a successful `execve` before declaring `ACTIVE` (detect via a close-on-exec pipe: the child holds a `CLOEXEC` pipe write end; EOF ⇒ exec succeeded, a written errno ⇒ exec failed) |
| `oneshot` | no MainPID; the unit is `activating` until every `ExecStart=` line has run **sequentially** to completion, then `inactive` (or `active (exited)` if `RemainAfterExit=yes`) |
| `notify`, `notify-reload` | the sender of `READY=1` on `$NOTIFY_SOCKET`, or the pid given in `MAINPID=`, **validated as a tree member**. `TimeoutStartSec` applies to the wait for `READY=1` |
| `dbus` | no bus in this environment; treat as `simple` and **warn once**, rather than the current silent `forking` treatment |
| `forking` + `PIDFile=` | after the direct child exits: poll for the file up to `TimeoutStartSec`, parse **strictly** (optional whitespace, one decimal integer, optional trailing newline — not "keep every ASCII digit found anywhere"), then **validate that the pid is in the unit's tree**. If it is not, fail the unit with `pid-file-out-of-tree`; a stale or hostile pid file must never cause init to adopt or later `SIGKILL` an unrelated process |
| `forking` without `PIDFile=` | after the direct child exits, enumerate the tree: **1 member** ⇒ that is MainPID; **>1** ⇒ MainPID is the member whose parent is the supervisor itself and whose start time is earliest; **0** ⇒ the unit exited during startup ⇒ evaluate as a failure unless `RemainAfterExit=yes` |

`sd_notify` is worth the ~150 lines it costs. It is the only mechanism in this list that
is *exact* rather than heuristic, it is what modern daemons already speak, and it also
gives `STATUS=`, `RELOADING=1`, `STOPPING=1`, `ERRNO=` and `WATCHDOG=1` for free.
Protocol: `AF_UNIX` `SOCK_DGRAM`, `$NOTIFY_SOCKET` path exported to the unit, newline-
separated `KEY=value` datagrams, sender pid taken from `SCM_CREDENTIALS` (enable
`SO_PASSCRED`) — never from the message body alone.

## 8. The termination ladder

Driven by the supervisor. Directives: `KillMode=` (`control-group` (default) | `mixed` |
`process` | `none`), `KillSignal=` (default `SIGTERM`), `RestartKillSignal=`,
`FinalKillSignal=` (default `SIGKILL`), `SendSIGHUP=` (default no), `SendSIGKILL=`
(default yes), `TimeoutStopSec=` (default 90 s).

```
stop(unit, reason):
    state = DEACTIVATING; report(state)
    deadline = now + TimeoutStopSec

    # 1. Cooperative stop
    if ExecStop is set:
        run ExecStop* sequentially, each bounded by (deadline - now)
        if all succeeded and tree is empty: goto 6

    # 2. Signal the unit
    if KillMode == none: goto 6
    targets = (KillMode == process) ? {MainPID} : enumerate_tree(supervisor)
    for t in targets:
        signal(t, KillSignal)
        signal(t, SIGCONT)              # in case it was stopped
        if SendSIGHUP: signal(t, SIGHUP)

    # 3. Cheap supplement: also signal every distinct process group in the tree.
    #    Catches shells and job-control children that a per-pid pass may race with.
    #    NOT sufficient on its own - measured in §4.3 - but free.
    for g in distinct_pgids(targets): kill(-g, KillSignal)

    # 4. Wait, RE-ENUMERATING each round so that processes forked during the ladder
    #    are also signalled. This loop is why membership must be cheap to recompute.
    while now < deadline:
        sleep(50ms)                              # or wait on pidfds with a timeout
        live = enumerate_tree(supervisor)
        if live is empty: goto 6
        for t in live not in already_signalled:
            signal(t, KillSignal); already_signalled.add(t)

    # 5. Escalate. Unblockable.
    if SendSIGKILL:
        repeat up to 10 times, 100ms apart:
            live = enumerate_tree(supervisor)
            if live is empty: break
            for t in live: signal(t, FinalKillSignal)
        if live still not empty:
            report(state = FAILED, reason = "processes remain after SIGKILL")
            # only possible for uninterruptible-sleep processes; log the pids and comms

    # 6. Finish
    run ExecStopPost*
    state = INACTIVE or FAILED; report(state)
    supervisor exits            # <-- PID 1's authoritative signal that the unit is gone
```

Three things distinguish this from the current implementation:

* it signals **all** members, not just the direct children (fixes B1/B2);
* it **re-enumerates**, so it converges even against a process that keeps forking;
* the supervisor's **own exit** is the completion signal, so PID 1 needs no polling and
  no per-unit bookkeeping to know the unit is fully gone.

`signal(t, sig)` uses `pidfd_send_signal` when the pidfd is held, otherwise
`kill(pid, sig)` guarded by a re-read of `/proc/<pid>/stat` start-time to reject a
recycled PID.

## 9. Cost analysis

| | v0.5.x (`LD_PRELOAD`) | this design (subreaper) |
|---|---|---|
| Cost imposed on unrelated processes | 2 × `access` + `socket`/`connect`/`write`/`close` **per `exec` anywhere in the container** | zero |
| PID 1 memory | one map entry per `exec`, forever | O(active units) |
| PID 1 CPU while idle | accept + parse per `exec` | zero |
| Per-unit steady state | recursive scan of the whole relation map in a polling loop | a blocked `waitid()`; no `/proc` access |
| Per-unit RSS overhead | 0 | ~1–3 MiB per active unit (one re-exec'd supervisor) |
| Stop cost | n/a (does not work) | ≤ 20 `/proc` passes over ~50–200 entries |
| Coverage | 2 of 6 measured child-creation paths | all (P1) |

The RSS cost is the one genuine regression, and it is the price of the guarantee. For a
container running 10 services that is ~10–30 MiB. If it ever matters, the mitigation is
a single "supervisor host" process running one thread per unit — but that reintroduces
shared-fate, so it is explicitly **not** in scope for v1.

Do **not** UPX-compress the binary (F10): every supervisor is a re-exec and would
decompress its own private copy of the image into anonymous memory.

## 10. Worked examples

### 10.1 `Type=simple` — nginx in the foreground

```
supervisor: subreaper on, spawn /usr/sbin/nginx -g "daemon off;" -> pid 100
            MainPID = 100, state ACTIVE
nginx forks workers 101,102  (children of 100, inside the tree, never orphaned)
worker 101 crashes           -> reaped by nginx, supervisor is not involved
systemctl stop:
   no ExecStop; KillMode=control-group
   enumerate_tree(sup) = {100,101,102}; SIGTERM each
   nginx shuts workers down; all three exit; supervisor reaps all three; exits
```

Zero `/proc` walks during steady state; one during stop.

### 10.2 `Type=forking` + `PIDFile=` — a classic daemon

```
supervisor: spawn /usr/sbin/foo            -> pid 200
pid 200: fork(300); parent exits           -> supervisor reaps 200 (status 0)
pid 300: setsid; fork(301); exits          -> reparents 301 to SUPERVISOR (P1)
                                              supervisor reaps 300
pid 301: writes /run/foo.pid = 301; runs
supervisor: direct child exited -> read PIDFile -> 301
            enumerate_tree(sup) = {301}     -> 301 IS a member -> accept
            MainPID = 301, state ACTIVE
```

Under v0.5.x this only works if `fork(3)` happened to be interposed at every level and
the `relations[1]` filter happened to match; with `daemon(3)` it observes nothing.

### 10.3 `Type=forking` with a **stale** pid file

```
PIDFile contains 1234, which is sshd started by `docker exec`
enumerate_tree(sup) = {301}   -> 1234 is NOT a member
=> unit fails with "PIDFile /run/foo.pid points to pid 1234 which is not part of this unit"
```

v0.5.x would adopt 1234, report it in `status`, and — once B1 is fixed — eventually
`SIGKILL` it. This validation step is not optional.

### 10.4 `Type=notify`

```
supervisor: NOTIFY_SOCKET=/run/docker-systemd/notify/foo.sock
            spawn /usr/sbin/foo -> pid 400
pid 400: daemonises to 402, which sends "READY=1\nMAINPID=402\nSTATUS=accepting"
supervisor: SCM_CREDENTIALS says sender pid 402; 402 is in the tree; accept
            MainPID = 402, STATUS text stored, state ACTIVE
```

Exact, no polling, no heuristics.

### 10.5 The adversarial case — a fork bomb during stop

```
stop: enumerate -> {500}; SIGTERM 500
      500 ignores SIGTERM and forks 501,502,...
      re-enumerate each 50ms; signal the new ones too
      at TimeoutStopSec: SIGKILL every member; re-enumerate; repeat up to 10x
      converges because SIGKILL cannot be blocked and each round strictly
      reduces the set of *signalled-and-still-alive* processes
```

The current implementation would exit its wait loop after `TimeoutStopSec` and SIGKILL
only the (empty) `d.cmds` set, leaving the bomb running.

## 11. What is explicitly not solved

* **Resource limits per unit** (`MemoryMax=`, `CPUQuota=`, `TasksMax=`) need cgroup
  write access. Unavailable ⇒ parsed, warned once, ignored. Document
  `docker run --memory`/`--cpus` as the alternative.
* **A process that a unit deliberately hands off to PID 1 via a helper started outside
  the unit** (e.g. `ExecStart=/usr/bin/systemd-run …`) is out of scope.
* **`SIGSTOP`ped or `D`-state processes** cannot be killed by anyone; the ladder
  reports them rather than hanging.
* **Cross-unit process theft** is impossible by construction (P1), so no defence is
  needed.
