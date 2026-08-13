# 07 — Logging and the journal

---

## 1. Problems with the current scheme

* Unit output is appended **verbatim**, with no timestamp, so `journalctl --since`,
  `--until` and `-b` can never match anything (B7). The only timestamped copy is the
  `--log-to-stderr` mirror, which is not what `journalctl` reads.
* No rotation: `/var/log/services/<unit>.log` grows until the container's disk fills
  (F9).
* stdout and stderr are interleaved into one stream with no way to tell them apart, and
  there is no priority concept, so `journalctl -p err` is impossible.
* The log file is opened **once per start and once per stop**, and the stop-side handle
  is never closed (B14) — an fd leak proportional to restart count.
* `Logger.Write` splits on `\n` for the stderr mirror but writes the raw buffer to the
  file, so a partial line from the unit produces a mangled mirror.
* Files are `0644`, world-readable, and may contain credentials echoed by a service.

## 2. Model

Three roles:

```
unit process ──pipe──▶ supervisor ──framed records──▶ PID 1 log broker ──▶ sink(s)
                                                                          ├─ file
                                                                          └─ stderr mirror
```

* The **supervisor** owns the read end of the unit's stdout and stderr pipes (two
  separate pipes, so the stream can be recorded) and performs line assembly: it buffers
  until `\n`, with a hard line cap (default 16 KiB, then force a break with a
  `truncated` flag).
* Each assembled line becomes a **record** and is forwarded to PID 1 over the
  supervisor's control fd, so that there is exactly one writer per log file and rotation
  is race-free.
* The **log broker** in PID 1 owns one open file handle per unit for the unit's whole
  lifetime — opened once, closed once (fixes B14/B15).

Backpressure: the pipe itself provides it. If the broker cannot keep up, the supervisor
stops reading, the pipe fills, and the unit blocks on `write(2)` — exactly what systemd
does. Never drop records silently; if a sink fails (disk full), emit one
`LOG-SINK-ERROR` line to stderr per unit per minute and keep the unit running.

## 3. Record format

On-disk, one record per line, designed to be greppable by a human *and* parseable
without ambiguity:

```
2026-08-11T09:14:02.117384Z <6> nginx.service[431]: starting worker processes
└──────── RFC3339 micro ───┘ │   └── unit ──┘└pid┘  └────── message ──────┘
                             └ syslog priority 0-7
```

* Timestamps are UTC, RFC3339 with microseconds, fixed width — so a lexicographic
  comparison is a chronological comparison, and `--since`/`--until` can be answered by
  binary search over the file rather than a linear parse.
* Priority: stdout ⇒ `<6>` (info), stderr ⇒ `<3>` (err), by default; overridable with
  `SyslogLevel=`/`SyslogLevelPrefix=`. If the unit emits a leading `<N>` prefix (the
  systemd convention), honour it and strip it.
* The `unit[pid]` field uses `SyslogIdentifier=` when set.
* Bytes that are not valid UTF-8 are escaped `\xNN`; embedded `\n` cannot occur by
  construction; embedded `\r` is stripped at end-of-line.

`journalctl -o json` reconstructs `__REALTIME_TIMESTAMP`, `PRIORITY`, `_PID`,
`_SYSTEMD_UNIT`, `SYSLOG_IDENTIFIER`, `MESSAGE` from these fields, which is enough for
log shippers.

**Compatibility note.** This changes the on-disk format. `journalctl` must accept
legacy un-timestamped lines (treat them as priority 6 with an unknown timestamp,
inheriting the previous record's time) so that a container upgraded in place still
shows its old logs.

## 4. Rotation

Per unit, in the broker:

```
LogSizeMax=      default 16M     rotate when the active file exceeds this
LogFileCount=    default 3       keep <unit>.log.1 … .N, then discard
LogCompress=     default off     gzip rotated files  (off by default: no cgo, small win)
LogTotalMax=     default 128M    across all units; oldest rotated files pruned first
```

Rotation is a rename plus reopen, performed by the single writer, so no reader can
observe a torn record. `journalctl` reads `<unit>.log.N … <unit>.log` in order.

Add `journalctl --disk-usage` and `--vacuum-size=`/`--vacuum-time=` to match the
interface people expect.

## 5. Boot boundary

`/run/docker-systemd/boot-id` holds `<RFC3339 timestamp> <random 128-bit hex>`, written
once at boot. `journalctl -b` filters to records at or after that timestamp; `-b -1` is
unsupported (nothing survives a container restart unless `/var/log` is a volume) and
must say so rather than silently returning everything.

Moving this out of `/etc/boot-time` also stops the manager from dirtying `/etc` in
images that get committed.

## 6. The stderr mirror

`--log-to-stderr` writes the same records to init's stderr, so `docker logs` shows
everything. Two refinements over the current behaviour:

* the mirror uses the same record format, so `docker logs` output is parseable;
* `--log-to-stderr=<unit>[,<unit>...]` selects specific units, because mirroring a
  chatty service into `docker logs` is often exactly what one does not want.

The manager's **own** messages always go to stderr, with a level filter
(`--log-level=`), and always carry a component tag:

```
2026-08-11T09:14:01.002110Z <6> systemd: starting nginx.service
2026-08-11T09:14:02.117000Z <4> systemd[nginx.service]: PrivateTmp=yes ignored: requires CAP_SYS_ADMIN
```

## 7. `StandardOutput=` / `StandardError=`

| value | behaviour |
|---|---|
| `journal` (default), `journal+console` | broker sink (+ stderr mirror) |
| `inherit` | init's stdout/stderr directly |
| `null` | `/dev/null` |
| `tty` | the container's controlling terminal if any, else `null` |
| `file:<path>` | truncate and write |
| `append:<path>` | append |
| `socket`, `fd:<name>` | unsupported; warn once |

This is a small amount of work in the supervisor's pre-exec step and it removes a whole
class of "why can't I get this service to log where I want" issues.
