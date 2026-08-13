# 01 — Review of the current system (v0.5.0)

This is a description of what the code in this repository does today, written so that a
reimplementation can be done clean-room from the specification documents without
reading the old source.

---

## 1. Shape of the program

A single Go binary, built statically (`CGO_ENABLED=0`) for `linux/amd64` and
`linux/arm64`, optionally UPX-compressed. It multiplexes on `argv[0]`:

| invoked as | behaviour | source |
|---|---|---|
| anything, **when `getpid() == 1`** | init | `main.go:26` |
| `systemd`, `init` | init | `main.go:50` |
| `systemctl` | control client | `systemctl/main.go` |
| `journalctl` | log reader | `journalctl/main.go` |
| `service` | swaps `argv[1]`/`argv[2]`, then `systemctl` | `main.go:42` |
| `poweroff`, `shutdown` | `systemctl poweroff` | `main.go:39` |

On init startup it **symlinks itself over the distro's binaries** in the first of
`/usr/local/sbin`, `/usr/local/bin`, `/usr/bin`, `/bin`, `/usr/sbin`, `/sbin` found in
`$PATH` (default `/usr/sbin`), for the seven names above, renaming anything already
present to `<name>.old` (`main.go:59-100`).

## 2. Runtime topology

```
PID 1  init
 ├── control socket   /tmp/docker-systemd.sock      (systemctl clients)
 ├── pidtrack socket  /tmp/docker-systemd-pidtrack.sock  (LD_PRELOAD shim reports)
 ├── reaper goroutine: wait4(-1, ...) in a loop, dispatches statuses to waiters
 ├── goroutine per unit: monitorCmds()  — waits on the unit's processes
 └── children: /bin/bash -c "<ExecStart line>"  (one per ExecStart= line)
```

Everything is one process. There is no per-unit supervisor. Unit state lives in a
`map[string]*daemon` guarded by an `RWMutex`, with a second `RWMutex` per daemon.

## 3. How PID tracking works today

This is the mechanism the rebuild replaces, so it is described precisely.

1. At boot, init writes an embedded shared object to `/usr/local/lib/fork.so` and
   appends that path to `/etc/ld.so.preload` (`systemd/main.go:34-44,130`). Every
   dynamically linked glibc process started in the container from then on loads it.
2. The shim (`forkpreload/preload.c`) interposes two symbols:
   * `fork()` — calls the real `fork`, and *in the parent* reports the triple
     `ppid:pid:childpid`.
   * `execve()` — reports the pair `ppid:pid` **before** exec'ing.
   Reports go to `/tmp/docker-systemd-pidtrack.sock` (and, if the file happens to
   exist, are also appended to `/var/log/pidtrack.log`). Both destinations are probed
   with `access(2)` on every single call.
3. init accepts one connection per report, parses `a:b[:c]`, and records edges in
   `systemd/pidtracker`: `relations[a] += b` and, when present, `relations[b] += c`.
   Entries are **never removed**.
4. `pidtracker.Find(pid)` recursively collects descendants of `pid` from that map, then
   **filters the result down to pids that also appear in `relations[1]`** — i.e. pids
   that were observed to have `getppid() == 1` at some report. That filter is the
   "child that frees itself to PID 1" detection: a descendant of the unit that has been
   reparented to init is the daemonised survivor init must wait on.
5. `daemon.monitorCmds` (`systemd/daemons/daemon.go:178`) then, for
   `Type in {forking, dbus, notify, notify-reload}`:
   * if `PIDFile=` is set, read it and wait on that pid;
   * otherwise scan `/proc` for processes with `PPid == 1` whose `/proc/<pid>/environ`
     contains `SYSTEMD_SERVICE_NAME=<unit>` (init injects that variable into every
     `ExecStart` child), and wait on those;
   * then loop calling `pidtracker.Find()` on the original child pids and wait on any
     live results, repeating until a pass finds nothing.

### 3.1 Assessment

The approach is inventive and the `relations[1]` filter is a genuinely clever way to
identify escapees. It is nonetheless the wrong foundation:

* **It does not observe most daemonisation.** Measured coverage of the current shim
  (see [README §4](README.md#4-the-evidence-that-drove-d1)): `fork(3)` ✅,
  `system(3)` ✅, but `vfork` ❌, `posix_spawn` ❌, **`daemon(3)` ❌**, static
  binaries ❌. glibc's internal callers reach `fork` through the hidden `__fork` alias
  and never traverse the PLT, so interposition cannot see them.
* **It is timing-dependent even when it does fire.** A pid only lands in
  `relations[1]` if it happens to call `fork` or `execve` *after* its parent died. A
  double-forking daemon that execs before the intermediate exits is recorded under the
  intermediate and then filtered *out* by the `relations[1]` intersection.
* **It never forgets.** `relations` grows for the lifetime of the container — one entry
  per `exec` anywhere in the container, forever. `Find()` is a recursive scan of the
  whole map, called in a busy loop.
* **PIDs are recycled.** Stale edges make init attribute an unrelated new process to a
  unit.
* **It taxes and endangers every process in the container.** Two `access(2)` calls plus
  a `socket`/`connect`/`write`/`close` round-trip to PID 1 on every `exec`; a bug in
  the `.so`, or a dangling `/etc/ld.so.preload` entry in a committed image, breaks
  every dynamically linked binary in the image.
* **And the tracked pids are never used to stop anything** — see §5.

## 4. Unit handling

* **Discovery** (`common.GetSystemdPaths`): `/etc/systemd/system`,
  `/usr/lib/systemd/system`, `/lib/systemd/system`, with a symlink dance to avoid
  double-loading on usrmerge systems, plus per-unit drop-in directories
  `<unit>.service.d/*.conf`. Only `*.service`.
* **Parsing** (`systemd/daemons/unit.go`): a line scanner handling `[Unit]`,
  `[Service]`, `[Install]`, `#`/`;` comments, trailing-`\` continuation, and
  `KEY=value` with optional surrounding quotes stripped. Keys are upper-cased and
  matched exactly. `%i`/`%I` are substituted in `Exec*=` lines for `name@instance`
  units.
* **Dependency wiring** (`daemons.Reload`): after load, each named dependency string is
  resolved to a `*daemon` pointer, and the inverse edge is written into the target.
* **Enablement**: `systemctl enable` writes a *regular file* containing `OK` at
  `/etc/systemd/system/multi-user.target.wants/<unit>.service`. Boot starts everything
  in that directory. Package-installed symlinks work too, since only presence is
  checked.
* **Templates**: `foo@bar.service` is *materialised* — `CreateInstance` hard-links the
  template file to a concrete `foo@bar.service` path, and `%i` is expanded at load.
  Instances persist until `delete-instance`.

## 5. Start / stop / restart

`start()` (`daemon.go:414`) runs, in order: masked check → rlimit warnings → `stop()` →
env assembly (`os.Environ()` + `Environment=` + `EnvironmentFile=`) → `User=`/`Group=`
resolution → `ExecCondition=` → `Requisite=` check → start `Requires=`, `BindsTo=`,
`Wants=`, `Upholds=` → stop `Conflicts=` → `ExecStartPre=` → `ExecStart=` →
`ExecStartPost=` → state `Running` → spawn `monitorCmds` goroutine.

Every command is executed as `/bin/bash -c "<line>"`, with stdout/stderr wired to the
unit's log writer.

`stop()` runs `ExecStopPre=` → `ExecStop=` → **SIGTERM to the direct `ExecStart`
children only** → `ExecStopPost=` → poll up to `TimeoutStopSec` (default 5 s) → SIGKILL
the same direct children.

> **The tracked forked pids (`d.pids`) are never signalled.** They are collected by all
> of the machinery in §3 and then used only to print PIDs in `systemctl status`
> (`daemon.go:1146`). For a `Type=forking` unit the direct child has already exited by
> the time the unit is "running", so `d.cmds` is empty and `systemctl stop` signals
> **nothing**. Such a unit can only be stopped if it declares `ExecStop=`.

`Restart=` is handled in `monitorCmds` after all waits return, mapping
`always` / `on-failure`-family / `on-success` / everything-else, with a `RestartSec`
sleep (forced to a 1 s minimum) and a shutdown-in-progress re-check.

## 6. Logging and journal

Each unit's stdout+stderr is appended verbatim to `/var/log/services/<unit>.log`
(`--no-logfile` disables; `--log-to-stderr` additionally mirrors to init's stderr with a
`<unit>` prefix and a timestamp). `journalctl` reads that file directly: `-f` and `-n`
shell out to `tail`; `--since`/`--until` parse the first 19 bytes of each line as a
timestamp.

Note that **no timestamp is ever written to the log file** — only the stderr mirror is
prefixed — so the time filters can never match anything real.

## 7. Control protocol

`systemctl` connects to `/tmp/docker-systemd.sock` and writes
`u16 argc, {u16 len, bytes}*`. init parses it in a single `read()` into a 64 KiB
buffer, dispatches through `go-flags`, streams human output back, terminates the stream
with a `0x00` byte, waits for a `0x00` ack, then sends a 5-byte magic followed by a
`u16` exit code.

## 8. Feature support matrix

### 8.1 Unit types

| Type | Status |
|---|---|
| `.service` | supported |
| `.target` | **not supported** — `multi-user.target` is emulated by the `.wants` directory only; `Wants=network.target` resolves to a nil dependency |
| `.socket`, `.timer`, `.path`, `.mount`, `.automount`, `.swap`, `.slice`, `.scope`, `.device` | **not supported** |

### 8.2 `[Unit]` directives

| Directive | Status |
|---|---|
| `Description=` | supported (display only) |
| `Wants=`, `Requires=`, `Requisite=`, `BindsTo=`, `PartOf=`, `Upholds=`, `Conflicts=`, and their inverses | parsed and wired; start-side semantics approximated. `Requisite=` is **mis-wired into `Requires=`** so it starts the dependency instead of only checking it (`daemons.go:300`) |
| `Before=`, `After=` | **parsed and stored, but never used** — there is no ordering pass at all. README lists these as "Planned" |
| `OnFailure=`, `OnSuccess=` | supported, but `Reload()` cross-wires `OnFailure`↔`OnSuccess` inverses |
| `StopWhenUnneeded=` | present but the "is anyone still needing me" check is a no-op (`daemon.go:140-169`), so it degenerates to "stop after 1 s" |
| `FailureAction=`, `SuccessAction=` | only `poweroff*` prefixes act; everything else ignored |
| `Condition*=`, `Assert*=` | **not supported** |
| `DefaultDependencies=`, `RefuseManualStart/Stop=`, `JobTimeout*=` | **not supported** |

### 8.3 `[Service]` directives

| Directive | Status |
|---|---|
| `Type=simple` | supported |
| `Type=oneshot` | partial — **forces `RemainAfterExit=true`** (`daemon.go:499`), which systemd does not; multiple `ExecStart=` lines are run in parallel rather than sequentially |
| `Type=forking` | partial — waits correctly-ish, cannot be stopped (§5) |
| `Type=exec` | falls through to `simple` (readiness not distinguished) |
| `Type=notify`, `notify-reload` | **treated as `forking`**; `$NOTIFY_SOCKET` is never set, `sd_notify` is not implemented, `READY=1` is never awaited |
| `Type=dbus` | treated as `forking`; `BusName=` ignored |
| `Type=idle` | falls through to `simple` |
| `ExecStart=`, `ExecStop=`, `ExecStartPre=`, `ExecStartPost=`, `ExecStopPost=`, `ExecCondition=`, `ExecReload=` | supported, run via `bash -c`. Only the `-` prefix is honoured; `@`, `+`, `!`, `!!`, `:` are not |
| `ExecStopPre=` | supported — **but this directive does not exist in systemd** |
| `RemainAfterExit=` | supported (`true` only; `yes`/`1`/`on` are **not** accepted) |
| `PIDFile=` | supported, unvalidated (a stale or hostile pid file makes init wait on / report an arbitrary process) |
| `Restart=` | `always`, `on-failure`/`on-abnormal`/`on-watchdog`/`on-abort`, `on-success`, else `no`. No `StartLimitBurst`/`StartLimitIntervalSec` rate limiting |
| `RestartSec=` | supported, but coerced to a 1 s minimum (systemd default is 100 ms) |
| `TimeoutSec=`, `TimeoutStopSec=` | supported (stop only; default 5 s vs systemd's 90 s) |
| `TimeoutStartSec=` | **not supported** — a hanging `ExecStartPre` hangs the unit forever |
| `WorkingDirectory=` | supported for `ExecStart`/`ExecStop` only |
| `User=`, `Group=` | partial — no supplementary groups, no `$HOME`/`$USER`/`$LOGNAME`, and **ignored entirely when the resolved uid is 0**, so `User=root` + `Group=x` does nothing |
| `Environment=` | supported, one `KEY=value` per line only (systemd allows several space-separated, with quoting) |
| `EnvironmentFile=` | partial — split on `\n` with **no comment stripping, no blank-line stripping, no quote removal**; `-` optional-prefix honoured |
| `Limit*=` (16 directives) | parsed, **warned about, and ignored**. Several of them (`LimitNOFILE`, `LimitCORE`, `LimitSTACK`, …) are in fact settable by an unprivileged process when lowering, or up to the hard limit |
| `KillMode=`, `KillSignal=`, `SendSIGKILL=`, `SendSIGHUP=`, `FinalKillSignal=` | **not supported** |
| `StandardOutput=`, `StandardError=`, `StandardInput=`, `SyslogIdentifier=` | **not supported** (always: inherit stdin, log to file/stderr) |
| `UMask=`, `Nice=`, `OOMScoreAdjust=`, `IOSchedulingClass=`, `CPUAffinity=` | **not supported** (`Nice`/`OOMScoreAdjust`/`UMask` are all achievable unprivileged) |
| `RuntimeDirectory=`, `StateDirectory=`, `CacheDirectory=`, `LogsDirectory=`, `ConfigurationDirectory=` | **not supported** — a common cause of real units failing |
| `PrivateTmp=`, `ProtectSystem=`, `NoNewPrivileges=`, `CapabilityBoundingSet=`, `ReadOnlyPaths=`, … | **not supported** (most need privileges we do not have; `NoNewPrivileges` does not) |
| `WatchdogSec=` | **not supported** |
| `Slice=`, `MemoryMax=`, `CPUQuota=`, `TasksMax=` | **not supported** (need cgroup write access) |

### 8.4 `[Install]` directives

| Directive | Status |
|---|---|
| `WantedBy=` | parsed, but `enable` **ignores it** and always links into `multi-user.target.wants` |
| `RequiredBy=`, `UpheldBy=` | parsed, unused at enable time |
| `Alias=`, `Also=`, `DefaultInstance=` | **not supported** |

### 8.5 Specifiers

`%i` and `%I` in `Exec*=` only. Not supported anywhere else, and
`%n %N %p %P %f %t %h %U %u %m %b %H %v %%` are not implemented at all — `%n` and `%N`
in particular appear in many stock units.

### 8.6 `systemctl` verbs

Supported: `start`, `stop`, `restart`, `reload`, `status`, `enable [--now]`, `disable`,
`mask`, `unmask`, `show`, `list`, `daemon-reload`, `poweroff`, `create-instance`,
`delete-instance`, `set-environment`, `unset-environment`, `version`.

Absent, and **required by distro maintainer scripts** (`deb-systemd-invoke`,
`deb-systemd-helper`, RPM `%systemd_post` macros): `is-active`, `is-enabled`,
`is-failed`, `list-units`, `list-unit-files`, `show -p <prop> --value`,
`daemon-reexec`, `preset`, `try-restart`, `reload-or-restart`, `cat`, `edit`,
`--quiet`, `--no-pager`, `--no-block`, `--system`, `--version`.

Also absent: `/run/systemd/system` is never created, so every `[ -d /run/systemd/system ]`
probe in a postinst script concludes systemd is *not* running and skips unit
registration entirely.

### 8.7 `journalctl`

Supported: `-u` (**mandatory**), `-n`, `-f`, `-S`, `-U`, `-b`, `--no-pager`.
`-n` is broken (tails stdin, not the file); `-S`/`-U`/`-b` are broken (filter only
applies when *both* `-S` and `-U` are given, and the log has no timestamps to match).
Absent: no `-u` (all units), `-p`, `-o`, `-k`, `--since=yesterday`-style relative times,
`-x`, `--disk-usage`, `--vacuum-*`.

## 9. Operational characteristics

* No log rotation — `/var/log/services/*.log` grows unbounded.
* No unit-start rate limiting — a crash-looping unit restarts every second forever.
* Shutdown iterates units in Go **map order**: no reverse dependency order, no
  parallelism, and a 5 s worst case per unit. With more than two slow units this
  exceeds Docker's default 10 s `stop` grace and the container is `SIGKILL`ed
  mid-shutdown.
* No `Before=`/`After=` ordering at boot either — units start in `ReadDir` order of
  `multi-user.target.wants`, serially, each blocking the next, while holding a read
  lock that stalls every incoming `systemctl` command.
* Build artefacts: four `.so` binaries committed to git, built by a manual
  `docker`-dependent script (`forkpreload/dockerbuild.sh`), embedded via `go:embed`.
  They are not reproducible from CI and the two architectures were evidently built with
  different flags (`fakefork_amd64.so` 7,896 B from an *empty* source file vs
  `fakefork_arm64.so` 70,120 B).
* Zero automated tests. `test/` is a manual demo that downloads Aerospike.
