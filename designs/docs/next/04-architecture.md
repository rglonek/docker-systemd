# 04 — Architecture

Language-agnostic component and process decomposition. Names are logical; a concrete
implementation may map them to packages, modules, or crates as idiomatic.

---

## 1. Process topology

```
                          ┌──────────────────────────────────────────┐
                          │  PID 1 — manager                         │
  docker stop ──SIGTERM──▶│                                          │
                          │  • unit registry (parse, drop-ins, deps) │
                          │  • job scheduler (ordering, transactions)│
                          │  • control server (/run/.../control.sock)│
                          │  • log broker (units → files/stderr)     │
                          │  • reaper of last resort  wait(-1)       │
                          │  • shutdown orchestrator                 │
                          └───┬──────────────┬──────────────┬────────┘
     socketpair(config+control+log fds) per unit           │
                              │              │              │
                    ┌─────────▼───┐  ┌───────▼─────┐ ┌──────▼──────┐
                    │ supervisor  │  │ supervisor  │ │ supervisor  │
                    │  nginx      │  │  cron       │ │  postfix    │
                    │ SUBREAPER   │  │ SUBREAPER   │ │ SUBREAPER   │
                    └─────┬───────┘  └──────┬──────┘ └──────┬──────┘
                          │                 │               │
                     unit processes    unit processes  unit processes

  systemctl / journalctl / service / poweroff  ──unix socket──▶ control server
```

**Invariants**

| | |
|---|---|
| I1 | PID 1 never `exec`s a unit command and never blocks on one. |
| I2 | PID 1 never touches a unit's processes except as reaper of last resort. |
| I3 | A supervisor never reads unit files; it receives a fully-resolved unit configuration over its inherited fd. |
| I4 | A supervisor's exit is the authoritative "this unit is fully stopped" event. |
| I5 | Unit state visible to clients is PID 1's replica, updated only by supervisor reports. |

I3 matters: it means unit-file parsing bugs cannot occur in N places, drop-in
resolution happens exactly once per activation, and `daemon-reload` semantics are
crisp (running jobs keep the configuration they were started with — fixes C17).

## 2. Components

| Component | Runs in | Responsibility |
|---|---|---|
| `bootstrap` | PID 1 | argv[0] dispatch, self-install of symlinks, boot-time filesystem preparation |
| `unitfile` | PID 1 | discovery, lexing, parsing, drop-in merge, specifier expansion, validation → `ResolvedUnit` |
| `registry` | PID 1 | name → unit map, aliases, templates, enable/mask state derived from the filesystem |
| `graph` | PID 1 | dependency edges, transaction building, topological ordering, cycle detection |
| `jobs` | PID 1 | start/stop/restart/reload jobs, their queueing, merging and timeouts |
| `supervisorctl` | PID 1 | spawns supervisors, owns their fds, tracks their pids, applies `Restart=` policy decisions that outlive a supervisor |
| `control` | PID 1 | listens on the control socket, authenticates peers, dispatches verbs, streams responses |
| `logbroker` | PID 1 | owns unit log sinks: reads unit log pipes, frames records, writes files, rotates, mirrors to stderr |
| `reaper` | PID 1 | `waitid(P_ALL)` loop for supervisors and for stray orphans |
| `shutdown` | PID 1 | ordered, parallel, deadline-bounded teardown |
| `supervise` | supervisor | everything in [03 §6–§8](03-process-model.md#6-the-supervisor-model) |
| `spawn` | supervisor | the pre-exec child setup of [03 §6.4](03-process-model.md#64-spawning-a-unit-process) |
| `notify` | supervisor | `sd_notify` datagram listener |
| `proctree` | supervisor | `/proc` enumeration, `ProcRef`, pidfd management |
| `cli-systemctl`, `cli-journalctl`, `cli-service` | client | argument parsing, protocol client, output formatting |
| `journal` | client | log file reading, filtering, following |

## 3. Concurrency model

Each unit is an **actor**: a single thread of control owning that unit's mutable state,
reachable only by message. This is what removes the entire A-class defect family (A9's
unsynchronised access, A7's lock inversion, A8's lock-order deadlock).

```
PID 1
  manager loop        — owns registry + graph + job queue
  one unit actor per known unit  — owns UnitState, mailbox of UnitMsg
  control server      — one task per connection, talks to actors by message
  log broker          — one task per open unit sink
  reaper              — one thread, blocking waitid, publishes ChildExit messages
```

Rules:

* No component holds a lock across a syscall that can block, or across a message send.
* The registry is replaced wholesale on `daemon-reload` (copy-on-write); readers hold a
  reference to an immutable snapshot, so a reload can never expose a half-built graph.
* Dependency edges are stored as **names**, never as pointers. Resolution happens at
  use time against the current snapshot and yields `Option<Unit>`. This makes A6's
  nil-pointer family structurally impossible.
* Every actor loop has a top-level catch/recover that transitions the unit to `failed`
  with the panic message as the failure reason, and logs it. A bug degrades one unit; it
  does not kill the container.

## 4. On-disk layout

| Path | Purpose | Mode |
|---|---|---|
| `/run/docker-systemd/` | all runtime state | `0700 root:root` |
| `/run/docker-systemd/control.sock` | control socket | `0600` |
| `/run/docker-systemd/notify/<unit>.sock` | `sd_notify` sockets | `0666`¹ |
| `/run/docker-systemd/units/<unit>.state` | last known state, for `daemon-reexec` | `0600` |
| `/run/docker-systemd/boot-id` | boot timestamp + random id, for `journalctl -b` | `0644` |
| `/run/systemd/system/` | **empty marker directory** — maintainer scripts test for it | `0755` |
| `/var/log/services/<unit>.log` | unit logs | `0640 root:adm` |
| `/var/log/services/<unit>.log.1..N` | rotated logs | `0640` |
| `/etc/systemd/system/`, `/usr/lib/systemd/system/`, `/lib/systemd/system/`, `/run/systemd/system/` | unit files (read) | — |

¹ `notify` sockets must be writable by a unit that dropped to `User=`; the socket is
per-unit and its directory is `0700`, and the sender pid is taken from
`SCM_CREDENTIALS`, so an unprivileged sender can only lie about a pid that is already
inside its own unit's tree.

Compatibility: `/tmp/docker-systemd.sock` may be maintained as a **symlink** to the real
control socket for one release, behind `--compat-tmp-socket`, defaulting to **off**.
It is a privilege-escalation vector (E1) and should not be the default.

`/etc/boot-time` moves to `/run/docker-systemd/boot-id` — writing boot state into `/etc`
pollutes committed images.

## 5. The unit registry

```
ResolvedUnit:                       # immutable; what a supervisor receives
    name            : string        # "nginx.service", canonical, with suffix
    id              : string        # invocation uuid, per activation
    fragment_path   : path | none   # none for synthesised targets
    drop_in_paths   : [path]
    load_state      : loaded | not-found | bad-setting | masked
    load_error      : string | none
    unit            : UnitSection
    service         : ServiceSection | none
    install         : InstallSection
```

* Names are canonicalised **with** the `.service` suffix internally; the CLI adds a
  missing suffix on input and may hide it on output.
* **Aliases** (a unit-directory symlink whose target is a different unit's file) create
  an alias entry in the registry pointing at the same `ResolvedUnit`, rather than a
  second half-loaded unit. This is defect A5.
* **Masked** = the highest-precedence fragment is a symlink to `/dev/null`, or the
  fragment is an empty file. Derived from the filesystem on every load, never cached in
  memory across a reload (defect B4).
* A unit that fails to parse gets `load_state = bad-setting` and a populated
  `load_error`; it stays in the registry so that `systemctl status` can explain itself.
  **It is never represented by a null configuration** (defect A4).

## 6. Unit state machine

```
                 ┌────────────┐
                 │  inactive  │◀──────────────────────┐
                 └─────┬──────┘                       │
              start    │                              │ stop complete
                       ▼                              │
                 ┌────────────┐   cond not met  ┌─────┴──────┐
                 │ activating ├────────────────▶│deactivating│
                 └─────┬──────┘                 └─────┬──────┘
        ready / exec ok │        stop / main exit     ▲     │ ExecStopPost done
                        ▼                             │     ▼
                 ┌────────────┐  stop  ───────────────┘  ┌──────┐
                 │   active   │                          │failed│
                 │  (running) │──── main exit nonzero ──▶└──┬───┘
                 │  (exited)  │                             │
                 └─────┬──────┘                             │ Restart=
                       │  reload                            ▼
                 ┌─────▼──────┐                      ┌────────────┐
                 │ reloading  │                      │auto-restart│
                 └────────────┘                      └─────┬──────┘
                                                           │ RestartSec elapsed
                                                           └──▶ activating
```

Sub-states carried alongside for `systemctl status`/`is-active` fidelity:
`active (running)`, `active (exited)` (`RemainAfterExit=yes`), `activating (start-pre)`,
`activating (start)`, `activating (start-post)`, `deactivating (stop)`,
`deactivating (stop-sigterm)`, `deactivating (stop-sigkill)`, `deactivating (stop-post)`,
`failed`, `auto-restart`, `inactive (dead)`.

`is-active` maps `active|reloading → 0`, everything else → non-zero (systemd's contract,
relied on by maintainer scripts).

## 7. Job scheduling and ordering

The current implementation has no ordering pass at all (B11). The new one:

1. **Transaction building.** A request (`start nginx`) expands into a job set by
   following requirement dependencies: `Requires=`, `Requisite=`, `Wants=`, `BindsTo=`,
   `PartOf=`, `Upholds=`, `Conflicts=`. Each becomes a `start` or `stop` job.
2. **Conflict resolution.** `Conflicts=` yields a `stop` job for the other unit;
   if the transaction contains both a `start` and a `stop` job for the same unit, it is
   rejected as inconsistent (systemd's behaviour) unless one is `Requires`-mandatory and
   the other is `Wants`-optional, in which case the optional job is dropped.
3. **Ordering.** `Before=`/`After=` induce a DAG **over the jobs in the transaction
   only**. Ordering edges to units not in the transaction are ignored (this is exactly
   systemd's rule and it is what keeps boot from serialising on absent units).
4. **Cycle detection.** Tarjan SCC over the ordering edges. On a cycle, drop the
   ordering edge with the lowest "requirement strength" in the cycle, log
   `Found ordering cycle on a.service/start; breaking at b.service`, and continue —
   matching systemd rather than refusing to boot.
5. **Execution.** Units in the same topological stratum start **in parallel**, bounded
   by a concurrency limit (default: `min(8, 2 × ncpu)`). A unit becomes eligible when
   every `After=` predecessor in the transaction has reached `active` (or
   `active (exited)`, or has failed and was only `Wants=`-linked).
6. **Timeouts.** Each job carries `TimeoutStartSec`/`TimeoutStopSec`; a job that expires
   fails the unit and releases its dependents (C11).

Semantics table (this is where the current code is loosest):

| Directive | Start-side | Stop-side | Ordering implied? |
|---|---|---|---|
| `Requires=` | start dep; if it fails, **fail this unit** | if dep stops, stop this unit | no |
| `Requisite=` | **do not start** dep; fail if dep is not already active | none | no |
| `Wants=` | start dep; ignore failure | none | no |
| `BindsTo=` | as `Requires=` | if dep stops **for any reason**, stop this unit | no |
| `PartOf=` | none | if dep stops/restarts, stop/restart this unit | no |
| `Upholds=` | start dep; **restart it whenever it stops** | none | no |
| `Conflicts=` | stop the other unit first | none | no |
| `Before=`/`After=` | none | none | **yes** |
| `OnFailure=` | start listed units when this unit enters `failed` | none | no |
| `OnSuccess=` | start listed units when this unit exits cleanly to `inactive` | none | no |

Two corrections from the current code are visible here: `Requisite=` does **not** start
its dependency (B8), and `OnFailure=`/`OnSuccess=` have **no inverse edge** (B9).

Implementation note: express the whole table as data, not as eighteen copy-pasted
loops:

```
DEPENDENCY_KINDS = [
  { name: "Requires",  inverse: "RequiredBy",  start_dep: true,  fail_on_dep_fail: true  },
  { name: "Requisite", inverse: "RequisiteOf", start_dep: false, fail_on_dep_fail: true  },
  { name: "Wants",     inverse: "WantedBy",    start_dep: true,  fail_on_dep_fail: false },
  ...
]
```

## 8. Boot sequence

```
1.  argv[0]/getpid dispatch -> manager
2.  if getpid() != 1: warn loudly (we are not the container's init; reaping will be
    incomplete) but continue
3.  prctl(PR_SET_CHILD_SUBREAPER, 1)      # belt and braces if we are not pid 1
4.  start the reaper thread
5.  backend probe (03 §5); log the chosen backend
6.  mkdir -p  /run/docker-systemd{,/notify}, /run/systemd/system,
    /etc/systemd/system, /var/log/services
    write /run/docker-systemd/boot-id
7.  self-install symlinks unless --no-install
8.  bind the control socket, chmod 0600, start serving  <-- BEFORE starting units, so
    that a unit's ExecStartPre can call systemctl (the current code already relies on
    this and it must be preserved: see test/installer.sh)
9.  install signal handlers: SIGTERM/SIGINT/SIGQUIT -> shutdown; SIGHUP -> daemon-reload;
    SIGUSR1 -> dump state; SIGCHLD via the reaper
10. load the registry
11. start the unit-directory watcher unless --no-auto-reload
12. build the boot transaction from default.target (default: multi-user.target)
13. execute it (parallel, ordered)
14. log "Startup finished in Xms (N units started, M failed)"
15. serve
```

Step 8's ordering is load-bearing: package installation inside a `docker build` runs
maintainer scripts that call `systemctl`, and those must work while units are still
coming up.

Step 11 is before the boot transaction, not after it, because a `docker exec apt
install` can land while slow units are still coming up. The transaction is planned
from an immutable registry snapshot, so a reload underneath it is no different from
an operator running `daemon-reload` mid-boot.

### 8.1 Automatic reload

The unit search path and its `.wants`/`.requires`/`.d` subdirectories are watched with
`inotify` (`IN_CREATE|IN_DELETE|IN_MOVED_TO|IN_MOVED_FROM|IN_CLOSE_WRITE` — not
`IN_MODIFY`, which fires per `write()`). Any event triggers the same reload as
`systemctl daemon-reload` once the directories have been quiet for 400 ms, bounded at
5 s, so one `apt install` that drops a dozen unit files causes one reload.

This is a deliberate departure from systemd, which reloads only when asked. The
`daemon-reload` in a postinst is emitted only by `dh_installsystemd`/`%systemd_post`
packaging, runs only when that script decided systemd was running, and never covers a
unit file that arrived by `docker cp`, a Dockerfile `COPY`, `apk add`, or
`rpm -i --noscripts`. The failure mode — a service the operator cannot see at all — is
worse in a container than the cost of a periodic registry rebuild.

A directory that does not exist yet cannot be watched, so its parent stands in until it
appears; one level only, because the level above that is `/etc` and `/usr`. Watches are
re-established after every reload, which is what picks up a `multi-user.target.wants/`
directory created by a first install. `inotify_add_watch` is idempotent, so this needs
no watch-descriptor bookkeeping and self-heals a directory that was deleted and
recreated.

Nothing is started, stopped or enabled as a side effect: the reload makes a unit
visible, not active. `--no-auto-reload` restores the strict systemd behaviour.

## 9. Shutdown

Triggered by `SIGTERM`/`SIGINT`/`SIGQUIT` from `docker stop`, or by
`systemctl poweroff|halt|reboot`, or by a unit's `FailureAction=poweroff`.

```
shutdown(grace):
    idempotent: a second signal is ignored except that a THIRD signal forces
                immediate SIGKILL of everything (operator escape hatch)
    stop accepting new control connections; answer in-flight ones with "shutting down"
    build the stop transaction: every active unit
    order it REVERSE-topologically (stop dependents before their dependencies)
    budget = grace (default: min(TimeoutStopSec sum, DefaultTimeoutStopSec=90s),
                    overridable with --shutdown-timeout, and clamped to
                    $DOCKER_STOP_TIMEOUT if we can infer it)
    for each stratum, in reverse order:
        stop all units in the stratum in PARALLEL
        each unit gets min(its TimeoutStopSec, remaining budget / strata_remaining)
    after all supervisors have exited (or the budget expired):
        SIGTERM every remaining process in the container except self
        wait 2s
        SIGKILL every remaining process except self
        final reap loop, bounded at 5s
    exit(0)   (or 1 if any unit failed to stop)
```

The current implementation stops units **serially in map order** with a 5 s timeout each
and no global budget (C14) — three slow units and Docker's default 10 s grace is blown,
so the container is `SIGKILL`ed mid-shutdown and data-writing services lose their
graceful stop. Parallelism within a stratum plus a global budget is the fix.

Document prominently that `docker stop -t <n>` should be set to at least the sum of the
critical units' `TimeoutStopSec`.

## 10. Reboot / `daemon-reexec`

There is no kernel to reboot to. Define:

| Verb | Behaviour |
|---|---|
| `poweroff`, `halt` | full shutdown, exit 0 → container stops |
| `reboot` | full shutdown, exit **with a distinguishable code** (default 0) → container stops; with `--restart=always` Docker restarts it, which is the closest honest analogue. Document it |
| `daemon-reload` | reload unit files; running jobs keep their pinned configuration |
| `daemon-reexec` | serialise state to `/run/docker-systemd/units/*.state`, `execve` self, reattach to running supervisors via their inherited fds |

`daemon-reexec` requires the supervisor control fds to survive the re-exec: keep them
non-`CLOEXEC` and pass their numbers in the serialised state. Maintainer scripts call
`daemon-reexec` on upgrade; implementing it as "reload + keep going" is acceptable for
v1 provided it does not restart units.

## 11. Directory of behaviour flags

| Flag | Default | Effect |
|---|---|---|
| `--log-to-stderr` | off | mirror unit logs to init's stderr (`docker logs`) |
| `--no-logfile` | off | do not write `/var/log/services/*.log` (disables `journalctl`) |
| `--log-level=` | `info` | `error,warn,info,debug,trace` for the manager's own log |
| `--default-target=` | `multi-user.target` | boot target |
| `--no-install` | off | do not symlink over the distro's `init`/`systemctl`/… |
| `--no-auto-reload` | off | do not reload unit files when the unit directories change (8.1) |
| `--shutdown-timeout=` | `90s` | global shutdown budget |
| `--backend=` | `auto` | `auto,cgroup2,subreaper,degraded` — force for testing |
| `--supervisor-heartbeat=` | `1s` | tree re-scan interval while a unit has escaped members |
| `--compat-tmp-socket` | off | also create the legacy world-accessible `/tmp` socket |
| `--no-pidtrack` | *removed* | accepted and ignored with a deprecation warning for one release |
| `--debug-reaper` | folded into `--log-level=trace` | |
