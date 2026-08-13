# Attic — the `LD_PRELOAD` mechanism and the measurements that retired it

This directory preserves the reasoning behind [ADR-1](../README.md#adr-1) so that the
decision can be re-examined if the facts change.

The probes below are the sources for the results quoted in
[03 §4](../03-process-model.md#4-empirical-results). They should be maintained as a
runnable suite ([11 §6](../11-testing.md#6-mechanism-probes)) rather than left here as
prose, but they are recorded here in the form in which the measurements were originally
taken.

Environment: unprivileged container, kernel 6.17, glibc, default Docker seccomp,
`docker run` with no added capabilities.

---

## 1. Coverage of the existing shim

`forkpreload/preload.c` compiled unmodified as
`gcc -fPIC -shared -o fork.so preload.c -ldl`, with `/var/log/pidtrack.log` created so
the shim's file sink is active, then one event-generator run per child-creation path.

```c
/* gen.c — one child-creation path per invocation */
#define _GNU_SOURCE
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <unistd.h>
#include <spawn.h>
#include <sys/wait.h>
extern char **environ;
int main(int argc, char **argv) {
    char *w = argv[1];
    if (!strcmp(w, "fork"))        { pid_t p = fork(); if (p == 0) _exit(0); waitpid(p, 0, 0); }
    else if (!strcmp(w, "vfork"))  { pid_t p = vfork(); if (p == 0) _exit(0); waitpid(p, 0, 0); }
    else if (!strcmp(w, "posix_spawn")) {
        pid_t p; char *const a[] = {"/bin/true", 0};
        posix_spawn(&p, "/bin/true", 0, 0, a, environ); waitpid(p, 0, 0);
    }
    else if (!strcmp(w, "system")) { system("/bin/true"); }
    else if (!strcmp(w, "daemon")) { if (daemon(0, 0) == 0) { usleep(100000); _exit(0); } }
    return 0;
}
```

```sh
for w in fork vfork posix_spawn system daemon; do
  : > /var/log/pidtrack.log
  LD_PRELOAD=./fork.so ./gen $w >/dev/null 2>&1; sleep 0.3
  printf "%-14s events=%s\n" "$w" "$(grep -c . /var/log/pidtrack.log)"
done
```

Result:

```
fork           events=1
vfork          events=0
posix_spawn    events=0
system         events=1
daemon         events=0
go static      events=0
```

**`daemon(3)` — the canonical daemonisation call, and the exact case the shim exists to
catch — produces zero events.** glibc's `daemon()` reaches `fork` through the internal
`__fork` alias, which does not traverse the PLT and therefore cannot be interposed. The
same applies to `posix_spawn`'s internal `clone`. `vfork` is a distinct symbol that the
shim does not wrap. A statically linked binary has no dynamic loader at all.

Independently of the measurement, `/etc/ld.so.preload` is a glibc facility: musl ignores
it, so Alpine-based images are entirely untracked; and glibc rejects preload paths
containing a slash for `AT_SECURE` binaries, so setuid daemons are untracked.

## 2. Subreaper coverage of the same cases

```go
// sup.go — supervisor probe (abridged; full version in test/daemons/)
const PR_SET_CHILD_SUBREAPER = 36
prctl(PR_SET_CHILD_SUBREAPER, 1)
cmd := exec.Command(target)                    // the daemonising program
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
cmd.Start()
go func() { for { wpid, _ := syscall.Wait4(-1, &ws, 0, nil); log(wpid) } }()
// treeOf(pid) walks /proc collecting every pid whose ppid-chain reaches pid
```

Against the classic double-fork daemon:

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
   pid=538 comm=daemonize  ppid=531
```

Against `daemon(3)` — the case the shim scores 0 on:

```
supervisor pid 742 subreaper=on
direct child (pgid leader): 747
  REAPED pid=747 status=0
live tree rooted at supervisor: [748]
   pid=748 comm=gend ppid=742
```

In both cases the escaped process reparents to the **supervisor**, not to PID 1, and a
single `/proc` pass recovers the exact tree.

## 3. Process-group kill is insufficient

From the same run:

```
--- killing whole pgid 747 ---
tree after pgid TERM: [748]        <- survived
--- killing survivors individually ---
tree after individual KILL: []
```

Daemonising means calling `setsid()`, which creates a new session *and* a new process
group. Any termination strategy built on `kill(-pgid, …)` alone will leave real daemons
running. This is why the ladder in
[03 §8](../03-process-model.md#8-the-termination-ladder) enumerates and signals members
individually, and why `probe-pgid-insufficient` is a permanent regression guard.

## 4. `pidfd` availability

```
grandchild (non-child of us): 583
pidfd_open OK fd=5
poll returned n=1 revents=0x1 after 2s     <- exit notification for a NON-child
pidfd_send_signal(SIGKILL) err: <nil>
```

`pidfd_open(2)` (Linux ≥ 5.3) and `pidfd_send_signal(2)` (≥ 5.1) are permitted by
Docker's default seccomp profile, giving race-free waiting on and signalling of adopted
PIDs without a `(pid, starttime)` re-validation dance.

## 5. cgroups

```
$ mkdir /sys/fs/cgroup/probe-test
mkdir: cannot create directory '/sys/fs/cgroup/probe-test': Read-only file system
```

Confirms that cgroup-based membership cannot be the primary mechanism under plain
`docker run`, and must remain an auto-detected optional accelerator
([03 §5](../03-process-model.md#5-backend-selection)).

## 6. If the shim is ever revived

It would have to be as an *additional* signal on top of subreaper tracking, not a
replacement, and it would need to fix all of the following, none of which the current
version does:

* interpose `vfork`, `clone`, `clone3`, `posix_spawn`, and `daemon` — and accept that
  glibc-internal callers remain invisible regardless;
* prune the relation graph on process exit, and key entries on `(pid, starttime)` to
  survive PID reuse;
* batch reports over a persistent connection instead of one `connect()` per `exec`;
* authenticate the socket with `SO_PEERCRED` and place it outside `/tmp`;
* install the `.so` with `O_TMPFILE` + `linkat` so a partially written library can never
  be loaded, and remove the `/etc/ld.so.preload` entry on clean shutdown;
* drop the `/var/log/pidtrack.log` sink entirely.

Given that the resulting mechanism would still be strictly less complete than a
subreaper — which cannot be escaped at all — this is recorded as "how to do it
properly", not as a recommendation.
