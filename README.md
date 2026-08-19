# docker-systemd

A single static binary that acts as PID 1 inside an OCI container and behaves enough like `systemd` that a stock distro image "works like a VM": unit files in `/lib/systemd/system` are parsed, `multi-user.target` services are started at boot, and `systemctl` / `journalctl` / `service` / `poweroff` behave as expected.

It works under plain `docker run` — **no added privileges, no added capabilities, no `--privileged`, no cgroup delegation, and the default seccomp/AppArmor profiles**. That is the hard requirement the whole design is built around; see [designs/docs/next/](designs/docs/next/).

## Quickstart: prebuilt images

```
docker run -itd robertglonek/ubuntu:24.04
docker run -itd robertglonek/ubuntu:22.04
docker run -itd robertglonek/ubuntu:20.04

docker run -itd robertglonek/debian:12
docker run -itd robertglonek/debian:11
docker run -itd robertglonek/debian:10

docker run -itd robertglonek/rockylinux:9
docker run -itd robertglonek/rockylinux:8

docker run -itd robertglonek/centos:stream9
```

See [QUICKSTART.md](QUICKSTART.md) for a getting started guide.

## How process tracking works

Unprivileged containers have no writable cgroup hierarchy, so there is no kernel-provided notion of "the set of processes belonging to service X". A naive "the unit is the process I spawned" fails for the single most common pattern in real service software: **daemonisation**, where the process forks, the parent exits, and the survivor reparents away.

Each unit is therefore run under a dedicated **supervisor process** that sets `PR_SET_CHILD_SUBREAPER=1`. Orphaned descendants of the unit reparent to that supervisor rather than to PID 1, which makes unit membership exact and unforgeable:

```
PID 1  init
  ├── supervisor(nginx)      <- PR_SET_CHILD_SUBREAPER=1
  │     └── nginx master     <- reparented here when it daemonised
  │           ├── worker
  │           └── worker
  └── supervisor(cron)
        └── cron
```

This catches `daemon(3)`, `posix_spawn`, `vfork`, `clone`, static binaries, musl binaries and setuid binaries. Versions up to 0.5.x used an `LD_PRELOAD` shim instead, which — as measured in [03-process-model.md](designs/docs/next/03-process-model.md#4-empirical-results) — saw only 2 of 6 child-creation paths and missed `daemon(3)` entirely. The shim, its four committed `.so` blobs and `/etc/ld.so.preload` manipulation are all gone.

cgroup v2 is probed at boot and used as an accelerator when a writable hierarchy happens to be available (`--privileged`, Podman with delegation); it is never required.

## Binary multiplexing

The binary dispatches on `argv[0]`, with `getpid() == 1` forcing manager mode. At boot it symlinks itself into the first writable directory among `/usr/local/sbin`, `/usr/sbin`, `/sbin`, `/usr/bin`, `/bin` under these names:

| Name | Mode |
| --- | --- |
| `init`, `systemd`, `init-docker-systemd` | manager (PID 1) |
| `systemctl` | control client |
| `journalctl` | log client |
| `service` | SysV-ish shim |
| `poweroff`, `halt`, `reboot`, `shutdown`, `telinit`, `runlevel` | control client |
| `systemd-notify` | readiness helper for shell-implemented `Type=notify` units |
| `systemd-detect-virt` | reports `docker`, so `ConditionVirtualization=` works |

Self-installation is idempotent, preserves an existing real binary as `<name>.dist` exactly once, never overwrites an existing `.dist`, and is non-fatal per name. `--no-install` skips it entirely.

## Manager flags

| Flag | Default | Effect |
| --- | --- | --- |
| `--log-to-stderr[=UNIT,…]` | off | mirror unit logs to init's stderr (`docker logs`); optionally only the named units |
| `--no-logfile` | off | do not write `/var/log/services/*.log` (disables `journalctl`) |
| `--log-level=` | `info` | `error`, `warn`, `info`, `debug`, `trace` |
| `--default-target=` | `multi-user.target` | boot target |
| `--no-install` | off | do not symlink over the distro's `init`/`systemctl`/… |
| `--no-auto-reload` | off | do not reload unit files when the unit directories change on disk |
| `--shutdown-timeout=` | `90s` | global shutdown budget |
| `--backend=` | `auto` | `auto`, `cgroup2`, `subreaper`, `degraded` — force for testing |
| `--supervisor-heartbeat=` | `1s` | tree re-scan interval while a unit has escaped members |
| `--control-allow-uid=` / `--control-allow-gid=` | none | widen control-socket access beyond uid 0 |
| `--pass-environment=` | `all` | `all`, `none`, or a comma-separated variable list |
| `--log-size-max=` / `--log-file-count=` / `--log-total-max=` | `16M` / `3` / `128M` | log rotation |
| `--compat-tmp-socket` | off | also create the legacy world-accessible `/tmp` socket (insecure) |
| `--no-pidtrack` | *removed* | accepted and ignored with a deprecation warning for one release |

## Security

Everything runtime lives under `/run/docker-systemd/` (`0711 root:root` — searchable, so a unit that dropped to `User=` can reach its own `$NOTIFY_SOCKET`, but not listable), with the control socket at `0600`, authenticated with `SO_PEERCRED`: **only uid 0 may control the manager** by default. Versions up to 0.5.x put the socket in `/tmp` with the process umask, so any uid in the container could start a root unit, mask a security-relevant one, or power the container off.

A pid read from `PIDFile=` is accepted only if it is a member of the unit's own process tree; otherwise the unit fails with a specific, greppable message. This closes the escalation where a unit running as `User=nobody` writes root's pid into its own pid file, and also catches the far more common benign case of a stale pid file.

Unit log files are `0640 root:adm`, because services frequently echo credentials on startup.

## Supported unit directives

`.service` and `.target` only. The full support matrix is in [05-unit-semantics.md §8](designs/docs/next/05-unit-semantics.md#8-directive-support-matrix-for-the-new-version). Highlights:

* **`[Service] Type=`** — `simple`, `exec`, `forking`, `oneshot`, `idle`, `notify`, `notify-reload`. `dbus` degrades to `simple` with a warning.
* **`sd_notify`** — real `$NOTIFY_SOCKET` support with `SO_PASSCRED`: `READY=1`, `MAINPID=`, `STATUS=`, `RELOADING=1`, `STOPPING=1`, `WATCHDOG=1`.
* **Ordering** — `Before=`/`After=` are honoured: units are topologically ordered and started in parallel within a stratum. An ordering cycle is broken with a warning rather than a hang.
* **Dependencies** — `Requires=`, `Requisite=`, `Wants=`, `BindsTo=`, `PartOf=`, `Upholds=`, `Conflicts=`, `OnFailure=`, `OnSuccess=`.
* **Conditions** — the `Condition*=`/`Assert*=` families, including `ConditionVirtualization=container`, which lets many stock units correctly skip themselves.
* **Execution context** — `User=`/`Group=`/`SupplementaryGroups=` (with `$HOME`/`$USER`/`$LOGNAME`), `Limit*=` (clamped to the container's hard limits with a diagnostic rather than ignored), `UMask=`, `Nice=`, `OOMScoreAdjust=`, `NoNewPrivileges=`, `WorkingDirectory=`, and the `RuntimeDirectory=`/`StateDirectory=`/`CacheDirectory=`/`LogsDirectory=`/`ConfigurationDirectory=` family.
* **Kill behaviour** — `KillMode=`, `KillSignal=`, `FinalKillSignal=`, `SendSIGHUP=`, `SendSIGKILL=`, `TimeoutStartSec=`, `TimeoutStopSec=`.
* **Restart** — `Restart=`, `RestartSec=`, `SuccessExitStatus=`, `RestartPreventExitStatus=`, `RestartForceExitStatus=`, `StartLimitIntervalSec=`, `StartLimitBurst=`.
* **Stdio** — `StandardInput=`, `StandardOutput=`, `StandardError=` (`journal`, `inherit`, `null`, `tty`, `file:`, `append:`), `SyslogIdentifier=`.

Every rejected or clamped directive produces exactly one structured warning at load time, naming the file, line, directive and reason. `systemctl status` and `systemctl show -p LoadError` surface them, and `systemctl show --property=Capabilities` prints the selected backend, the probe results, the runtime paths and every warned-about directive — paste that into a bug report.

## Behaviour differences from systemd

1. **No cgroups under plain `docker run`.** `MemoryMax=`, `CPUQuota=`, `TasksMax=` and friends are parsed, warned about once, and ignored. Use `docker run --memory/--cpus/--pids-limit`.
2. **Sandboxing directives are ignored with a warning** — `PrivateTmp=`, `ProtectSystem=`, `SystemCallFilter=`, `CapabilityBoundingSet=` and the rest need privileges the container does not have. They warn rather than failing silently, because operators otherwise assume they took effect.
3. **Only `.service` and `.target`.** No `.socket`, `.timer`, `.path`, `.mount`.
4. **No D-Bus**, so `Type=dbus` degrades to `simple` and `BusName=` is ignored.
5. **`reboot` stops the container.** Combine with `docker run --restart=always` for the closest analogue.
6. **`journalctl -b -1` is unsupported** and says so; persistent journals across container restarts require `/var/log` to be a volume.
7. **`systemctl --user` is not supported.**
8. **Process tracking uses subreaper adoption rather than cgroup membership.** A process that a unit hands to a *helper started outside the unit* is not tracked. Everything started by the unit itself is.
9. **Unit commands do not run under a shell.** See below.
10. **Unit files are reloaded automatically when they change on disk.** See below.

## Automatic `daemon-reload`

`apt install mysql-server` — or `dnf`, or `apk`, or a `docker cp` of a hand-written unit — makes the new unit visible immediately. There is no need to remember `systemctl daemon-reload` first, and `systemctl start mysql` right after the install works.

The manager watches the unit directories (`/etc/systemd/system`, `/run/systemd/system`, `/usr/lib/systemd/system`, `/lib/systemd/system` and their `.wants`/`.requires`/`.d` subdirectories) with `inotify`. Events are coalesced, so one `apt install` that drops twelve unit files causes one reload, not twelve.

This is deliberately more than systemd does. Real systemd reloads only when asked, and relies on every package's postinst calling `systemctl daemon-reload` — which happens only for packaging built with `dh_installsystemd`/`%systemd_post`, only when the postinst decided systemd was running, and never at all for a unit file that arrived by any other route. In a container the cost of that being wrong is a service the operator cannot see, so the manager watches rather than trusting the hook.

The reload is exactly the one `systemctl daemon-reload` performs: units already running keep the configuration they were started with, and **nothing is started, stopped or enabled as a side effect** — the new unit becomes visible, not active. Use `--no-auto-reload` for the strict systemd behaviour.

## Upgrading from 0.5.x

| Change | Impact | Action |
| --- | --- | --- |
| Unit commands no longer run under `bash -c` | a unit using `&&`, `\|`, `;`, `>`, globs or `~` will fail, exactly as under real systemd | rewrite as `ExecStart=/bin/sh -c '…'`; the manager warns at load naming the unit and line |
| `RemainAfterExit=yes` now actually parses as true | units that accidentally worked because it parsed as false may report state differently | this is the correct behaviour |
| `Type=oneshot` no longer forces `RemainAfterExit` | oneshot units now go `inactive` when they finish, as in systemd | — |
| Control socket moved to `/run/docker-systemd/control.sock`, root-only | tooling that connected to `/tmp/docker-systemd.sock` as non-root breaks | `--compat-tmp-socket` for one release; `--control-allow-uid=` for legitimate cases |
| `/etc/ld.so.preload` is no longer written | an image committed from an older container may carry a dangling entry | the manager removes its own stale entry at boot |
| `--no-pidtrack` removed | the flag is now ignored | accepted with a deprecation warning for one release |
| `delete-instance` no longer deletes unit files | previously it could delete the distro's unit file | this was a bug |
| `enable` writes symlinks instead of a file containing `OK` | strictly more compatible | — |
| Log format gained timestamps | log parsers keyed on the old raw format need updating | `journalctl` reads both formats |
| `/etc/boot-time` moved to `/run/docker-systemd/boot-id` | none | the stale file is removed at boot |

## Building and testing

```
make build            # static amd64 + arm64 binaries
make lint             # gofmt, go vet
make test             # L1: unit tests with the race detector
make fuzz             # L1: the four fuzz targets, 60s each
make test-proc        # L2: real daemonising services against a real kernel
make conformance      # L3/L4: one container per base image
```

The L2 suite is where the design's premise is verified: `libc-daemon` (a `daemon(3)` service) is tracked and stopped, `ignore-sigterm` and `fork-bomb-on-term` both terminate with zero survivors, and a stale `PIDFile=` fails the unit while leaving the unrelated process alive.

## Releasing

Releases are cut by hand: **Actions → Release → Run workflow**
([.github/workflows/release.yml](.github/workflows/release.yml)). The run tests, builds
the artifact set below, signs the rpms if a `GPGPRIVATE` secret is configured, tags the
commit it ran on and creates the GitHub release with that version's `CHANGELOG.md`
section as the notes.

| Artifact | What it is |
| --- | --- |
| `systemd-amd64`, `systemd-arm64` | the static binaries, for `COPY` into an image |
| `docker-systemd_<version>_amd64.deb`, `docker-systemd_<version>_arm64.deb` | installer packages, `/usr/sbin/init-docker-systemd` |
| `docker-systemd-<version>-2.x86_64.rpm`, `docker-systemd-<version>-2.aarch64.rpm` | the same, converted with `alien` (`-2` is alien's release bump) |
| `SHA256SUMS` | checksums of the six files above |

The version comes from the `VERSION` file unless the `version` input overrides it, and
the run refuses to overwrite a tag or release that already exists. `dry_run` builds and
checks the artifacts without tagging or releasing anything — either way they are attached
to the workflow run itself. The release is a draft by default, so it can be reviewed
before it goes public.

`make release` produces the same set locally, into `./dist`; it needs `dpkg-dev` and
`alien` installed. The apt and yum repositories on GitHub Pages are published separately,
by [.github/workflows/build.yml](.github/workflows/build.yml).

## Design documents

The full design — the process model, the defect register that motivated the rebuild, the unit semantics, the control protocol, the security model and the test strategy — lives in [designs/docs/next/](designs/docs/next/README.md).
