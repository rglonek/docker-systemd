# 11 — Testing strategy

The current repository has **zero automated tests**. `test/` is a manual demo that
downloads an Aerospike tarball. Every defect in [02](02-defect-register.md) shipped
because nothing could have caught it.

This is not a "nice to have" section. For an init system, the test suite *is* the
product's reliability.

---

## 1. Layers

| Layer | Runs | Speed | Catches |
|---|---|---|---|
| L1 unit tests | `go test ./...` | < 5 s | parser, lexer, duration/boolean grammar, specifier expansion, dependency table, ordering/cycle-breaking, protocol framing, log record encode/decode |
| L2 process tests | `go test -tags=proc` on a plain Linux host | < 30 s | supervisor behaviour against synthetic daemons: subreaper adoption, tree enumeration, the termination ladder, `sd_notify`, `pidfd` paths |
| L3 container tests | inside a built image | < 5 min | boot, unit lifecycle, `systemctl`/`journalctl` surface, shutdown timing, socket permissions |
| L4 conformance matrix | one container per base image × package set | < 30 min | real distro packages installing, enabling and starting |
| L5 soak | nightly | hours | fd/memory leaks, restart storms, log rotation, PID churn |

CI runs L1–L4 on every PR; L5 nightly.

## 2. L1 — pure unit tests

Table-driven, with a fixtures directory of real unit files harvested from the supported
base images (`nginx.service`, `postfix.service`, `mariadb.service`, `cron.service`,
`ssh.service`, `redis-server.service`, `getty@.service`, plus their drop-ins).

Must include a regression case for **every** defect in [02](02-defect-register.md) that
is expressible without processes:

| Defect | Test |
|---|---|
| C2 | `RemainAfterExit=yes` parses as **true** |
| C3 | an `EnvironmentFile=` containing comments, blanks, `export `, quotes, continuations |
| C4 | `Environment="A=1 2" B=3` yields two variables |
| C5 | `ExecStart="/opt/my app/bin" --flag` keeps the space in the path |
| C7 | `%n %N %p %P %i %I %f %t %H %%` all expand; `%I` differs from `%i` for `a-b` |
| C8 | each of `-`, `@`, `:`, `+`, `!`, `!!` |
| C10 | two `ExecStart=` for `Type=simple` is a parse error |
| B8 | `Requisite=` does not create a start edge |
| B9 | `OnFailure=` creates no inverse edge |
| A5 | an alias symlink registers one unit with two names, both loaded |
| A4 | a unit whose fragment disappears between reloads does not produce a null config |
| A6 | `Requisite=nonexistent.target` resolves to *absent*, not to a null unit |
| A7 | `Requires=self.service` loads and does not deadlock |
| B11 | ordering: `After=` produces the expected topological order; a cycle is broken with a warning, not a hang |
| D1–D3 | framing: a 4 MiB response, a payload containing `0x00`, a message split across ten reads |

Fuzz targets: the unit-file parser, the command lexer, the duration parser, and the
protocol framer. All four take untrusted-ish input and all four have historically had
bugs.

## 3. L2 — process-level tests

A `testdaemon` helper binary, built by the test harness, with modes:

| mode | behaviour |
|---|---|
| `foreground` | runs until signalled |
| `double-fork` | classic daemonise, then sleep |
| `libc-daemon` | `daemon(3)` — **the case the old shim scored 0 on** |
| `posix-spawn` | spawns a child via `posix_spawn` |
| `vfork` | spawns via `vfork` |
| `setsid-escape` | `setsid` into a new session, then fork |
| `pidfile` | writes a pid file (correct / stale / hostile / malformed variants) |
| `notify` | `sd_notify(READY=1, MAINPID=…)` |
| `ignore-sigterm` | catches and ignores `SIGTERM` |
| `fork-bomb-on-term` | forks a new child every time it is signalled |
| `slow-stop` | takes N seconds to exit after `SIGTERM` |
| `closefrom` | closes all descriptors on start (defeats inherited-fd liveness tricks) |
| `crash-loop` | exits non-zero immediately |
| `zombie-maker` | forks children and never reaps them |

Assertions per mode: the supervisor's tree matches the expected set; `MainPID` is
correct; `systemctl stop` leaves **zero** surviving processes; the stop completes within
`TimeoutStopSec + ε`; no zombies remain under PID 1; no fds leak.

The `fork-bomb-on-term` and `ignore-sigterm` cases are the ones that prove the
termination ladder converges, and `libc-daemon` is the one that proves the whole design
premise.

## 4. Conformance matrix

Base images: `ubuntu:24.04`, `ubuntu:22.04`, `ubuntu:20.04`, `debian:12`, `debian:11`,
`debian:10`, `rockylinux:9`, `rockylinux:8`, `quay.io/centos/centos:stream9`, plus
`alpine:3` as an **expected-degraded** case that must at least boot and run
`Type=simple` units (it has no glibc, which the old shim silently could not handle at
all).

Packages, installed via the distro's own tooling *inside `docker build`* so that
maintainer scripts run:

| Package | Exercises |
|---|---|
| `cron` / `cronie` | `Type=simple`, enable-on-install |
| `nginx` | `Type=forking` + `PIDFile=`, `ExecReload=`, drop-ins |
| `postfix` | multiple processes, `Type=forking`, a shell-ish `ExecStart` |
| `mariadb-server` | `Type=notify`, long start, `TimeoutStartSec` |
| `openssh-server` | `Type=notify`, `RuntimeDirectory=`, `ConditionPathExists=` |
| `redis-server` | `Type=notify`, `User=`, `LimitNOFILE=` |
| `rsyslog` | `Type=notify`, `StandardOutput=` |
| `apache2` / `httpd` | `Type=forking`, `EnvironmentFile=`, `%i` free |

Per package, assert: the package installs without error; `systemctl is-enabled` matches
the distro's intent; `systemctl start` succeeds; the service answers on its port or
socket; `systemctl status` shows a plausible `MainPID` and process tree;
`systemctl stop` leaves zero surviving processes; `systemctl restart` works ten times in
a row; `journalctl -u` shows output.

Both architectures (`linux/amd64`, `linux/arm64` via QEMU) for at least one image, since
the project ships both and the old `.so` files diverged between them (F7).

## 5. Behaviour tests worth calling out

| Scenario | Assertion |
|---|---|
| `docker stop -t 10` with six services, three of which take 3 s to stop | all six stop cleanly, container exits 0 within the grace period ([04 §9](04-architecture.md#9-shutdown)) |
| `docker stop -t 1` | manager still exits promptly and does not corrupt state |
| Kill a supervisor with `SIGKILL` | unit is marked failed, orphans recovered by env marker, `Restart=` honoured ([03 §6.8](03-process-model.md#68-orphan-recovery)) |
| A unit's `ExecStartPre` hangs | `TimeoutStartSec` fires; the unit fails; **boot continues** (today it hangs the whole boot) |
| `systemctl` invoked as uid 65534 | access denied ([09 §3.2](09-security-model.md#32-control-socket-authentication)) |
| Stale `PIDFile=` pointing at an unrelated pid | unit fails with a specific message; the unrelated process is untouched |
| 1,000 `systemctl status` calls in a loop | no fd growth, no memory growth |
| A unit that logs 100 MB | rotation holds the directory at `LogTotalMax` |
| `daemon-reload` with a running unit whose file was deleted | no panic (A4) |
| `docker exec` running `nohup sleep 1000 &` then exiting | reaped by PID 1, no zombie |
| Crash-looping unit | `StartLimitBurst` trips; unit enters `failed`; no infinite restart |

## 6. Mechanism probes

The empirical results in [03 §4](03-process-model.md#4-empirical-results) should be a
**permanent, runnable probe suite**, not a one-off investigation — kernel and runtime
behaviour changes, and the design rests on these facts:

```
probe-subreaper       prctl(PR_SET_CHILD_SUBREAPER) succeeds; an orphan reparents to us
probe-pidfd           pidfd_open on a non-child; poll() delivers exit; pidfd_send_signal works
probe-cgroup          report whether /sys/fs/cgroup is writable (expected: no)
probe-pgid-insufficient   kill(-pgid) does NOT stop a setsid'd daemon  (regression guard
                          against anyone "simplifying" the termination ladder)
probe-proc            /proc/<pid>/stat is readable and parseable for all processes
```

Run at CI time on every supported base image, and expose them at runtime as
`systemctl show --property=Capabilities` so a user's bug report includes the answers.

## 7. CI pipeline

```
lint        gofmt, go vet, staticcheck, govulncheck
build       amd64 + arm64, CGO_ENABLED=0, reproducible (-trimpath)
L1          go test -race ./...            (race detector mandatory - defect class A9/F3)
L1-fuzz     go test -fuzz -fuzztime=60s    on the four fuzz targets
L2          go test -tags=proc ./...       (needs a real Linux kernel, not a mock)
L3/L4       matrix: {base image} x {amd64, arm64}
artefacts   binaries, deb, rpm, images
nightly     L5 soak: 8h with a churn workload; assert flat RSS and fd count
```

Coverage gate: 80% on the parser, graph, and protocol packages; the supervisor is
covered by L2 rather than by line coverage.

Additionally, the release job must **not** depend on a developer's local `docker`
invocation for any artefact — the current `.so` files are built by a manual script
(F7). Everything in a release must be reproducible from a clean checkout by CI.
