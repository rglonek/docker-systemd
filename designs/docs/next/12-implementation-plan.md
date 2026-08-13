# 12 — Implementation plan

Clean-room rebuild in nine phases. Each phase ends in a mergeable, testable state; no
phase leaves the tree in a "half-migrated" condition. Estimates are engineer-days for
one person familiar with the domain, excluding review.

---

## 0. Ground rules

* **Clean room.** Write against the specification documents, not against the existing
  source. Two exceptions, explicitly permitted because they are already correct and
  self-contained: the systemd duration parser (`systemd/daemons/unit.go:479-736`) and
  the base-image/search-path knowledge encoded in `common.GetSystemdPaths`. Everything
  else is re-derived.
* **New module layout**, not an in-place edit of the old packages. The old tree is
  deleted in Phase 9, in one commit, once the new one passes the conformance matrix.
* **Tests land in the same PR as the code they cover.** No "tests later" phase.
* **Every defect in [02](02-defect-register.md) carries a test.** The register is the
  acceptance checklist; a defect is closed when its test exists and passes.
* **Feature flags for behaviour changes.** Anything in §7 that changes observable
  behaviour ships behind a flag with the compatible value as the default for one
  release, then flips.

## 1. Target layout

```
cmd/
  docker-systemd/         main; argv[0] dispatch; --supervise and --exec-helper modes
internal/
  manager/                PID 1: registry, graph, jobs, control server, log broker, shutdown
  supervisor/             per-unit supervisor: spawn, proctree, notify, ladder, restart
  unitfile/               discovery, lexer, parser, drop-ins, specifiers, validation
  graph/                  dependency kinds, transactions, topological ordering
  proctree/               /proc enumeration, ProcRef, pidfd
  backend/                cgroup2 | subreaper | degraded, behind one interface
  proto/                  framing and message types (shared by client and server)
  journal/                record format, rotation, reader
  cli/                    systemctl, journalctl, service, poweroff, systemd-notify
  probe/                  runtime capability probes (11 §6)
testdata/units/           harvested real unit files
test/
  daemons/                the testdaemon helper (11 §3)
  conformance/            per-base-image container tests
designs/docs/next/        this design
designs/docs/next/attic/  preload.c and the historical analysis
```

## 2. Phases

### Phase 1 — Skeleton and protocol · 3 d

* `cmd/docker-systemd` with `argv[0]` dispatch and the `getpid()==1` override.
* `internal/proto`: framing, message types, HELLO handshake, full-read/full-write.
* Control server in the manager; `systemctl --version` and `systemctl list` (empty)
  round-trip end to end.
* Runtime directory creation with correct modes; `SO_PEERCRED` authentication.
* Structured logging with levels.

**Acceptance.** `systemctl --version` works inside a container; the control socket is
`0600` in a `0700` directory; a non-root uid is refused; a 4 MiB response and a payload
containing `0x00` both round-trip (D1–D4, E1).

### Phase 2 — Unit files · 6 d

* Discovery with `realpath` directory de-duplication, drop-ins, `.wants`/`.requires`,
  masks, aliases.
* Lexer and parser; boolean, duration and size grammars; the full directive table;
  specifiers; command-line lexing with all prefixes; validation diagnostics.
* Registry with copy-on-write snapshots; `daemon-reload`.
* `systemctl cat`, `show`, `list-unit-files`.

**Acceptance.** Every L1 test in [11 §2](11-testing.md#2-l1--pure-unit-tests) passes,
including the whole C-class defect list; the fuzzers run 60 s clean; the harvested unit
files from all nine base images parse with zero unexpected warnings.

### Phase 3 — Supervisor core · 8 d — *the critical phase*

* `internal/proctree`: `/proc` enumeration, `ProcRef` with start-time, pidfd upgrade,
  signal helpers.
* `internal/backend`: probe and select; the `subreaper` implementation; `degraded`.
* `internal/supervisor`: `PR_SET_CHILD_SUBREAPER`, spawn with the pre-exec context, the
  `--exec-helper` trampoline, reaping, `Type=simple|exec|oneshot|forking`, main-PID
  determination, the termination ladder.
* Manager ↔ supervisor protocol; supervisor exit as the completion signal.

**Acceptance.** Every L2 mode in [11 §3](11-testing.md#3-l2--process-level-tests)
passes. In particular: `libc-daemon` is tracked and stopped (the case the old shim
scored zero on); `ignore-sigterm` and `fork-bomb-on-term` both terminate with zero
survivors within `TimeoutStopSec + 1 s`; a stale `PIDFile=` fails the unit and leaves
the unrelated process alive (B1, B2, C13, E4).

### Phase 4 — Dependency graph and jobs · 5 d

* Dependency kinds as data; transaction building; conflict resolution; topological
  ordering; cycle breaking; parallel execution within a stratum.
* Job timeouts, `Restart=` policy, `StartLimitBurst`, `OnFailure=`/`OnSuccess=`.
* Boot from `default.target`; synthetic targets.

**Acceptance.** B8, B9, B10, B11, B20, C12 tests pass; a ten-unit fixture with a
declared order boots in that order and in parallel where permitted; an ordering cycle is
broken with a warning rather than a hang (A7).

### Phase 5 — `sd_notify`, credentials, exec context · 4 d

* Notify sockets with `SO_PASSCRED`; `READY=1`, `MAINPID=`, `STATUS=`, `RELOADING=1`,
  `STOPPING=1`, `WATCHDOG=1`.
* `User=`/`Group=`/`SupplementaryGroups=`, `$HOME`/`$USER`/`$LOGNAME`.
* `Limit*=` with clamping and diagnostics; `UMask=`, `Nice=`, `OOMScoreAdjust=`,
  `NoNewPrivileges=`; `RuntimeDirectory=` and friends.
* `Condition*=`/`Assert*=`, including `ConditionVirtualization=container`.
* `StandardInput/Output/Error=`.
* `systemd-notify` helper.

**Acceptance.** B12, C6, C11 tests pass; `redis-server` (a real `Type=notify` unit)
starts, reports `MainPID` exactly, and honours `LimitNOFILE=`.

### Phase 6 — Logging and journal · 4 d

* Log broker, record format, per-unit sinks, rotation, `LogTotalMax`.
* `journalctl` rewritten natively: `-u` optional, `-n`, `-f` (inotify), `-S`/`-U`/`-b`
  with binary search, `-p`, `-o`, `--disk-usage`, `--vacuum-*`; legacy-line tolerance.

**Acceptance.** B5, B6, B7, B14, F9 tests pass; a unit producing 100 MB leaves the
directory at `LogTotalMax`; `journalctl --since` returns the right window.

### Phase 7 — CLI completeness · 5 d

* All verbs in [06 §2.1](06-cli-surface.md#21-verbs), the option set, the output
  formats, the exit-code table.
* `service` shim, `poweroff`/`halt`/`reboot`/`shutdown`, `systemd-detect-virt`.
* `enable`/`disable` writing real symlinks into the `[Install]`-named target;
  `preset`, `link`, `revert`, `add-wants`.
* Self-install: idempotent, `.dist` naming, non-fatal, `--no-install`.
* `/run/systemd/system` marker.

**Acceptance.** B3, B4, B13, B18, B19, C15, D5, D6 tests pass; the maintainer-script
checklist in [06 §5](06-cli-surface.md#5-making-maintainer-scripts-work) is green.

### Phase 8 — Shutdown, recovery, hardening · 3 d

* Reverse-topological parallel shutdown with a global budget; the three-signal escape
  hatch; final sweep and reap.
* Orphan recovery via the invocation-id environment marker; `daemon-reexec`.
* The hardening checklist in [09 §4](09-security-model.md#4-hardening-checklist-for-the-implementation).

**Acceptance.** C14 test passes: six services, three slow, `docker stop -t 10` exits
cleanly within the grace period; killing a supervisor recovers its orphans.

### Phase 9 — Migration, CI, removal · 4 d

* Conformance matrix in CI across all base images and both architectures.
* Deb/RPM packaging and the release pipeline, with **no** manual `docker` step.
* **Delete** `forkpreload/`, `systemd/pidtracker/`, `systemd/fork.go`, all `*.so`, the
  old `systemd/`, `procwait/`, `journalctl/`, `systemctl/`, `common/` packages.
* Move `preload.c` and the coverage measurements to `designs/docs/next/attic/`.
* README, QUICKSTART, CHANGELOG, and a migration note.

**Acceptance.** The conformance matrix is green; `git grep -i ld_preload` returns only
documentation; a clean checkout produces every release artefact.

**Total: ~42 engineer-days.** Phases 1–3 are the load-bearing 40%; if the schedule
compresses, cut Phase 7's long tail (`link`, `revert`, `preset-all`) and Phase 6's
`-o json`, not Phase 3's test matrix.

## 3. Dependency order

```
1 ──▶ 2 ──▶ 4 ──▶ 7 ──▶ 9
      │      ▲     ▲
      └▶ 3 ──┴▶ 5  │
              └▶ 6 ┘
                 8 (needs 3 and 4)
```

Phases 3 and 4 can proceed in parallel after 2. Phase 6 is independent of 4/5.

## 4. Risk register

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| A base image's kernel lacks `PR_SET_CHILD_SUBREAPER` (< 3.4, i.e. pre-2012) | very low | high | `degraded` backend + loud warning; probe in CI on every image |
| `pidfd_*` blocked by a hardened seccomp profile in some user's environment | low | low | feature-probed; falls back to `kill` + start-time validation |
| Supervisor RSS × many units in a memory-capped container | medium | medium | measured in L5 soak; documented; threaded-host design noted as the escape hatch ([10 §6](10-language-choice.md#6-conditions-that-would-reverse-this)) |
| Dropping `bash -c` breaks a user's unit that relied on shell syntax | **high** | medium | this is the intended behaviour change; ship `--compat-shell-exec` for one release, default off, with a loud warning naming the unit and line |
| Removing `--no-pidtrack` breaks a user's `docker run` command line | medium | low | accept and ignore with a deprecation warning for one release |
| The `/tmp` socket move breaks a user's tooling | low | medium | `--compat-tmp-socket` for one release, default off, documented as insecure |
| `/proc` unreadable under `hidepid=2` | low | high | probe at boot; `degraded` backend; documented |
| Scope creep into full systemd compatibility | **high** | high | the directive table in [05 §8](05-unit-semantics.md#8-directive-support-matrix-for-the-new-version) is the contract; anything outside it is a separate proposal |

## 5. What ships in v1 vs later

**v1** — everything in phases 1–9.

**Deferred, with the design left open for them:**

* `.socket` units and socket activation (`LISTEN_FDS`) — the protocol already reserves
  fd passing, and `sd_notify` infrastructure is shared.
* `.timer` units — needs a calendar-spec parser; genuinely useful in containers as a
  `cron` replacement.
* `.path`, `.mount`, `.automount`, `.slice`, `.scope`.
* `--user` instances.
* A D-Bus-compatible API. Some tools speak D-Bus rather than the CLI; a `busctl`-shaped
  shim is a large piece of work and should be justified by demand.
* Per-verb authorisation policy on the control socket
  ([09 §3.2](09-security-model.md#32-control-socket-authentication)); the dispatch table
  carries the `required_privilege` column from v1 so it is a small change later.

## 6. Migration for existing users

| Change | Impact | Action |
|---|---|---|
| Unit commands no longer run under `bash -c` | a unit using `&&`, `\|`, `;`, `>`, globs, or `~` will fail | ship `--compat-shell-exec`; warn at load naming the unit and line; document rewriting as `ExecStart=/bin/sh -c '…'` |
| `RemainAfterExit=yes` now actually parses as true | units that accidentally worked because it parsed as false may change state reporting | documented; this is the correct behaviour |
| `Type=oneshot` no longer forces `RemainAfterExit` | oneshot units now go `inactive` after they finish, as in systemd | documented |
| Control socket moves to `/run/docker-systemd/control.sock`, root-only | tooling that connected to `/tmp/docker-systemd.sock` as non-root breaks | `--compat-tmp-socket` for one release; `--control-allow-uid=` for legitimate cases |
| `/etc/ld.so.preload` no longer written | none, except that an image committed from an older container may carry a dangling entry | the new manager **removes** its own stale entry (`/usr/local/lib/fork.so`) at boot if present — a small, explicit cleanup path |
| `--no-pidtrack` removed | flag now ignored | accepted with a deprecation warning for one release |
| `delete-instance` no longer deletes unit files | previously it could delete the distro's unit file (B13) | documented as a bug fix |
| `enable` writes symlinks instead of a file containing `OK` | none; strictly more compatible | — |
| Log format gains timestamps | log parsers keyed on the old raw format need updating | `journalctl` reads both; document the new record layout |
| `/etc/boot-time` moves to `/run/docker-systemd/boot-id` | none | remove the stale file at boot |

Ship a **`systemctl show --property=Capabilities`** (or `docker-systemd doctor`) that
prints the selected backend, the probe results, the runtime paths, and every
warned-about directive in the loaded units. It turns "it doesn't work" bug reports into
one paste.

## 7. Behaviour differences from systemd

To be documented in the README — kept short, honest, and prominent rather than buried:

1. No cgroups under plain `docker run`; resource-control directives are ignored with a
   warning. Use `docker run --memory/--cpus/--ulimit`.
2. Sandboxing directives (`PrivateTmp=`, `ProtectSystem=`, …) are ignored with a
   warning — they need privileges the container does not have.
3. Only `.service` and `.target`; no `.socket`, `.timer`, `.path`, `.mount`.
4. No D-Bus, so `Type=dbus` degrades to `simple` and `BusName=` is ignored.
5. `reboot` stops the container; combine with `docker run --restart=always` for the
   closest analogue.
6. `journalctl -b -1` and persistent journals across container restarts require
   `/var/log` to be a volume.
7. `systemctl --user` is not supported.
8. Process tracking uses subreaper adoption rather than cgroup membership; a process
   that a unit hands to a *helper started outside the unit* is not tracked. Everything
   started by the unit itself is.
