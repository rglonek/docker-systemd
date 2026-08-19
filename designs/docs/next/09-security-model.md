# 09 — Security model

The container boundary is the primary security boundary, and this project does not
claim to be a sandbox. But it does run as root as PID 1 and it does accept commands
over a socket, so the *inside* of the container has a meaningful trust structure that
the current implementation gets wrong.

---

## 1. Threat model

| Actor | Assumed capability | In scope? |
|---|---|---|
| The image author | full control of the image, unit files, and the manager's flags | trusted |
| An operator with `docker exec` as root | full control | trusted |
| **A service running as `User=nobody` / any non-root uid inside the container** | can connect to unix sockets, read world-readable files, write world-writable directories | **untrusted — the boundary that matters** |
| A user with `docker exec -u 1000` | as above | **untrusted** |
| A remote attacker who has compromised a service | as above | **untrusted** |
| The container host | full control | out of scope |

The interesting question is therefore: **can a process that is not root inside the
container obtain root inside the container by talking to init?** Today, yes, trivially.

## 2. Findings against the current implementation

### 2.1 World-accessible control socket (E1) — critical

`/tmp/docker-systemd.sock` is created with the process umask in a world-writable,
sticky directory. Any uid in the container can connect and issue any verb:

```
$ id -u
65534
$ systemctl start any-root-service
$ systemctl poweroff
```

Since units run as root by default, this is a complete escalation from any
unprivileged service to root-in-container, and a trivial denial of service. A service
that carefully drops to `User=www-data` gains nothing, because it can ask init to start
a root unit for it. It can also `stop` a security-relevant unit, or `mask` it so it
never comes back.

### 2.2 World-accessible pidtrack socket (E2)

`/tmp/docker-systemd-pidtrack.sock` accepts arbitrary `parent:child:infant` triples
from anyone. Today the injected edges only corrupt `systemctl status` output and cause
spurious waits — but once the tracked pids are actually signalled (which is the whole
point of fixing B1), an attacker who can inject edges chooses which processes init
`SIGKILL`s.

### 2.3 `PIDFile=` is unvalidated (C13/E4)

The pid is extracted by keeping every ASCII digit found anywhere in the file, and is
never checked. A unit running as an unprivileged user writes any pid it likes into its
own pid file; init adopts that process as the unit's main process and will eventually
`SIGTERM`/`SIGKILL` it.

### 2.4 `/var/log/pidtrack.log` (E3)

The preload shim appends to this path **if it exists**. Any user can create it, after
which every process in the container writes to it forever, unrotated — an
unauthenticated disk-fill and a leak of process-tree structure.

### 2.5 `/etc/ld.so.preload` (F6)

Writing this file makes every dynamically linked process in the container load a
library from `/usr/local/lib/fork.so`. If that path is writable by a non-root user in
some image, it is arbitrary code execution as every user in the container, including
root. `/usr/local/lib` is `root:root 0755` in the base images used here, so this is
latent rather than live — but it is a large amount of standing risk for a mechanism
that [03 §2](03-process-model.md#2-what-the-current-implementation-does-and-why-it-does-not-work)
shows does not work.

## 3. The new model

### 3.1 Runtime state

```
/run/docker-systemd/            0711 root:root
/run/docker-systemd/control.sock  0600 root:root
/run/docker-systemd/units/        0700 root:root
/run/docker-systemd/notify/       0711 root:root
/run/docker-systemd/notify/<unit>.sock  0666 <unit User=>:<unit Group=>
```

`/run` is `tmpfs`-or-directory, root-owned, and not world-writable in any of the
supported base images (unlike `/tmp`). The directory modes remove the entire class of
attacks in §2.1–§2.4 for non-root users: neither directory can be listed or written
to, the control socket is `0600`, and the per-unit state files are `0600` inside a
`0700` directory.

The runtime and notify directories are `0711` — searchable but not listable — rather
than `0700`. An earlier revision of this document specified `0700` and justified the
notify socket's `0666` mode with "it is protected by the `0700` parent directory, so
only processes that init has told the path to can reach it". That reasoning is wrong:
the kernel checks the search bit on **every** component of a path regardless of how
the process learned the path, so a `0700` parent made `$NOTIFY_SOCKET` undeliverable
for exactly the units whose `User=` the `0666` mode existed to accommodate. A
`Type=notify` unit running as a non-root user could never send `READY=1` and sat in
`activating` until `TimeoutStartSec` — forever for `mysql.service`, which combines
`User=mysql` with `TimeoutSec=infinity`.

Path secrecy is not what defends the notify socket. The supervisor **binds a distinct
socket per unit** and enables `SO_PASSCRED`, so a message's sender pid comes from
`SCM_CREDENTIALS`, not from the message body; every assignment that changes unit
state — `READY=`, `MAINPID=`, `STATUS=`, `STOPPING=`, `RELOADING=` — is discarded
unless the kernel-supplied sender is a live member of that unit's process tree. The
socket is additionally chowned to the unit's own resolved credentials.

### 3.2 Control socket authentication

```
on accept:
    creds = getsockopt(SO_PEERCRED)      # {pid, uid, gid} - kernel-supplied, unforgeable
    if creds.uid != 0 and creds.uid not in AllowedUids:
        reply RESULT{exit=1, "Access denied: only uid 0 may control the manager"}
        close
    audit-log the (pid, uid, verb, units) tuple at info level
```

* Default policy: **uid 0 only**.
* `--control-allow-uid=<uid>[,<uid>...]` and `--control-allow-gid=` widen it for images
  that intentionally run tooling as a non-root user.
* A future refinement, explicitly out of scope for v1: per-verb policy (allow
  `is-active`/`status` to everyone, mutations to root only). Note it in the design so
  the dispatch table is built with a `required_privilege` column from day one.

`SO_PEERCRED` is the right primitive: it is filled in by the kernel at `connect(2)`
time from the peer's credentials and cannot be spoofed by the client.

### 3.3 `PIDFile=` validation

A pid read from a pid file is accepted **only** if it is a member of the unit's process
tree as computed by the supervisor ([03 §7](03-process-model.md#7-main-pid-determination)).
Otherwise the unit fails with a specific, greppable message. This closes §2.3 and also
catches the far more common benign case of a stale pid file left by an unclean stop.

### 3.4 Log files

`/var/log/services/` becomes `0750 root:adm`; log files `0640 root:adm`. Services
frequently echo credentials on startup; there is no reason for every uid in the
container to read them. `--log-mode=` overrides for images that need it.

### 3.5 Removal of `LD_PRELOAD`

Removing the shim (ADR-1) deletes §2.2, §2.4 and §2.5 outright, along with the standing
risk of an image being committed with a dangling `/etc/ld.so.preload` entry.

If a future version reintroduces an optional preload backend, it must: live in
`/run/docker-systemd/lib/` (0700 parent), be written with `O_TMPFILE` + `linkat` to
avoid a partially written `.so` ever being loadable, authenticate its socket with
`SO_PEERCRED`, and remove its `/etc/ld.so.preload` entry on clean shutdown.

### 3.6 Privilege handling in the supervisor

* The supervisor runs as root; the credential drop happens in the **child**, after
  `fork`, before `exec`, so a failure to drop privileges is a failure to start the unit,
  never a silent run-as-root. Order: `setgroups` → `setgid` → `setuid`, each checked;
  any failure means `_exit(217)` and the unit fails with `217/USER`.
  The current code's "skip the credential if uid == 0" shortcut (C6) is precisely the
  shape of bug that produces silent privilege retention.
* `NoNewPrivileges=yes` is honoured via `prctl(PR_SET_NO_NEW_PRIVS, 1)` — free, and it
  meaningfully reduces the blast radius of a setuid binary invoked by a unit.
* Environment files are read **as root before dropping privileges**, so a unit cannot
  point `EnvironmentFile=` at a file it could not otherwise read and have the manager
  leak it... but note it *can* — the manager reads as root by design, same as systemd.
  Document this: `EnvironmentFile=` is an image-author-controlled input, not
  service-controlled.

### 3.7 What is deliberately not defended

* An image author who writes a malicious unit file. They already control the image.
* Root inside the container. It is root.
* Container escape. That is the runtime's job; nothing here weakens it, and removing
  `/etc/ld.so.preload` manipulation strictly strengthens the image's integrity story.

## 4. Hardening checklist for the implementation

- [ ] All runtime state under `/run/docker-systemd`, directory created `0711` (`0700`
      for the per-unit state directory, which nothing but the manager reads) **before**
      any socket is bound (create the directory, `chmod`, *then* bind — do not rely on
      umask).
- [ ] `SO_PEERCRED` check on every control connection, before parsing the request.
- [ ] `SO_PASSCRED` + `SCM_CREDENTIALS` on notify sockets; never trust `MAINPID=` alone,
      and never trust a state-changing assignment from a live pid outside the unit's
      tree.
- [ ] Every pid obtained from outside the supervisor's own `fork` is validated for tree
      membership before it is waited on or signalled.
- [ ] Signals delivered through `pidfd` where available, to eliminate PID-reuse
      mis-targeting.
- [ ] No `world-writable` path is ever used for control or state.
- [ ] Unit log files `0640 root:adm`.
- [ ] The audit log records `(uid, pid, verb, units, result)` for every mutating verb.
- [ ] Reject `--control-allow-uid=` values that are not integers, and log the resulting
      policy at boot so it is visible in `docker logs`.
