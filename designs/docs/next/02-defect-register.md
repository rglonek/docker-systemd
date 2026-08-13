# 02 — Defect register

Every defect found during the review of v0.5.0, with the disposition in the new design.
Line references are to the tree at commit `550d55b`.

Severity key:
**S1** container-fatal (kills or hangs PID 1) ·
**S2** functional failure of a documented feature ·
**S3** incorrect behaviour / compatibility gap ·
**S4** hygiene, cost, or latent risk

---

## A. Crashes and hangs in PID 1 (S1)

A panic anywhere in the init process terminates PID 1, which terminates the container.
There is no `recover()` anywhere in the codebase.

| ID | Defect | Location | Trigger |
|---|---|---|---|
| **A1** | `cmdpids = append(cmdpids, cmd.Process.Pid)` sits **outside** the `if cmd.Process != nil` guard immediately above it. A command whose `Start()` failed is still appended to `d.cmds` when the line carried the `-` (ignore-failure) prefix, so `Process` is nil. | `systemd/daemons/daemon.go:183-188`, appended at `:773` | `ExecStart=-/nonexistent/binary` |
| **A2** | Same nil dereference in the stop path: `procwait.Is(cmd.Process.Pid)` with no guard, twenty lines above a guarded use of the same field. | `daemon.go:921` (guarded at `:938`) | as A1, then `systemctl stop` |
| **A3** | Same nil dereference in `Status()`. | `daemon.go:1144` | as A1, then `systemctl status` |
| **A4** | `Reload()` deletes units whose `def` failed to load **only if** they are `StateStopped`; the dependency-wiring loop that follows then dereferences `d.def.Requires` unconditionally. | `daemons.go:256-263` vs `:263` | `daemon-reload` after deleting or breaking the unit file of a **running** service |
| **A5** | The global `processedFiles` de-duplication is keyed on the *resolved* path across all units, so when two unit names resolve to the same file (a systemd **alias** symlink, e.g. `/etc/systemd/system/foo.service → /lib/systemd/system/bar.service`) the second unit is registered with `def == nil` → A4's panic. | `daemons.go:154,229` | any aliased unit |
| **A6** | Dependency maps are populated with `nil` values before the existence check: `d.def.Requires[depName] = ds.list[depName]` runs, *then* `if _, ok := ...; !ok { continue }`. Callers that lack a nil guard then panic. `start()`/`Stop()` guard; `i.State()` in the `Requisite` loop and `dep.State()` in `monitorNeeded`/`monitorCmds` do not. | writes `daemons.go:263-424`; unguarded reads `daemon.go:608`, `:141-168`, `:282` | `Requisite=` naming a non-`.service` unit (e.g. any `.target`); `UpheldBy=`; `StopWhenUnneeded=true` |
| **A7** | Self-deadlock in `Reload()`: the write lock on `ds` is held, `d.Lock()` is taken, then `ds.list[depName].Lock()` is taken — which is the *same* mutex when a unit refers to itself. Go mutexes are not reentrant; init hangs forever with the global lock held. | `daemons.go:254-425` | `Requires=self.service` (occurs in the wild via generated drop-ins) |
| **A8** | Lock-order inversion between `LoadAndStart` (holds `ds.RLock()` for the entire boot while calling `d.Start()`) and `Find()` (drops the read lock and takes the **write** lock via `Reload()`). A blocked writer blocks subsequent readers in Go's `RWMutex`, so any `systemctl` command issued during a slow boot can deadlock init. | `daemons.go:48-69`, `:430-446` | `systemctl` during boot with a slow unit |
| **A9** | `monitorCmds` reads and writes `d.state`, `d.cmds`, `d.pids` and `d.def.RestartSleep` with **no lock held**, concurrently with `stop()`/`Status()`/`Reload()` which hold one. Unsynchronised map/slice access; `go test -race` would flag every path. | `daemon.go:178-376` | concurrent `stop` during exit handling |

**Disposition.** The new design removes the whole class:
per-unit state is owned by a single goroutine/actor per unit and mutated only by
message passing ([04 §3](04-architecture.md#3-concurrency-model)); dependency edges are
stored as **names**, resolved through the registry at use time, so a nil pointer is
structurally impossible ([04 §5](04-architecture.md#5-the-unit-registry)); PID 1 runs
no unit logic at all (ADR-3); and a top-level `recover()` per unit actor converts a
residual bug into a failed unit rather than a dead container.

## B. Functional failures (S2)

| ID | Defect | Location | Fix |
|---|---|---|---|
| **B1** | **`systemctl stop` cannot stop a `Type=forking` service.** All the LD_PRELOAD tracking feeds `d.pids`, which is only ever *read* to print PIDs in `status`. `stop()` signals `d.cmds`, which for a forking unit is empty by then. | `daemon.go:888-941` signal only `d.cmds`; `d.pids` written at `:180-264`, read only at `:1146` | The supervisor kills the **whole live tree**, iteratively, until empty — [03 §8](03-process-model.md#8-the-termination-ladder) |
| **B2** | Even for `Type=simple`, only the direct child is signalled; its children are orphaned and leak. `KillMode=control-group` (systemd's default) is not implemented. | `daemon.go:888` | as B1 |
| **B3** | `set-environment` and `unset-environment` are **swapped**: `cmdSetEnvironment.Execute` calls `os.Unsetenv(arg)`; `cmdUnsetEnvironment.Execute` calls `os.Setenv`. | `command.go:490-508` | Correct implementations, plus persistence to the manager environment block applied to subsequently started units |
| **B4** | `Mask()` symlinks to the **literal string `"target"`** instead of the `target` variable: `os.Symlink("/dev/null", "target")`. A file named `target` is created in init's CWD and the mask is lost on restart. | `daemon.go:1091` | Correct path; mask state derived from the filesystem on every load, never cached |
| **B5** | `journalctl -n N` runs `tail -n N` **with no filename**, so it tails init's stdin and prints nothing. | `journalctl/main.go:94` | Native implementation, no `tail` subprocess |
| **B6** | `journalctl` time filters are only applied when **both** `--since` and `--until` are supplied (`if !since.IsZero() && !until.IsZero()`), so `-b` and `-S` alone silently do nothing. | `journalctl/main.go:119` | Correct predicate |
| **B7** | The log files contain **no timestamps** — `Logger.Write` appends raw bytes; only the `--log-to-stderr` mirror is prefixed. So `journalctl --since` can never match, and `-b` is meaningless. | `daemons/logger.go:42-55` | Structured, timestamped, per-record log format — [07](07-logging.md) |
| **B8** | `Requisite=` is wired into `Requires=` (copy/paste), inverting the directive's meaning from "fail if not already running" to "start it". | `daemons.go:300` | Correct wiring, with a table-driven dependency-kind definition instead of 18 copy-pasted blocks |
| **B9** | `OnFailure=` is cross-wired to `OnSuccess=` as its inverse (and vice versa). The inverse of `OnFailure` is not `OnSuccess`. | `daemons.go:407-424` | Drop the bogus inverse; `OnFailure`/`OnSuccess` have no inverse edge |
| **B10** | `StopWhenUnneeded=` is a no-op check: each guard loop is `for _, dep := range … { if dep.State() == StateRunning { continue } }` — `continue` advances the *inner* loop, so nothing is ever skipped and the unit is stopped one second after start regardless. | `daemon.go:140-169` | Correct predicate, or drop the directive and document it as unsupported |
| **B11** | `Before=`/`After=` are parsed, stored, wired — and never consulted. Boot order is `ReadDir` order. | no ordering pass exists | Topological ordering pass — [04 §7](04-architecture.md#7-job-scheduling-and-ordering) |
| **B12** | `Type=notify` is silently treated as `forking`; `$NOTIFY_SOCKET` is never set, so any daemon that *requires* notify semantics (`sd_notify(READY=1)` before its dependents start) is mis-sequenced. | `daemon.go:190` | Implement `sd_notify` — [03 §7](03-process-model.md#7-main-pid-determination) |
| **B13** | `delete-instance` calls `DeleteService()`, which `os.RemoveAll`s **every path in `d.paths`** with no instance check — running it on a non-template unit **deletes the distro's unit file** from `/lib/systemd/system`. | `command.go:130`, `daemon.go:1040` | Instances are resolved from templates, never materialised; `delete-instance` only removes generated state — [05 §6](05-unit-semantics.md#6-templates-and-instances) |
| **B14** | `stop()` opens a `Logger` and never closes it — one leaked file descriptor per stop/restart. A unit with `Restart=always` that flaps exhausts init's fd table. | `daemon.go:844` (no matching `Close`) | Log sinks are owned by the supervisor for the unit's lifetime, opened once |
| **B15** | `start()`'s error paths call `l.Close()` and then pass the closed logger to `d.runOnFailure(l)`. | `daemon.go:691-697` etc. | Lifetime ownership as B14 |
| **B16** | `procwait.Run` registers the command in a global map **before** `cmd.Start()`; if `Start()` fails the entry is never deleted, and the reaper linearly scans that map on every child exit. Unbounded growth + O(n) per exit. | `procwait.go:37-55`, `:175-183` | Per-child wait channels owned by the spawning code path; no global registry |
| **B17** | `procwait.Wait()` returns `nil` both for "already exited" and for genuine races, so callers cannot distinguish exit codes and `monitorCmds` treats an already-dead process as a success. | `procwait.go:72-95` | Wait results are `(status, error)` with an explicit `ErrNoSuchProcess` |
| **B18** | `enable` writes a **regular file containing `OK`** instead of a symlink into `.wants`, and always into `multi-user.target.wants` regardless of `[Install] WantedBy=`. `systemctl disable` of a package-installed symlink works, but `enable`'s output is not what any other tool expects. | `daemon.go:1062` | Real symlinks, honouring `WantedBy=`/`RequiredBy=`/`Alias=`/`Also=` |
| **B19** | `/run/systemd/system` is never created, so distro maintainer scripts conclude systemd is not running and skip unit enablement, socket registration, and restarts on upgrade. | absent | Created at boot — [06 §5](06-cli-surface.md#5-making-maintainer-scripts-work) |
| **B20** | `Type=oneshot` force-sets `RemainAfterExit=true`, so every oneshot unit reports `Running` forever and `Restart=` handling takes the wrong branch. | `daemon.go:499-501` | Correct oneshot semantics: sequential `ExecStart=` lines, `active(exited)` only when `RemainAfterExit=yes` |

## C. Compatibility and correctness gaps (S3)

| ID | Defect | Location | Fix |
|---|---|---|---|
| **C1** | **Everything runs through `/bin/bash -c`.** systemd does not use a shell: it word-splits itself, expands only `$VAR`/`${VAR}` from the unit environment, and does not glob or honour metacharacters. Consequences: `bash` becomes a mandatory dependency; `ExecStart=/bin/echo a;b` changes meaning; and — critically — the shell is an **extra process layer** that makes the real main PID ambiguous. | `daemon.go:576,713,747,788,854,872,902,990` | Native lexer + `execve` — [05 §4](05-unit-semantics.md#4-command-lines) |
| **C2** | Boolean parsing accepts only the literal `true`; systemd accepts `yes/no/on/off/1/0/true/false`. `RemainAfterExit=yes` — the spelling used by essentially every real unit file — is read as **false**. | `unit.go:288,340` | Full boolean grammar |
| **C3** | `EnvironmentFile=` content is split on `\n` with no comment, blank-line, quote or `export ` handling; comment lines become environment entries. | `daemon.go:534` | Proper `.env` parsing per systemd rules |
| **C4** | `Environment=` accepts only one assignment per line; systemd accepts several, space-separated, with quoting. | `unit.go:449` | Full grammar |
| **C5** | Quote stripping in `parseUnitLine` strips *outer* quotes from the whole value including for `Exec*=`, changing `ExecStart="/path with spaces/x" -a` handling. | `unit.go:467-474` | Quote handling belongs in the command lexer, not the line splitter |
| **C6** | `User=`/`Group=` are dropped whenever the resolved uid is 0, so `User=root Group=adm` silently runs with the wrong group; no supplementary groups; `$HOME`, `$USER`, `$LOGNAME`, `$SHELL` are not set. | `daemon.go:536-561,752` | Full credential setup — [05 §7](05-unit-semantics.md#7-execution-context) |
| **C7** | `%i`/`%I` are the only specifiers, and they are expanded only in `Exec*=`. `%n`, `%N`, `%p` appear in stock units. `%I` should be the *unescaped* instance name; here it is identical to `%i`. | `unit.go:348-394` | Full specifier table |
| **C8** | Only the `-` `Exec` prefix is honoured; `@`, `+`, `!`, `!!`, `:` are treated as part of the path. | `daemon.go:708,742,783` | All prefixes parsed; unsupported ones warn once |
| **C9** | `ExecStopPre=` is invented — no such systemd directive. Units relying on the real ordering (`ExecStop` then `ExecStopPost`) are unaffected, but the extension should be documented or dropped. | `unit.go:371` | Dropped; documented in [12 §7](12-implementation-plan.md#7-behaviour-differences-from-systemd) |
| **C10** | Multiple `ExecStart=` lines are accepted for non-oneshot types and started **in parallel**; systemd rejects this. | `daemon.go:738` | Reject at parse time with a clear error |
| **C11** | `TimeoutStopSec` defaults to 5 s (systemd: 90 s) and there is **no `TimeoutStartSec`** at all — a hanging `ExecStartPre` hangs the unit and, because boot is serial under a read lock, the whole boot (see A8). | `daemon.go:896` | Both timeouts, systemd defaults, enforced by the supervisor |
| **C12** | `RestartSec` is coerced to a minimum of 1 s (systemd: 100 ms default, 0 permitted) and there is no `StartLimitIntervalSec`/`StartLimitBurst`, so a crash-looping unit restarts forever. | `daemon.go:288` | Honour the value; add start rate limiting |
| **C13** | `PIDFile=` is trusted absolutely: the pid is extracted by keeping only ASCII digits from anywhere in the file (so `pid=/var/run/x` yields a nonsense number) and is never checked for membership in the unit. A stale pid file makes init adopt an unrelated process — and later `SIGKILL` it. | `daemon.go:192-208` | Parse strictly; **validate the pid is inside the unit's tree**, else fail the unit — [03 §7](03-process-model.md#7-main-pid-determination) |
| **C14** | Shutdown iterates units in Go **map iteration order** — no reverse-dependency order, no parallelism, up to 5 s each. Docker's default 10 s grace is exceeded with three slow units and the container is `SIGKILL`ed mid-shutdown. | `daemons.go:73-90` | Reverse-topological, parallel within a stratum, global deadline budget — [04 §9](04-architecture.md#9-shutdown) |
| **C15** | The `service` compatibility shim swaps `argv[1]`/`argv[2]` only when `len(args) > 2`, so `service --status-all` and `service foo` misbehave. | `main.go:42-49` | Proper argument mapping |
| **C16** | `strings.Split(nenv, "=")[1]` truncates any environment value containing `=`. | `daemon.go:227` | Split on the first `=` only |
| **C17** | `Reload()` (i.e. `daemon-reload`) rebuilds every `def` while units are running, so `ExecStop` may come from a different generation of the unit file than `ExecStart` did. systemd keeps the running job's definition. | `daemons.go:92` | Running jobs pin their definition; the new one applies at next start |
| **C18** | No `.target` support at all: `Wants=network-online.target` resolves to nil, `multi-user.target` exists only as a directory name. | throughout | Synthetic targets — [05 §5](05-unit-semantics.md#5-targets) |

## D. Protocol and client defects (S3)

| ID | Defect | Location | Fix |
|---|---|---|---|
| **D1** | Both ends assume one `read()` returns a whole message. On a `SOCK_STREAM` socket a long argument list or long output splits across reads → "message malformed" or truncated output. | `systemd/main.go:172-198`, `systemctl/main.go:51` | Length-prefixed framing with full reads — [08](08-control-protocol.md) |
| **D2** | The end-of-output sentinel is a bare `0x00` byte; any unit output containing `0x00` terminates the stream early. | `systemd/main.go:200`, `systemctl/main.go:55` | Typed frames |
| **D3** | 64 KiB hard cap on both request and response. | both | Streaming frames |
| **D4** | The response-code path does `log.Fatalf("Received extra bytes…")` on any unexpected trailing data, turning a protocol hiccup into a client crash. | `systemctl/main.go:70` | Defined error handling |
| **D5** | `systemctl` exits with `log.Fatal` and a Go-formatted dial error when init is not running, instead of the conventional message and exit status. | `systemctl/main.go:29` | `Failed to connect to bus: No such file or directory`, exit 1 |
| **D6** | `command()`'s error switch has an unreachable `default` branch that calls `msg.Error()` on a value typed as `error` after already matching `cmdResponse` and `*flags.Error`; unknown-command errors return exit **0**. | `command.go:423-449` | Explicit exit-code mapping — [06 §4](06-cli-surface.md#4-exit-codes) |

## E. Security (S3/S4)

| ID | Defect | Location | Fix |
|---|---|---|---|
| **E1** | The control socket lives at **`/tmp/docker-systemd.sock`** with default permissions in a world-writable, sticky directory. **Any user in the container can start, stop, mask, or `poweroff` anything, as root.** A service deliberately dropped to an unprivileged `User=` can trivially escalate by asking init to start a root unit. | `common.go:70`, `systemd/main.go:140` | `/run/docker-systemd/control.sock`, dir 0700 root-owned, plus `SO_PEERCRED` uid check — [09](09-security-model.md) |
| **E2** | The pidtrack socket is likewise world-writable; any process can inject arbitrary parent/child edges and cause init to `SIGKILL` a process of its choosing (via `d.pids`… which is never killed today, so latent — but it does corrupt `status` and cause spurious waits). | `systemd/main.go:45` | Mechanism removed entirely (ADR-1) |
| **E3** | `/var/log/pidtrack.log` — if any user creates this file, **every process in the container** appends to it forever, unrotated. Debug leftover. | `preload.c:51-53` | Removed with the shim |
| **E4** | `PIDFile=` trust (C13) is a privilege-escalation primitive once B1 is fixed and the pid is actually signalled: a unit running as `User=nobody` writes root's pid into its pid file and gets init to `SIGKILL` it. | `daemon.go:192` | Tree-membership validation (C13) |
| **E5** | Unit log files are created 0644 under `/var/log/services/` and are written by init as root while the service may run as another user; no `StandardOutput=` control. | `logger.go:31` | 0640 root:adm, configurable |

## F. Robustness, cost, and hygiene (S4)

| ID | Defect | Disposition |
|---|---|---|
| **F1** | `pidtracker.relations` never has entries removed → unbounded memory growth in PID 1, one entry per `exec` in the container, forever; `Find()` recursively scans the whole map inside a polling loop. | Removed with the shim |
| **F2** | PID reuse aliasing in the same map: stale edges attribute new, unrelated processes to a unit. | Removed; new design validates by `(pid, start-time)` and prefers `pidfd` |
| **F3** | `procwait` uses `sync.RWMutex` as a condition variable — `Lock()` in one goroutine, `Unlock()` from another, plus an ignored `TryLock()`. It happens to work; it is not a defensible synchronisation primitive and `go vet`/race detector flag it. | Channels / `waitid` per child |
| **F4** | `procwait.Is()` uses `Signal(0)`, which succeeds for **zombies**, so "has it exited?" polls can spin until timeout. | Wait on `pidfd`, or check `/proc/<pid>/stat` state `Z` |
| **F5** | Writing `/usr/local/lib/fork.so` with `os.WriteFile` over a file currently mapped by running processes risks `ETXTBSY`/torn loads; the directory may not exist; **all errors are ignored**. If the write fails after `/etc/ld.so.preload` was updated, *every* dynamically linked binary in the container prints a loader error. | Removed with the shim |
| **F6** | Committing an image that ran with pidtrack bakes `/etc/ld.so.preload` into the layer; if the `.so` is absent on next run the image is effectively bricked. Never cleaned up, not even by `--no-pidtrack` (which only overwrites the `.so` with an empty stub). | Removed with the shim |
| **F7** | Four `.so` blobs committed to git, built by a manual `docker`-dependent script, not reproducible in CI, inconsistent between architectures (`fakefork_amd64.so` = 7,896 B from an empty `.c`; `fakefork_arm64.so` = 70,120 B). | Deleted |
| **F8** | `install()` renames the distro's `/sbin/init`, `/sbin/poweroff`, `/sbin/shutdown`, … to `*.old` and symlinks itself over them, unconditionally and irreversibly, with `log.Fatalf` on failure. | Keep the behaviour (it is the product) but make it idempotent, logged, opt-out (`--no-install`), and non-fatal per-name |
| **F9** | No log rotation anywhere. | Size-capped rotation — [07 §4](07-logging.md#4-rotation) |
| **F10** | UPX compression of PID 1 (`make shrink`) — inflates RSS (the whole binary is decompressed into anonymous memory), and every re-exec'd supervisor in the new design would pay it again. | Drop UPX; a `-trimpath -ldflags="-s -w"` build is ~6 MB, which is noise next to a distro image |
| **F11** | CI installs Go 1.21.4 to build a `go 1.22` module, relying on implicit toolchain download. | Pin the toolchain in `go.mod` and use `actions/setup-go` |
| **F12** | `Logger.Close()` dereferences `l.f` unconditionally; harmless only because `(*os.File).Close()` nil-checks internally. | Explicit nil handling |
| **F13** | `GetSystemdPaths()` is a six-branch symlink heuristic; drop-in `.d` directories are only scanned at the top level of unit directories; `/run/systemd/system` and `*.wants`/`*.requires` subdirectories of unit directories are not scanned. | Single canonical search-path algorithm — [05 §1](05-unit-semantics.md#1-unit-discovery) |
| **F14** | Zero automated tests. | [11](11-testing.md) |
| **F15** | `log.Print("NEWCONN")` and similar debug output on the boot console. | Levelled logging |

## G. Summary counts

| Severity | Count |
|---|---|
| S1 container-fatal | 9 |
| S2 functional failure | 20 |
| S3 compatibility/correctness | 24 |
| S4 hygiene/cost/latent | 15 |
| **Total** | **68** |

Of these, 21 are eliminated outright by removing the `LD_PRELOAD` subsystem
(A6 partially, B1, B2, E2, E3, F1, F2, F5, F6, F7 and their dependents), and a further
9 by removing `bash -c` and the shared mutable daemon struct.
