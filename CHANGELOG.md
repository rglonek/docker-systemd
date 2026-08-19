# CHANGELOG

## Unreleased

### Changes

* **Unit files are reloaded automatically when they change on disk.**
  `apt install mysql-server`, `dnf install httpd`, `apk add postfix` or a
  `docker cp` of a hand-written unit now makes the unit visible without anyone
  running `systemctl daemon-reload` first. The manager watches the unit
  directories and their `.wants`/`.requires`/`.d` subdirectories with
  `inotify`, coalescing a burst of changes into a single reload. Relying on the
  package's postinst to call `daemon-reload` covers only packaging built with
  `dh_installsystemd`/`%systemd_post`, only when that script decided systemd
  was running, and never a unit file that arrived any other way. The reload is
  the one `daemon-reload` performs — running units keep the configuration they
  were started with, and nothing is started, stopped or enabled as a side
  effect. `--no-auto-reload` restores the strict systemd behaviour.
* `/etc/systemd/system` is created at boot if it is missing, as systemd ships
  it; `systemctl enable` previously created it on first use.

### Fixes

* **`daemon-reload` racing with a unit lookup.** The registry pointer was
  swapped under a lock but the loader that resolves template instances against
  it was not, so a reload concurrent with a `systemctl` query was a data race
  and could pair a new snapshot with the previous generation's loader. Both are
  now published together, and a reload whose load fails keeps the previous
  snapshot instead of replacing the loader out from under it.
* The conformance runner still asserted that `/run/docker-systemd` is `0700`,
  which the `Type=notify`/`User=` fix above changed to `0711`; it failed on
  every image. The README said `0700` for the same reason.

* **A `Type=notify` unit that drops to `User=` can now report readiness.**
  `/run/docker-systemd` and its `notify/` subdirectory were `0700`, so a unit
  running as a non-root user was denied the search permission it needed to
  reach its own `$NOTIFY_SOCKET`; `READY=1` never arrived and the unit sat in
  `activating` until `TimeoutStartSec`. Both directories are now `0711` —
  searchable but still not listable — and the per-unit socket is chowned to the
  unit's resolved credentials. `mysql.service` was the visible casualty:
  `User=mysql` plus `TimeoutSec=infinity` meant `systemctl start mysql` hung
  forever while `mysqld` logged that it was ready for connections.
* **`systemctl stop` works on a unit that is still activating.** Nothing drained
  the supervisor's control queue during activation, so a stop request waited out
  the whole start timeout — indefinitely for a unit with
  `TimeoutStartSec=infinity`. A stop that arrives mid-activation now unwinds the
  activation and runs the termination ladder immediately.
* **`ExecStop=`/`ExecStopPost=` are no longer killed on sight during shutdown.**
  They ran through the same cancel-on-quit path as the startup commands, so on
  container shutdown they were SIGTERMed the moment they started.
* A state-changing sd_notify assignment from a live process outside the unit's
  tree is now discarded rather than acted on.
* `Restart=` on a `Type=notify` unit no longer leaks the old notify listener or
  loses every notification sent after the first restart.

## v1.0.0

Clean-room reimplementation against the design in `designs/docs/next/`. The
whole `LD_PRELOAD` subsystem, the four committed `.so` blobs and the old
`systemd/`, `procwait/`, `journalctl/`, `systemctl/` and `common/` packages are
deleted.

### Process tracking

* **Replace `LD_PRELOAD` fork interposition with per-unit subreaper
  supervisors.** Each unit runs under a dedicated process that sets
  `PR_SET_CHILD_SUBREAPER=1`, so orphaned descendants reparent to it rather
  than to PID 1 and unit membership becomes exact. Measured, the old shim saw
  2 of 6 child-creation paths and missed `daemon(3)` — the case it existed to
  solve — entirely.
* **`systemctl stop` can now stop a `Type=forking` service.** The termination
  ladder signals every member of the unit's live tree individually,
  re-enumerating each round, so it converges even against a process that forks
  on every signal. Process-group kills are used only as a cheap supplement:
  measured, `kill(-pgid)` does not stop a daemonised service.
* `pidfd_open`/`pidfd_send_signal` are used where available, eliminating
  PID-reuse mis-targeting.
* cgroup v2 is auto-detected and used as an accelerator when a writable
  hierarchy is available; it is never required.
* Orphan recovery: if a supervisor dies, its processes are found again by an
  invocation-id environment marker and handed to a replacement.
* `--no-pidtrack` is accepted and ignored with a deprecation warning.

### Correctness

* **Unit commands no longer run through `/bin/bash -c`.** systemd's own command
  lexer is implemented: prefixes `-`, `@`, `:`, `+`, `!`, `!!`; quoting;
  C-style escapes; `$VAR`/`${VAR}` expansion with systemd's word-splitting
  rule; no globs, pipes, redirection or command substitution.
* Full boolean grammar: `RemainAfterExit=yes` now parses as **true**.
* `Type=notify` is implemented with a real `$NOTIFY_SOCKET` and
  `SCM_CREDENTIALS`, rather than being silently treated as `forking`.
* `Before=`/`After=` are honoured: transactions are topologically ordered,
  units in a stratum start in parallel, and an ordering cycle is broken with a
  warning rather than hanging the boot.
* `Requisite=` no longer starts its dependency; `OnFailure=`/`OnSuccess=` no
  longer create a bogus inverse edge; `StopWhenUnneeded=` works.
* `Type=oneshot` runs its `ExecStart=` lines sequentially and no longer forces
  `RemainAfterExit`. Multiple `ExecStart=` lines for other types are a parse
  error rather than being started in parallel.
* `PIDFile=` is parsed strictly and the pid is validated as a member of the
  unit's tree; a stale or hostile pid file fails the unit instead of making
  init adopt — and later kill — an unrelated process.
* `TimeoutStartSec=` is implemented and `TimeoutStopSec=` defaults to 90 s;
  a hanging `ExecStartPre=` fails its own unit instead of the whole boot.
* `StartLimitIntervalSec=`/`StartLimitBurst=` stop a crash-looping unit from
  restarting for ever; `RestartSec=` honours values below one second.
* `EnvironmentFile=` is parsed properly (comments, blanks, `export `, quotes,
  continuations, optional `-` prefix); `Environment=` accepts several
  space-separated assignments per line.
* Full specifier table (`%n %N %p %P %i %I %f %j %t %S %C %L %E %h %u %U %g %G
  %H %m %b %v %a %%`), expanded in every directive systemd expands them in.
* `User=`/`Group=` are applied whenever either is set, supplementary groups are
  set, and `$HOME`/`$USER`/`$LOGNAME`/`$SHELL` are exported. An unresolvable
  `User=` fails the unit with `217/USER` rather than falling back to root.
* `Limit*=` directives now work, clamped to the container's inherited hard
  limits with a diagnostic naming the `docker run --ulimit` that would fix it.
* New: `UMask=`, `Nice=`, `OOMScoreAdjust=`, `NoNewPrivileges=`,
  `WorkingDirectory=`, `RuntimeDirectory=` and its `State`/`Cache`/`Logs`/
  `Configuration` siblings, `StandardInput=`/`StandardOutput=`/
  `StandardError=`, `SyslogIdentifier=`, the `Condition*=`/`Assert*=` families,
  `KillMode=`/`KillSignal=`/`FinalKillSignal=`/`SendSIGHUP=`/`SendSIGKILL=`.
* `.target` units are first-class, with well-known targets synthesised on
  demand so `After=network.target` resolves.
* Templates are resolved from their template file, never materialised;
  `delete-instance` removes only the enable symlink.
* `set-environment` and `unset-environment` are no longer swapped.
* `mask` creates the right symlink instead of a file called `target` in init's
  working directory; mask state is derived from the filesystem on every load.
* `enable` writes real symlinks into the `[Install]`-named target's `.wants`
  or `.requires` directory, honouring `Alias=` and `Also=`.
* Unit discovery de-duplicates directories by `realpath`, scans
  `/run/systemd/system`, and applies drop-ins from all four search-path
  directories plus the type-wide and template directories.

### Reliability

* PID 1 is a thin dispatcher: it never execs a unit command and never blocks on
  one. Per-unit state is owned by one supervisor, dependency edges are stored
  as names and resolved at use time, and every unit actor has a top-level
  recover — the nine container-fatal crash and hang defects are removed by
  construction.
* Shutdown is reverse-topological and parallel within a stratum, under a global
  budget, with a three-signal operator escape hatch and a final sweep and reap.
* `daemon-reload` no longer rebuilds the definitions of running units: a
  running job keeps the configuration it was started with.

### Protocol, CLI and logging

* New length-prefixed, versioned, typed control protocol: no single-`read()`
  assumption, no `0x00` sentinel, no 64 KiB cap, and a `HELLO` handshake that
  diagnoses a stale client instead of misparsing.
* `systemctl` gains `try-restart`, `reload-or-restart`, `is-failed`,
  `is-enabled`, `list-unit-files`, `list-dependencies`, `cat`, `preset`,
  `preset-all`, `link`, `revert`, `add-wants`, `add-requires`, `kill`,
  `isolate`, `reset-failed`, `daemon-reexec`, `show -p X --value`, and the
  systemd exit-code table. An unknown verb exits 2 rather than 0.
* `journalctl` is native: `-u` is optional, `-n` tails the file (it used to
  tail init's stdin), `-f` uses inotify, `-S`/`-U`/`-b` work independently of
  each other, plus `-p`, `-o`, `-g`, `-r`, `--disk-usage` and `--vacuum-*`.
* Log records carry an RFC3339-microsecond UTC timestamp, a syslog priority and
  the unit and pid, so time filters work at all; legacy un-timestamped lines
  are still readable. Logs rotate by size with a total cap.
* New `systemd-notify` and `systemd-detect-virt` helpers, and
  `/run/systemd/system` is created so distro maintainer scripts stop skipping
  unit registration.

### Security

* Runtime state moved to `/run/docker-systemd/` (`0700 root:root`) with the
  control socket at `0600` and `SO_PEERCRED` authentication: only uid 0 may
  control the manager. The old `/tmp` socket let any uid in the container start
  a root unit or power the container off.
* The world-writable pidtrack socket, `/var/log/pidtrack.log` and
  `/etc/ld.so.preload` manipulation are gone with the shim.
* Unit log files are `0640 root:adm`.

### Build and test

* First automated tests: unit tests with the race detector, four fuzz targets,
  a process-level suite that runs real daemonising services, and a conformance
  matrix across ten base images including `alpine:3`.
* The toolchain is pinned in `go.mod`; no UPX; no committed binary artefacts;
  every release artefact is reproducible from a clean checkout by CI.

## v0.5.0
* add timeout handling and `wpid==0` handling to `procwait` in `FinalReap`
* add `isShuttingDown` method to `daemon` to check if the system is shutting down to prevent starting new services during shutdown

## v0.4.5
* add `dockerbuild.sh` script to build `LD_PRELOAD` libraries for different architectures
* build against older supported versions of `glibc`

## v0.4.2
* add error handling to `LD_PRELOAD` libraries
* fix `arm64` versions of `LD_PRELOAD` libraries

## v0.4.1
* daemons should also inherit `os.Environ()` of systemd process
* support `set-environment` and `unset-environment` features of systemd/systemctl
* disable journalctl pager; feature will be ignored; to page, simply pipe to `more` or `less`

## v0.4.0
* redo the `systemd` daemon handler, using signals and states instead of long-lasting mutex locks to allow for better state flow and querying capability
* add `version` systemctl command to print version

## v0.3.3
* bump dependencies
* automated docker build system

## v0.3.2
* improve `syscall.Wait4` call error handling in `procwait`
* improve communication protocol between `systemd.command` and `systemctl` so that multiple messages can be sent and printed to user during the command execution process

## v0.3.1
* add `--now` option to `systemctl enable`

## v0.3.0
* big change - not using double-process-init method for controlling and reaping processes; instead using a single init with a single dispatching `syscall.Wait4(-1,...)`
  * this allows for a more streamlined approach, single-pid init process, as well as improved tracking of other PIDs
  * more importantly, the forking-process wait system now can pause and wait on a mutex instead of timed polling, making this much more efficient

## v0.2.1
* support tracking processes that fork-detach themselves late (corner-case)
* running enable/start on a multi-instance will create that instance automatically

## v0.2.0
* first runnable release which properly tracks pids and reaps zombie processes
* added `ld_preload` feature for fork process tracking and multiple bugfixes
