# 06 — CLI surface

The measure of success is not "looks like systemctl" but **"stock distro packages
install, enable and start correctly, unattended, inside `docker build`"**. That is a
much sharper target, and it dictates most of what follows.

---

## 1. Binary multiplexing

One binary, dispatched on `argv[0]` (basename), with `getpid() == 1` forcing manager
mode:

| name | mode |
|---|---|
| `init`, `systemd`, `init-docker-systemd` | manager |
| *anything*, when pid == 1 | manager |
| `systemctl` | control client |
| `journalctl` | log client |
| `service` | SysV-ish shim → control client |
| `poweroff`, `halt`, `reboot`, `shutdown`, `telinit`, `runlevel` | control client |
| `systemd-notify` | notify helper (new — lets shell-based units report readiness) |

Self-installation symlinks the above names into the first writable directory among
`/usr/local/sbin`, `/usr/sbin`, `/sbin`, `/usr/bin`, `/bin`. Changes from today:

* skip and log if the target is already our symlink (idempotent);
* rename an existing real binary to `<name>.dist` **once**, and never overwrite an
  existing `.dist`;
* failure to install one name is a warning, not `log.Fatal` (F8);
* `--no-install` skips the whole step;
* record what was installed in `/run/docker-systemd/installed` so `systemctl` can report
  it.

## 2. `systemctl`

### 2.1 Verbs

**Unit lifecycle** — `start`, `stop`, `restart`, `try-restart`, `reload`,
`reload-or-restart`, `try-reload-or-restart`, `kill --signal=`, `isolate`.

**Unit state** — `status`, `is-active`, `is-failed`, `is-enabled`, `show`, `cat`,
`list-units`, `list-unit-files`, `list-dependencies`, `reset-failed`.

**Enablement** — `enable [--now]`, `disable [--now]`, `reenable`, `preset`,
`preset-all`, `mask [--now]`, `unmask`, `link`, `revert`, `add-wants`, `add-requires`,
`edit --full` (writes a drop-in or fragment under `/etc/systemd/system`).

**Manager** — `daemon-reload`, `daemon-reexec`, `show-environment`, `set-environment`,
`unset-environment`, `import-environment`, `poweroff`, `halt`, `reboot`,
`default`, `rescue`, `emergency`, `--version`.

**Retained project extensions** — `list` (alias of `list-units`),
`create-instance`, `delete-instance` (now enable-symlink only, B13).

### 2.2 Global options

`--no-pager`, `--no-legend`, `--no-block`, `--quiet`/`-q`, `--all`/`-a`,
`--type=`/`-t`, `--state=`, `--property=`/`-p`, `--value`, `--full`/`-l`,
`--lines=`/`-n`, `--output=`/`-o`, `--plain`, `--now`, `--force`/`-f`,
`--system` (accepted, the only mode), `--user` (rejected with a clear message),
`--root=`, `--signal=`/`-s`, `--kill-who=`.

Options that maintainer scripts pass and that must at minimum be **accepted and
ignored** rather than causing a parse failure: `--system`, `--no-block`, `--no-reload`,
`--global`, `--runtime`, `--quiet`, `--no-ask-password`, `--no-pager`.

> The current `go-flags` parser returns exit **0** for an unknown command (D6), which
> means a maintainer script's `systemctl --no-block start foo` looks like it succeeded
> while doing nothing. Unknown options must be a hard, visible failure — except for the
> accept-and-ignore list above.

### 2.3 Output formats

Match systemd closely enough for `grep`/`awk` in maintainer scripts.

`systemctl status nginx`:

```
● nginx.service - A high performance web server
     Loaded: loaded (/lib/systemd/system/nginx.service; enabled; preset: enabled)
     Active: active (running) since Tue 2026-08-11 09:14:02 UTC; 2h 3min ago
   Main PID: 431 (nginx)
      Tasks: 3
     Memory: 12.4M
        CGroup: (unavailable: unprivileged container; using subreaper tracking)
             ├─431 nginx: master process /usr/sbin/nginx -g daemon off;
             ├─432 nginx: worker process
             └─433 nginx: worker process

Aug 11 09:14:02 host nginx[431]: starting
```

The process tree comes from the supervisor's `enumerate_tree`. Exit status 0 if the
unit is active, 3 if inactive/failed, 4 if unknown — that mapping is scripted against.

`systemctl list-units --type=service --no-legend --plain` must emit the five-column
`UNIT LOAD ACTIVE SUB DESCRIPTION` form.

`systemctl show nginx -p MainPID --value` must print just the number. The property set
must include at minimum: `Id`, `Names`, `LoadState`, `ActiveState`, `SubState`,
`UnitFileState`, `MainPID`, `ExecMainPID`, `ExecMainStatus`, `ExecMainStartTimestamp`,
`Result`, `Description`, `FragmentPath`, `DropInPaths`, `Type`, `Restart`,
`NRestarts`, `Requires`, `Wants`, `After`, `Before`, `ConditionResult`,
`LoadError`, `StatusText`, `InvocationID`.

`systemctl cat nginx` prints the fragment and every drop-in with `# <path>` headers —
cheap to implement and the fastest way for a user to see what was actually loaded.

## 3. `journalctl`

Options: `-u`/`--unit` (now **optional** — default is all units, interleaved by
timestamp), `-n`/`--lines` (default 10 with `-f`, all otherwise), `-f`/`--follow`,
`-S`/`--since`, `-U`/`--until`, `-b`/`--boot`, `-p`/`--priority`, `-o`/`--output`
(`short`, `short-iso`, `short-precise`, `cat`, `json`, `json-pretty`), `-x` (accepted,
no catalog), `-k` (empty output, exit 0), `--no-pager`, `--no-hostname`, `-q`,
`--disk-usage`, `--vacuum-size=`, `--vacuum-time=`, `-r`/`--reverse`, `-e`.

Fixes required (B5, B6, B7): `-n` must tail the **file** and not stdin; time filters
must apply when only one bound is given; and the log format must actually carry
timestamps. Relative time expressions (`--since=-1h`, `--since=yesterday`,
`--since=today`) should be accepted — they are what people type.

Implement `-f` and `-n` natively (read backwards from the end for `-n`; `inotify` +
read-append for `-f`). Shelling out to `tail` is both a bug source and a dependency on
coreutils.

## 4. Exit codes

| Code | Meaning |
|---|---|
| 0 | success; for `is-active`/`is-enabled`/`is-failed`, the predicate holds |
| 1 | generic failure / predicate false |
| 2 | invalid arguments |
| 3 | unit is inactive (`is-active`, `status`) |
| 4 | no such unit (`status`, `show`) |
| 5 | unit not loaded / operation not applicable |

`systemctl start` of a masked unit exits 1 with
`Failed to start x.service: Unit x.service is masked.`
`systemctl enable` of a unit with no `[Install]` section exits 1 with
`The unit files have no installation config`, matching systemd — several maintainer
scripts branch on that exact behaviour.

## 5. Making maintainer scripts work

This is the practical acceptance criterion for the CLI, and the current version fails
most of it silently.

| Requirement | Why | Status today |
|---|---|---|
| `/run/systemd/system` exists as a directory | `deb-systemd-helper`, `deb-systemd-invoke`, RPM `%systemd_post`, and `systemd-detect-virt`-adjacent checks all gate on it. Without it, packages **skip unit registration entirely** | **missing** (B19) |
| `systemctl is-enabled <unit>` with correct exit codes | `deb-systemd-invoke` decides whether to start | missing |
| `systemctl --quiet is-active <unit>` | postrm/prerm decide whether to stop | missing |
| `systemctl daemon-reload` returns 0 | called by every postinst | present |
| `systemctl preset <unit>` | RPM `%systemd_post` | missing |
| `systemctl try-restart <unit>` | `%systemd_postun_with_restart` | missing |
| `systemctl list-unit-files --type=service` | `deb-systemd-helper` enumerates | missing |
| enable creates a **symlink** in the right `.wants` dir | tools inspect the link target | writes a file containing `OK` (B18) |
| `systemd-detect-virt` reports `docker` | units with `ConditionVirtualization=!container` must skip | binary absent — ship it as another `argv[0]` alias |
| `systemctl` exits non-zero on unknown verbs | otherwise silent no-ops | exits 0 (D6) |

Add a dedicated conformance test that installs a set of real packages
(`nginx`, `postfix`, `mariadb-server`, `cron`, `openssh-server`, `redis`) in
`docker build` and asserts each ends up enabled and startable
([11 §4](11-testing.md#4-conformance-matrix)).

## 6. `service` shim

```
service <name> <verb> [args]   ->  systemctl <verb> <name>
service <name> status          ->  systemctl status <name>   (exit codes preserved)
service --status-all           ->  list units with a [ + ] / [ - ] prefix
service <name> reload|force-reload|try-restart -> mapped
```

The current implementation swaps `argv[1]`/`argv[2]` only when `len(argv) > 2` (C15),
so `service --status-all` and `service foo` are mishandled. Parse properly.

## 7. `systemd-notify` helper

New. Lets shell-implemented `Type=notify` units work:

```
systemd-notify --ready [--status="text"] [--pid=<pid>] [KEY=VALUE...]
```

Writes to `$NOTIFY_SOCKET`. Twenty lines, and it makes `Type=notify` usable from units
that are shell scripts.
