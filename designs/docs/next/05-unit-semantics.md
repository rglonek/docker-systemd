# 05 — Unit file semantics

Specification of discovery, parsing, and execution semantics. Written so the parser can
be reimplemented without reference to the existing one.

---

## 1. Unit discovery

Search path, **highest precedence first**:

```
/etc/systemd/system         # administrator / enable symlinks
/run/systemd/system         # runtime, generated
/usr/lib/systemd/system     # vendor  ─┬─ these two are the same directory on
/lib/systemd/system         # vendor  ─┘  usrmerge systems; de-duplicate by
                            #             resolving each directory with realpath()
```

Rules:

* De-duplicate **directories** by `realpath` before scanning, once, at load. This
  replaces the six-branch symlink heuristic in `common.GetSystemdPaths` (F13) and is
  correct on merged, unmerged and partially-merged layouts.
* A unit name found in a higher-precedence directory **completely replaces** the
  lower-precedence fragment. Never merge two fragments for the same name.
* A fragment that is a symlink to `/dev/null` ⇒ `masked`.
* A fragment that is an empty regular file ⇒ `masked` (systemd treats it as such).
* A fragment that is a symlink to *another unit file* ⇒ **alias**: register the name as
  an alias of the target unit; do not load it twice, and do not leave it unloaded (A5).

**Drop-ins**, applied in this order after the fragment, each parsed as an overlay:

```
/usr/lib/systemd/system/<unit>.d/*.conf
/run/systemd/system/<unit>.d/*.conf
/etc/systemd/system/<unit>.d/*.conf
/etc/systemd/system/service.d/*.conf          # type-wide, lowest priority of all
/etc/systemd/system/<template>@.d/*.conf      # for instances, before the instance's own
```

within each directory, sorted by filename (ASCII). The current implementation scans only
`<unit>.service.d` next to the fragment and misses `/run` and the template drop-in
directory.

Also scanned, for dependency injection:

```
<any unit dir>/<unit>.wants/*        -> Wants=  edge to each entry
<any unit dir>/<unit>.requires/*     -> Requires= edge to each entry
```

## 2. Lexical structure

```
line        := comment | continuation | section | assignment | blank
comment     := WS* ('#' | ';') ANY*
section     := WS* '[' name ']' WS*
assignment  := WS* key WS* '=' value
continuation: a line whose last non-whitespace character is '\' continues onto the next
              line; the '\' is removed and replaced by a single space
```

* Encoding is UTF-8; invalid sequences are replaced, not fatal.
* Keys are **case-sensitive** in systemd. The current implementation upper-cases them,
  which accepts `execstart=` — harmless, but the canonical spelling should be preferred
  and unknown keys warned about with their original case.
* Unknown **sections** are skipped entirely along with their contents.
* Unknown **keys** in a known section produce one warning each and are ignored.
* The value is everything after the first `=`, with **leading whitespace stripped and
  trailing whitespace stripped**. No quote processing happens here — quotes are the
  business of the value's own grammar (fixes C5).
* An assignment with an **empty value resets** the directive: for list-valued
  directives it clears the accumulated list, for scalars it restores the default. This
  is how drop-ins subtract; it must be implemented for `ExecStart=`, all dependency
  lists, `Environment=`, `EnvironmentFile=`.

## 3. Value grammars

### 3.1 Booleans

Accept, case-insensitively: `1 yes true on` ⇒ true; `0 no false off` ⇒ false.
Anything else is a parse error naming the directive.
(The current parser accepts only the literal `true`, so `RemainAfterExit=yes` — the
spelling in essentially every real unit file — silently reads as false. Defect C2.)

### 3.2 Time spans

`<value>[unit]` repeated and summed; bare numbers are **seconds**; the literal
`infinity` means "no timeout".

| unit | aliases |
|---|---|
| `us` | `usec`, `µs`, `μs` |
| `ms` | `msec` |
| `s` | `sec`, `second`, `seconds` |
| `m` | `min`, `minute`, `minutes` |
| `h` | `hr`, `hour`, `hours` |
| `d` | `day`, `days` |
| `w` | `week`, `weeks` |
| `M` | `month`, `months` (30.44 d) |
| `y` | `year`, `years` (365.25 d) |

`"5min 20s"` = 320 s. The existing implementation of this (a vendored copy of Go's
`time.ParseDuration` with a substitution table) is correct in substance; note only that
its alias substitution is order-dependent (`"seconds"` must be replaced before `"s"`,
which it is) and that it should reject negative spans for timeouts.

### 3.3 Sizes

`K M G T` (×1024) and `KB MB GB TB` (×1000) suffixes, for `LimitFSIZE=` etc. and for
the new `LogSizeMax=`.

## 4. Command lines

**systemd does not use a shell, and neither should we** (defect C1). Replacing
`bash -c` is both a correctness fix and a tracking fix: today, the shell is an extra
process between the supervisor and the service, which is precisely what makes the main
PID ambiguous.

### 4.1 Prefixes

Parsed from the start of the value, in any order, before the executable path:

| prefix | meaning | support |
|---|---|---|
| `-` | ignore a non-zero exit status | **implement** |
| `@` | `argv[0]` is given separately from the executable path | **implement** |
| `:` | do not expand environment variables in the arguments | **implement** |
| `+` | run with full privileges, ignoring `User=`/`Group=` | **implement** (we are root; it simply skips the credential change) |
| `!` | ignore `User=`/`Group=` but still apply the rest of the exec context | **implement** as `+` |
| `!!` | as `!`, but only where ambient capabilities are unsupported | **implement** as `+` |

Today only `-` is honoured; the others become part of the path and the command fails
with a confusing "no such file".

### 4.2 Tokenisation

```
tokenize(value) -> [argv...]
    split on unquoted whitespace
    "..."   double quotes: contents taken literally except that \ escapes are processed
            and $VAR / ${VAR} are expanded
    '...'   single quotes: fully literal, no expansion
    \x      C-style escapes recognised: \a \b \f \n \r \t \v \\ \" \' \s(space)
            \xHH  \NNN(octal)  \uXXXX  \UXXXXXXXX
    NO glob expansion, NO pipes, NO redirection, NO command substitution, NO ~
```

A `;` or `|` in an `ExecStart=` is an ordinary argument character. Units that genuinely
need a shell must say so: `ExecStart=/bin/sh -c '…'`. This is a **behaviour change** from
the current implementation and must be called out in the migration notes — a unit that
today relies on `ExecStart=/bin/foo && /bin/bar` working will stop working, exactly as
it would under real systemd.

### 4.3 Variable expansion

* `$VAR` and `${VAR}` are expanded from the unit's assembled environment (see §7.3).
* Unset variables expand to the empty string.
* `${VAR}` inside a quoted string does **not** word-split; `$VAR` unquoted **does**
  (systemd's rule for `ExecStart=`: unquoted `$VAR` is split on whitespace,
  `${VAR}` is not).
* `$$` yields a literal `$`.
* No expansion at all if the `:` prefix was given.

### 4.4 Executable resolution

The first token must be an absolute path, or — a systemd ≥ 239 relaxation worth
supporting — a bare name resolved against a fixed search path
(`/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`). Resolution failure is
reported as `status=203/EXEC` with the attempted path, not as a shell error.

### 4.5 Multiplicity

| Type | `ExecStart=` lines |
|---|---|
| `oneshot` | zero or more, run **sequentially**, each to completion; a non-zero exit (without `-`) fails the unit and skips the rest |
| everything else | **exactly one**; more is a parse error (`ExecStart= may only be specified once for Type=<t>`). Today, several lines are started in parallel (C10) |

`ExecStartPre=`, `ExecStartPost=`, `ExecStopPost=`, `ExecCondition=`, `ExecReload=` may
each appear multiple times and run sequentially. `ExecStop=` likewise.

`ExecStopPre=` is **not a systemd directive** and is dropped (C9); if any real unit in
the wild uses it because of this project, accept it with a deprecation warning for one
release.

## 5. Targets

Targets are synchronisation points with no processes. The current implementation has
none (C18), which makes `After=network.target` a dangling reference and forces the
`.wants`-directory hack.

Implement `.target` units as first-class, with:

* real `.target` files parsed from the search path when present;
* **synthetic** targets created on demand for well-known names that a container image
  may not ship: `basic.target`, `sysinit.target`, `local-fs.target`, `network.target`,
  `network-online.target`, `remote-fs.target`, `time-sync.target`, `sockets.target`,
  `timers.target`, `paths.target`, `multi-user.target`, `graphical.target`,
  `default.target`, `shutdown.target`, `getty.target`, `nss-lookup.target`,
  `rpcbind.target`, `dbus.socket`;
* a synthetic target starts immediately and successfully; it exists so that
  `After=`/`Wants=` resolve and order correctly;
* `network-online.target` optionally gated on a configurable readiness probe
  (`--network-online-probe=none|route|dns`, default `none` — a container's network is up
  before init runs).

`multi-user.target` `Wants=` is populated from `/etc/systemd/system/multi-user.target.wants/`
as today, **plus** every unit whose `[Install] WantedBy=` names it and which is enabled.

## 6. Templates and instances

Do **not** materialise instances (defect B13 — `delete-instance` currently `RemoveAll`s
the distro's unit file). Resolve them:

```
resolve("foo@bar.service"):
    if a fragment named "foo@bar.service" exists -> use it (an explicit instance)
    else if a fragment named "foo@.service" exists -> use it as the template,
        with instance = "bar"
    else -> not-found
```

* Enabling an instance creates `…/<target>.wants/foo@bar.service` → the **template**
  file. That is exactly what systemd does and what `ls -l` in the container will show.
* `create-instance` / `delete-instance` are retained as compatibility aliases that
  create/remove the enable symlink only. They must never delete a fragment.
* `DefaultInstance=` in `[Install]` supplies the instance for a bare `foo@.service`
  enable.
* Instance names are `%i`-escaped: `-` in a path becomes `\x2d`, `/` becomes `-`.
  `%I` is the **unescaped** form. The current code treats `%i` and `%I` as identical
  (C7).

## 7. Execution context

### 7.1 Specifiers

Expanded in *all* directive values that systemd expands them in — not just `Exec*=`
(defect C7). Minimum set:

| spec | value |
|---|---|
| `%n` | full unit name, e.g. `foo@bar.service` |
| `%N` | as `%n` without the type suffix |
| `%p` | prefix (before `@`), escaped |
| `%P` | prefix, unescaped |
| `%i` | instance, escaped |
| `%I` | instance, unescaped |
| `%f` | unescaped instance, prefixed with `/`; or unescaped prefix |
| `%j` | final component of the prefix |
| `%t` | `/run` |
| `%S` | `/var/lib` |
| `%C` | `/var/cache` |
| `%L` | `/var/log` |
| `%E` | `/etc` |
| `%h` | home of `User=` (or `/root`) |
| `%u` / `%U` | `User=` name / uid |
| `%g` / `%G` | `Group=` name / gid |
| `%H` | hostname |
| `%m` | machine id (`/etc/machine-id`; synthesise and persist one at boot if absent) |
| `%b` | boot id |
| `%v` | kernel release (`uname -r`) |
| `%a` | architecture |
| `%%` | literal `%` |

### 7.2 Credentials

```
User=   resolve via NSS; if numeric, use directly and do not require a passwd entry
Group=  resolve via NSS
if User= is set and Group= is not: Group = the user's primary group
supplementary groups = the user's group memberships + SupplementaryGroups=
apply in the child, in this order:  setgroups -> setgid -> setuid
```

Corrections to the current behaviour (C6):

* apply the credential change **whenever `User=` or `Group=` is set**, including when
  the resolved uid is 0 — today it is skipped entirely for uid 0, so `User=root
  Group=adm` silently runs as `root:root`;
* set supplementary groups — today they are not set, so a service dropped to `www-data`
  loses access to group-readable files;
* export `$HOME`, `$USER`, `$LOGNAME`, `$SHELL` for the target user;
* if `User=` cannot be resolved, fail the unit with `217/USER`, do not fall back to root.

### 7.3 Environment assembly

Precedence, lowest to highest:

```
1. a fixed base:  PATH, LANG (from the manager), TERM (if a tty), INVOCATION_ID,
                  MANAGED_BY_UNIT, MANAGED_BY_INVOCATION   (see 03 §6.8)
2. the manager environment set by `systemctl set-environment`
3. EnvironmentFile=  (in declaration order; later files win)
4. Environment=      (in declaration order; later assignments win)
5. NOTIFY_SOCKET, MAINPID, LISTEN_* injected by the supervisor
```

The current code starts from `os.Environ()` — the **container's** entire environment,
including whatever `docker run -e` set and whatever the manager inherited. Keep that
(it is a useful container-specific behaviour, and units rely on `docker run -e`) but
make it explicit and filterable with `--pass-environment=<list>|all|none`, defaulting to
`all` for compatibility.

`EnvironmentFile=` parsing (defect C3) — currently a bare split on `\n`:

```
skip blank lines and lines whose first non-whitespace char is '#'
strip an optional leading "export "
split on the first '='
strip surrounding single or double quotes from the value
process \ escapes inside double quotes only
support line continuation with a trailing backslash
a leading '-' on the FILENAME means "ignore if missing"
```

`Environment=` (defect C4) accepts **several** space-separated assignments per line,
with quoting: `Environment="A=1 2" B=3`.

### 7.4 Resource limits

The current code warns and ignores all 16 `Limit*=` directives, on the stated grounds
that docker forbids them. That is only half true: `setrlimit` may always **lower** a
limit, and may raise a soft limit up to the inherited hard limit, without any
capability. Only raising a *hard* limit needs `CAP_SYS_RESOURCE`.

```
apply(limit, soft, hard):
    cur = getrlimit(limit)
    new_hard = min(hard, cur.hard)         unless we hold CAP_SYS_RESOURCE
    new_soft = min(soft, new_hard)
    if requested hard > cur.hard and not privileged:
        warn once: "LimitX=<v> exceeds the container hard limit <cur.hard>;
                    clamped. Raise it with `docker run --ulimit x=<v>`."
    setrlimit(limit, new_soft, new_hard)
```

`LimitNOFILE=` is the one that matters in practice (databases, proxies) and it is
almost always *lowering* relative to the container's hard limit, so it will simply work.
This turns 16 dead directives into working ones with a clear diagnostic for the
genuinely impossible cases.

Also implement, since none of them need privileges: `UMask=`, `Nice=`,
`OOMScoreAdjust=` (raising it is always allowed; lowering needs privileges — clamp and
warn), `WorkingDirectory=` (with the `-` prefix meaning "ignore if missing"),
`RuntimeDirectory=`/`StateDirectory=`/`CacheDirectory=`/`LogsDirectory=`/
`ConfigurationDirectory=` (create with `RuntimeDirectoryMode=` etc., chown to
`User=`/`Group=`, remove `RuntimeDirectory=` on stop) — the `*Directory=` family is a
frequent cause of stock units failing under this project today.

`NoNewPrivileges=` is also free (`prctl(PR_SET_NO_NEW_PRIVS)`).

### 7.5 Standard I/O

Implement `StandardInput=` (`null` default, `tty`, `inherit`),
`StandardOutput=`/`StandardError=` (`journal` default → our log broker, `inherit`,
`null`, `tty`, `file:<path>`, `append:<path>`), and `SyslogIdentifier=`.

## 8. Directive support matrix for the new version

Legend: **✔** implemented · **◐** partial, documented · **✖** rejected with a one-time
warning · **–** parsed and ignored silently.

### `[Unit]`

| Directive | v1 |
|---|---|
| `Description=` | ✔ |
| `Documentation=` | – |
| `Wants=` `Requires=` `Requisite=` `BindsTo=` `PartOf=` `Upholds=` `Conflicts=` | ✔ |
| `WantedBy=` `RequiredBy=` `UpheldBy=` (in `[Unit]`, non-standard but seen) | ✔ |
| `Before=` `After=` | ✔ (new — was parsed and unused) |
| `OnFailure=` `OnSuccess=` `OnFailureJobMode=` | ✔ / ✔ / ◐ |
| `ConditionPathExists=` `ConditionPathIsDirectory=` `ConditionFileNotEmpty=` `ConditionDirectoryNotEmpty=` `ConditionPathIsSymbolicLink=` `ConditionPathIsMountPoint=` `ConditionFileIsExecutable=` | ✔ (new) |
| `ConditionKernelVersion=` `ConditionArchitecture=` `ConditionHost=` `ConditionEnvironment=` `ConditionUser=` `ConditionGroup=` | ✔ (new) |
| `ConditionVirtualization=` | ✔ — must report `container`/`docker`, which makes many stock units correctly skip themselves |
| `ConditionCapability=` `ConditionSecurity=` `ConditionACPower=` `ConditionMemory=` `ConditionCPUs=` `ConditionFirstBoot=` `ConditionNeedsUpdate=` | ◐ (evaluate what we can, else treat as satisfied and warn once) |
| `Assert*=` | ✔ (same evaluators; failure ⇒ `failed` rather than skipped) |
| `StopWhenUnneeded=` | ✔ (was a no-op — B10) |
| `RefuseManualStart=` `RefuseManualStop=` | ✔ |
| `AllowIsolate=` `IgnoreOnIsolate=` | – |
| `JobTimeoutSec=` `JobRunningTimeoutSec=` | ✔ |
| `StartLimitIntervalSec=` `StartLimitBurst=` `StartLimitAction=` | ✔ (new — C12) |
| `FailureAction=` `SuccessAction=` | ◐ `none`, `poweroff*`, `exit*`, `reboot*`; others warn |
| `DefaultDependencies=` | ✔ (when `no`, skip the implicit `Before=shutdown.target` etc.) |
| `SourcePath=` | – |

### `[Service]`

| Directive | v1 |
|---|---|
| `Type=simple|exec|forking|oneshot|idle` | ✔ |
| `Type=notify|notify-reload` | ✔ (new — real `sd_notify`, B12) |
| `Type=dbus` | ◐ treated as `simple`, warn once (no bus) |
| `ExitType=main|cgroup` | ◐ `main` ✔; `cgroup` maps to "tree empty" |
| `RemainAfterExit=` | ✔ (and no longer force-set for oneshot — B20) |
| `GuessMainPID=` | ✔ ([03 §7](03-process-model.md#7-main-pid-determination)) |
| `PIDFile=` | ✔ with tree validation (C13/E4) |
| `BusName=` | ✖ |
| `ExecStart=` `ExecStartPre=` `ExecStartPost=` `ExecCondition=` `ExecReload=` `ExecStop=` `ExecStopPost=` | ✔ with full prefix and lexer support |
| `RestartSec=` `Restart=` `RestartPreventExitStatus=` `RestartForceExitStatus=` `SuccessExitStatus=` | ✔ |
| `TimeoutStartSec=` `TimeoutStopSec=` `TimeoutAbortSec=` `TimeoutSec=` `RuntimeMaxSec=` | ✔ (`TimeoutStartSec` is new — C11) |
| `WatchdogSec=` | ✔ (with `Type=notify`) |
| `KillMode=` `KillSignal=` `RestartKillSignal=` `FinalKillSignal=` `SendSIGHUP=` `SendSIGKILL=` | ✔ (new — [03 §8](03-process-model.md#8-the-termination-ladder)) |
| `User=` `Group=` `SupplementaryGroups=` | ✔ (C6) |
| `Environment=` `EnvironmentFile=` `PassEnvironment=` `UnsetEnvironment=` | ✔ (C3, C4) |
| `WorkingDirectory=` `RootDirectory=` | ✔ / ✖ |
| `UMask=` `Nice=` `OOMScoreAdjust=` `NoNewPrivileges=` | ✔ (new, all unprivileged) |
| `Limit*=` (16) | ✔ clamped to the container's hard limits with a diagnostic (§7.4) |
| `RuntimeDirectory=` `StateDirectory=` `CacheDirectory=` `LogsDirectory=` `ConfigurationDirectory=` + `*Mode=` + `RuntimeDirectoryPreserve=` | ✔ (new) |
| `StandardInput=` `StandardOutput=` `StandardError=` `SyslogIdentifier=` `SyslogLevel*=` | ✔ (new) |
| `TTYPath=` `TTYReset=` `TTYVHangup=` | ◐ |
| `PrivateTmp=` `PrivateDevices=` `PrivateNetwork=` `ProtectSystem=` `ProtectHome=` `ProtectKernel*=` `ReadOnlyPaths=` `InaccessiblePaths=` `RestrictAddressFamilies=` `SystemCallFilter=` `MemoryDenyWriteExecute=` `CapabilityBoundingSet=` `AmbientCapabilities=` | ✖ — need `CAP_SYS_ADMIN`/seccomp we do not have. **Warn once per unit, name the directive, and continue.** Silent ignoring of hardening directives is worse than a warning, because operators assume they took effect |
| `MemoryMax=` `MemoryHigh=` `CPUQuota=` `CPUWeight=` `TasksMax=` `IOWeight=` `Slice=` | ✖ unless the cgroup backend is active; suggest the equivalent `docker run` flag in the warning |
| `Delegate=` `OOMPolicy=` | ✖ |

### `[Install]`

| Directive | v1 |
|---|---|
| `WantedBy=` `RequiredBy=` `UpheldBy=` | ✔ — `enable` now links into the **named** target's `.wants`, not always `multi-user.target.wants` (B18) |
| `Alias=` | ✔ |
| `Also=` | ✔ |
| `DefaultInstance=` | ✔ |

## 9. Validation and diagnostics

Every rejected or clamped directive produces one structured warning, at load time, with
the file and line:

```
WARN unit=redis.service file=/lib/systemd/system/redis.service:24
     directive=PrivateTmp value=yes
     reason="requires CAP_SYS_ADMIN, not available in an unprivileged container"
     action=ignored
```

`systemctl status <unit>` and `systemctl cat <unit>` surface these, and
`systemctl show <unit> -p LoadError` returns them. This is the single highest-value
usability improvement available: the current implementation's failure mode for an
unsupported directive is silence.
