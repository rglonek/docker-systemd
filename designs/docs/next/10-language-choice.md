# 10 — Language choice: Go vs Rust

**Decision: stay in Go. Delete the C.**

The question was put as "keep Go + LD_PRELOAD, or move to Rust". Those two axes are
independent, and the answer differs on each:

* the **C `LD_PRELOAD` shim goes**, because it does not work
  ([03 §2](03-process-model.md#2-what-the-current-implementation-does-and-why-it-does-not-work));
* the **Go** stays, because the reason one would have reached for Rust here dissolves
  once the shim is gone.

---

## 1. What the language actually has to do

| Requirement | Difficulty |
|---|---|
| Single static binary, runs on glibc *and* musl images, amd64 + arm64 | decisive |
| `prctl(PR_SET_CHILD_SUBREAPER)`, `waitid`, `pidfd_open/send_signal`, `signalfd`/`epoll`, `setsid`, `setpgid`, `setgroups/setgid/setuid`, `setrlimit`, `SO_PEERCRED`, `SCM_CREDENTIALS`, `inotify` | routine in both |
| Run arbitrary code **between `fork` and `exec`** | the one real discriminator — see §3 |
| Unit-file parsing, dependency graph, job scheduling, CLI, log handling — ~80% of the code | routine in both |
| One process per active unit, small RSS | slight edge to Rust |
| Contributor accessibility, and the fact that 4,100 lines of working Go already exist | edge to Go |

## 2. Static linking is the decisive constraint

This binary is copied into arbitrary base images — Ubuntu 20.04 through 24.04, Debian
9–12, Rocky 8/9, CentOS Stream 9, and users will try Alpine. It must be one file with
zero runtime dependencies.

* **Go** with `CGO_ENABLED=0` produces a genuinely static binary with no libc
  dependency at all. It runs unchanged on glibc and musl images. This is already how
  the project ships, and it works.
* **Rust** defaults to dynamically linking the host glibc. A binary built on Debian 12
  will not start on Rocky 8 (`GLIBC_2.34 not found`) — the same class of problem the
  project already hit and worked around by building the `.so` files on Rocky 8
  (commit `b472df8`, "compile using rocky 8 for glibc compatibility"). The fix is
  `--target x86_64-unknown-linux-musl`, which works, but brings musl's allocator and
  DNS resolver, requires a cross-toolchain per architecture in CI, and is a step
  backwards in build simplicity from `GOOS=linux GOARCH=arm64 go build`.

This alone is not disqualifying for Rust, but it means Rust's build story here is
*worse*, not better, and the project has already been bitten by exactly this.

## 3. The `fork`-without-`exec` question

This is the strongest technical argument for Rust, and it is worth stating fairly
before rejecting it.

A Go program is unavoidably multi-threaded (the runtime starts threads before `main`).
After `fork(2)` in a multi-threaded process, only async-signal-safe operations are
legal in the child — no allocation, no mutex acquisition, no runtime scheduling. Go
therefore provides **no supported hook to run arbitrary code in the child between
`fork` and `exec`**. Rust's `CommandExt::pre_exec` provides exactly that, and Rust can
call `fork(2)` directly (`unsafe`, but expressible).

For a service manager this looks fatal, because "set the subreaper flag, then supervise"
sounds like it needs a `fork` with no `exec`.

**It does not**, for two reasons:

1. `PR_SET_CHILD_SUBREAPER` is set by the supervisor **on itself, after it starts** —
   not in a forked child. The supervisor is `/proc/self/exe --supervise`, a normal
   `exec`, and it calls `prctl` as its first statement. Nothing needs to happen between
   `fork` and `exec` for this to work. (This is the re-exec pattern that `runc`,
   `containerd-shim` and `docker` itself all use, for precisely this reason.)
2. The per-child setup that *does* need to happen between `fork` and `exec` —
   `setsid`, `setpgid`, credentials, rlimits, `chdir`, fd redirection — is expressible
   through Go's `syscall.SysProcAttr`, which the runtime performs in the child using
   only raw syscalls in hand-written assembly-safe code:

   | need | `SysProcAttr` field |
   |---|---|
   | `setsid` | `Setsid` |
   | `setpgid` | `Setpgid`, `Pgid` |
   | uid/gid/supplementary groups | `Credential{Uid, Gid, Groups, NoSetGroups}` |
   | `PR_SET_PDEATHSIG` | `Pdeathsig` |
   | `chdir` | `exec.Cmd.Dir` |
   | fd table | `exec.Cmd.Stdin/Stdout/Stderr/ExtraFiles` |
   | `CLONE_*` | `Cloneflags`, `Unshareflags` |
   | ambient capabilities | `AmbientCaps` |

   The gaps are `setrlimit`, `umask`, `nice`, `OOMScoreAdjust` and
   `PR_SET_NO_NEW_PRIVS`, which `SysProcAttr` does not cover. Standard, well-trodden
   solution: a **tiny re-exec trampoline** — the supervisor spawns
   `/proc/self/exe --exec-helper` with the remaining settings on `fd 3`; the helper is
   a single-threaded, freshly-`exec`'d process that applies them with plain syscalls
   and then `execve`s the real target. Cost: one extra `exec` per unit start (~1 ms),
   no extra artefact, and the helper is ~100 lines.

   (`PR_SET_NO_NEW_PRIVS` and `setrlimit` are inherited across `execve`, so applying
   them in the trampoline is semantically identical to applying them in the child.)

So the discriminator disappears. What remains is a preference, not a constraint.

## 4. Comparison on the remaining axes

| Axis | Go | Rust |
|---|---|---|
| Static, portable, cross-arch build | `GOOS/GOARCH`, one command, no cross toolchain | needs musl targets + cross toolchain per arch |
| Runs on musl images | yes, unmodified | only if built for musl |
| Binary size | ~6–8 MB | ~2–4 MB |
| Supervisor RSS (×N units) | ~2–4 MiB | ~1–2 MiB |
| Signal handling in a multi-threaded process | `signal.Notify`; correct but the runtime is opinionated | `signalfd` directly; slightly cleaner |
| Concurrency for N units + N log pipes + a control server | goroutines; a very good fit | `tokio`, or threads; also fine, more ceremony |
| Freedom from data races (the A9/F3 defect class) | `-race` in CI; discipline required | compiler-enforced |
| Panics in PID 1 | possible; contained by `recover()` + ADR-3 | panics also possible; `abort` on panic in a supervisor is fine |
| GC pauses in an init system | irrelevant at this scale (a few hundred objects, no latency SLO) | n/a |
| Existing code to draw on | 4,100 lines, including a working unit parser, duration parser, CLI, and protocol | none |
| Contributor pool for a container-tooling project | large | smaller |
| Time to a correct v1 | shorter | longer, by roughly the whole rewrite |

The one axis where Rust would genuinely have paid — compiler-enforced elimination of the
A9/F3 data-race family — is largely addressed by the architectural change in
[04 §3](04-architecture.md#3-concurrency-model): one actor per unit, no shared mutable
state, dependencies by name rather than pointer. That design removes the races by
construction in either language, and `go test -race` in CI keeps them out.

## 5. Decision

**Go**, with:

* `CGO_ENABLED=0`, `-trimpath`, `-ldflags="-s -w"`, `GOOS=linux`, `GOARCH ∈ {amd64, arm64}`;
* `golang.org/x/sys/unix` for `prctl`, `pidfd_*`, `waitid`, `signalfd`, `setrlimit`,
  `SO_PEERCRED`, `SCM_CREDENTIALS`;
* the `--supervise` and `--exec-helper` re-exec modes described above;
* **no cgo, no C, no committed binary artefacts**;
* **no UPX** (F10): every supervisor is a re-exec of the image and would decompress its
  own private copy.

**Delete**: `forkpreload/` (C source, Makefile, `dockerbuild.sh`, four `.so` blobs),
`systemd/fork.go` (the `go:embed` of those blobs), `systemd/pidtracker/`, and the
`.so` copies under `systemd/`. Preserve `forkpreload/preload.c` and this analysis under
`designs/docs/next/attic/` for the historical record.

Minimum Go version: whatever is current at implementation time, pinned in `go.mod` with
a `toolchain` directive so CI cannot silently build with a different one (F11).

## 6. Conditions that would reverse this

Stated explicitly so the decision is falsifiable:

* If the supervisor-per-unit RSS turns out to matter for a real user running 50+ units
  in a memory-capped container, the mitigation is a threaded single-process supervisor
  host — at which point Rust's per-thread cost and lack of a GC become relevant, and
  the calculus changes.
* If a future requirement needs genuine `fork`-without-`exec` (e.g. snapshotting the
  manager's state into a child), Go cannot do it safely and Rust can.
* If the project ever needs to ship a `.so` again (an optional preload backend), that
  component should be C or a Rust `cdylib`, built in CI — never committed.
