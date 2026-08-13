# docker-systemd — design for the next (stable) version

Status: **proposal / not yet implemented**
Target: replaces the v0.5.x implementation with a clean-room reimplementation
Audience: whoever implements it (the design is deliberately language-agnostic; the
language recommendation is a separate, revisable decision — see [10](10-language-choice.md))

---

## 1. What this is

`docker-systemd` is a single static binary that acts as PID 1 inside an OCI container
and behaves enough like `systemd` that a stock distro image "works like a VM": unit
files in `/lib/systemd/system` are parsed, `multi-user.target` services are started at
boot, and `systemctl` / `journalctl` / `service` / `poweroff` behave as expected.

The v0.5.x implementation works, but it is structurally fragile: process tracking rests
on an `LD_PRELOAD` shim that (as measured below) misses the majority of real
daemonisation paths; PID 1 can be crashed by a nil dereference from several ordinary
inputs; and `systemctl stop` cannot actually stop a `Type=forking` service.

This document set specifies a rebuild.

## 2. The one hard requirement

> **It must work in `docker run` with no added privileges, no added capabilities, no
> `--privileged`, no cgroup delegation, and the default seccomp/AppArmor profiles.**

Every mechanism proposed here was probed inside an unprivileged container before being
adopted. Results are in [03-process-model.md](03-process-model.md#4-empirical-results);
the summary is below.

## 3. Executive summary of the decisions

| # | Decision | Where |
|---|---|---|
| D1 | **Replace `LD_PRELOAD` fork-interposition with a per-unit supervisor process that sets `PR_SET_CHILD_SUBREAPER`.** Delete the C shim, the committed `.so` blobs and the embedded-binary machinery. | [03](03-process-model.md), [ADR-1](#adr-1) |
| D2 | **Stay in Go.** Rust buys nothing that matters here; the `fork`-without-`exec` limitation that would have favoured Rust is dissolved by the re-exec supervisor pattern. | [10](10-language-choice.md) |
| D3 | **Stop running unit commands through `/bin/bash -c`.** Implement systemd's own command lexer and `execve` directly. This is both a correctness fix and a *tracking* fix — the shell was an extra process layer that obscured the real main PID. | [05](05-unit-semantics.md#4-command-lines) |
| D4 | **Implement `sd_notify`.** `Type=notify` is currently mis-handled as `forking`; a 150-line datagram listener makes readiness and `MAINPID` exact instead of guessed. | [03](03-process-model.md#7-main-pid-determination) |
| D5 | **Signal *tree members individually*, not process groups.** Measured: `kill(-pgid)` does not stop a daemonised service, because daemonising means `setsid()`. | [03](03-process-model.md#8-the-termination-ladder) |
| D6 | **cgroup v2 as an auto-detected optional accelerator**, never a requirement. Probed at boot; unavailable under plain `docker run`. | [03](03-process-model.md#5-backend-selection) |
| D7 | **Move all runtime state out of `/tmp` into `/run/docker-systemd/` with mode 0700**, and authenticate the control socket with `SO_PEERCRED`. The current world-writable socket in `/tmp` lets any container user stop services and power off the container. | [09](09-security-model.md) |
| D8 | **Length-prefixed, versioned control protocol** replacing the current single-`read()`, 64 KiB, `0x00`-terminated framing. | [08](08-control-protocol.md) |
| D9 | **A real test suite**, including a container-based conformance matrix that runs real daemonising services. Today there are zero automated tests. | [11](11-testing.md) |

## 4. The evidence that drove D1

Measured inside an unprivileged container (kernel 6.17, glibc), using the **current**
`forkpreload/preload.c` compiled as-is and its own `/var/log/pidtrack.log` sink:

| How the process created a child | events seen by the current shim |
|---|---|
| `fork(3)` | 1 ✅ |
| `system(3)` | 1 ✅ |
| `vfork(3)` | **0 ❌** |
| `posix_spawn(3)` | **0 ❌** |
| **`daemon(3)`** — the canonical daemonisation call | **0 ❌** |
| static binary (Go/Rust/musl — no glibc PLT to interpose) | **0 ❌** |

`daemon(3)` is the exact case the shim exists to solve, and it is invisible to it,
because glibc calls its internal `__fork` alias rather than the interposable `fork`
symbol. The same probe with a subreaper supervisor:

```
supervisor pid 742 subreaper=on
direct child (pgid leader): 747
  REAPED pid=747 status=0            <- the daemon(3) intermediate
live tree rooted at supervisor: [748]
   pid=748 comm=gend ppid=742        <- the escaped daemon, correctly attributed
```

The escaped daemon reparents to **the supervisor**, not to PID 1, and a `/proc` ppid
walk rooted at the supervisor recovers the exact tree. A subreaper's subtree is
*closed*: nothing inside it can escape upward. That is a stronger guarantee than
interposition can ever give, it needs no privileges, no capabilities, no C, and it
imposes zero cost on processes that are not part of a managed unit.

Also measured, and load-bearing for the design:

* `/sys/fs/cgroup` is **read-only** under plain `docker run` → cgroups cannot be the
  primary mechanism (D6).
* `kill(-pgid, SIGTERM)` **did not** stop the daemonised service — it had called
  `setsid()` into its own group (D5).
* `pidfd_open(2)` on a **non-child** works, and `poll()` on it delivers exit
  notification. `pidfd_send_signal(2)` works. Both are permitted by Docker's default
  seccomp profile → race-free waiting on, and signalling of, adopted PIDs.

## 5. Document map

| Doc | Contents |
|---|---|
| [01-current-system-review.md](01-current-system-review.md) | What v0.5.x does, how it is put together, and a feature-by-feature support matrix (supported / partial / absent) |
| [02-defect-register.md](02-defect-register.md) | Every defect found in the review, with severity, `file:line`, and the fix that the new design applies |
| [03-process-model.md](03-process-model.md) | **The core document.** Process supervision and PID tracking without privileges: options analysis, empirical results, the chosen model, algorithms, state machines, failure modes |
| [04-architecture.md](04-architecture.md) | Component decomposition, process topology, on-disk layout, boot and shutdown sequences |
| [05-unit-semantics.md](05-unit-semantics.md) | Unit file discovery, parsing, specifiers, command-line lexing, the full directive table |
| [06-cli-surface.md](06-cli-surface.md) | `systemctl` / `journalctl` / `service` / `poweroff` surface, exit codes, and the subset distro maintainer scripts actually require |
| [07-logging.md](07-logging.md) | Log record format, rotation, and journal query semantics |
| [08-control-protocol.md](08-control-protocol.md) | Wire format between clients, PID 1, and supervisors |
| [09-security-model.md](09-security-model.md) | Threat model, socket authentication, privilege handling |
| [10-language-choice.md](10-language-choice.md) | Go vs Rust, decided with reasons |
| [11-testing.md](11-testing.md) | Test strategy, fixtures, conformance matrix, CI |
| [12-implementation-plan.md](12-implementation-plan.md) | Phased clean-room plan with acceptance criteria per milestone |

## 6. Architecture decision records

### ADR-1
**Replace `LD_PRELOAD` fork interposition with per-unit subreaper supervisors.**

*Context.* Unprivileged containers have no writable cgroup hierarchy, so there is no
kernel-provided notion of "the set of processes belonging to service X". v0.5.x solves
this by injecting a shared object into every process in the container via
`/etc/ld.so.preload`, interposing `fork()` and `execve()`, and reporting parent/child
edges to PID 1 over a unix socket.

*Decision.* Drop it. Each unit is instead run under a dedicated supervisor process that
sets `PR_SET_CHILD_SUBREAPER=1`. Orphaned descendants of the unit reparent to that
supervisor rather than to PID 1, which makes unit membership exact and unforgeable.

*Consequences.*
+ Catches `daemon(3)`, `posix_spawn`, `vfork`, `clone`, static binaries, musl binaries,
  and setuid binaries — all of which the shim misses.
+ Removes: 4 committed `.so` blobs, a C toolchain, a docker-dependent cross-build
  script, `go:embed` of binaries, and the risk of bricking a committed image by
  leaving a dangling `/etc/ld.so.preload` entry.
+ Removes an unbounded, never-pruned PID-relation map in PID 1 and its PID-reuse
  aliasing bug.
+ Removes per-`exec` socket traffic to PID 1 from *every* process in the container.
− Membership is now discovered by walking `/proc` rather than by push notification;
  worst case is one `/proc` scan per unit stop plus a low-rate heartbeat. Bounded and
  measured in [03](03-process-model.md#9-cost-analysis).
− One extra resident process per running unit (~1–3 MiB RSS). Acceptable; it is what
  buys the guarantee.

*Rejected alternatives* (full analysis in [03](03-process-model.md#3-options-considered)):
netlink proc connector (needs `CAP_NET_ADMIN`), per-unit PID namespaces (needs
`CAP_SYS_ADMIN`), `ptrace` (fragile, monopolises the tracer slot, breaks in-container
debugging), `/proc` polling alone (races on short-lived processes), keeping the shim
(does not work, as measured).

### ADR-2
**Keep Go; do not port to Rust.** See [10-language-choice.md](10-language-choice.md).

### ADR-3
**PID 1 stays a thin dispatcher.** All unit-specific logic lives in the per-unit
supervisor. PID 1 owns: unit discovery/parsing, the dependency graph, the control
socket, the reaper of last resort, and shutdown orchestration. It never blocks on a
service. A crash in unit handling can no longer kill the container.
