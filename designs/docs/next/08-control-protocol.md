# 08 — Control protocol

Two protocols: **client ↔ manager** (over the control socket) and **manager ↔
supervisor** (over an inherited socketpair). Both use the same framing.

---

## 1. Why the current one has to be replaced

The existing protocol (`u16 argc, {u16 len, bytes}*` request; free-text response
terminated by a bare `0x00`; then a 5-byte magic and a `u16` exit code) has four
defects:

* **D1** both ends assume a single `read()` returns a whole message; a long argument
  list or long output splits across reads and is misparsed;
* **D2** the sentinel is a bare `0x00`, so any unit output containing a NUL truncates
  the stream;
* **D3** a hard 64 KiB cap on both directions;
* **D4** unexpected trailing bytes call `log.Fatalf` in the client.

It is also untyped: the client cannot distinguish "this is progress output" from "this
is the answer", which makes machine-readable verbs (`show -p X --value`, `list-units
--no-legend`) impossible to implement cleanly.

## 2. Framing

```
frame := u32 length (big-endian, of everything after this field, max 16 MiB)
         u8  type
         u8  flags
         u16 reserved (0)
         payload[length-4]
```

Full reads on both sides; a short read continues, it never truncates. A frame larger
than the cap closes the connection with `E_FRAME_TOO_LARGE`.

## 3. Message types

| type | direction | payload |
|---|---|---|
| `0x01 HELLO` | C→S | `u16 proto_version`, client name |
| `0x02 HELLO_ACK` | S→C | `u16 proto_version`, server version string, capability flags |
| `0x10 REQUEST` | C→S | canonical JSON: `{verb, units[], options{}, args[]}` |
| `0x20 PROGRESS` | S→C | UTF-8 text, for human display; suppressed under `--quiet` |
| `0x21 DATA` | S→C | canonical JSON: the machine-readable result |
| `0x22 LOG` | S→C | a log record, for streaming verbs |
| `0x2F RESULT` | S→C | `{exit_code:u8, error_kind, message}` — always the last frame |
| `0x30 CANCEL` | C→S | cancel the in-flight request |

Rules:

* Exactly one `RESULT` terminates every request; the client exits with its code.
* `PROGRESS` is for humans; `DATA` is for scripts. `systemctl show -p MainPID --value`
  reads only `DATA`. This is what makes the CLI scriptable without output scraping.
* The version handshake lets a mismatched `systemctl` (from a package upgrade inside a
  running container) fail with a clear message instead of misparsing.

Choosing JSON for the payload is deliberate: the protocol is local, low-volume, and
long-lived; a self-describing format costs nothing measurable here and removes the
entire class of framing/versioning defects above. Fixed-layout binary would be
premature optimisation on a socket that carries a few hundred bytes per `systemctl`
invocation.

## 4. Request semantics

```json
{ "verb": "start",
  "units": ["nginx.service"],
  "options": { "now": true, "no_block": false, "quiet": false },
  "args": [] }
```

* `no_block: true` ⇒ the manager enqueues the job and returns immediately with the job
  id in `DATA`; used by maintainer scripts.
* Otherwise the manager streams `PROGRESS` per unit and returns when the job set
  settles or its `JobTimeoutSec` expires.
* Verbs are validated against a table; an unknown verb returns `RESULT` with exit code
  2, never 0 (fixes D6).

## 5. Manager ↔ supervisor

Same framing, over `fd 3` of the supervisor (a `SOCK_SEQPACKET` socketpair — message
boundaries for free, and it is available in every container).

| type | direction | payload |
|---|---|---|
| `0x40 CONFIG` | M→S | the full `ResolvedUnit` (sent once, at spawn) |
| `0x41 START` | M→S | begin activation |
| `0x42 STOP` | M→S | `{mode: normal|restart|shutdown, timeout_ms}` |
| `0x43 RELOAD` | M→S | run `ExecReload=` or send `SIGHUP` |
| `0x44 KILL` | M→S | `{signal, who: main|all|control}` |
| `0x45 QUERY` | M→S | request a state snapshot |
| `0x50 STATE` | S→M | `{state, substate, main_pid, tasks, result, status_text, timestamps}` |
| `0x51 LOG` | S→M | a log record for the broker |
| `0x52 NOTIFY` | S→M | a forwarded `sd_notify` assignment |
| `0x53 EXITED` | S→M | `{pid, code, signal, is_main}` |

Additional fds passed at spawn: `fd 4` = log pipe write end (unused if the supervisor
frames records itself), `fd 5` = the notify socket when `Type=notify`.

The supervisor's **process exit** is the authoritative "unit fully stopped" event
(invariant I4); the `STATE` stream is advisory replication. That means a supervisor that
dies without reporting still produces a correct manager-side transition.

## 6. Reliability

* The manager never blocks on a supervisor write; each supervisor connection has a
  bounded queue, and overflow drops `PROGRESS`/`LOG` (never `STATE`/`EXITED`) with a
  counter exposed in `systemctl show -p DroppedMessages`.
* A supervisor that stops responding to `QUERY` within 5 s is reported as
  `state: unknown` in `status` and is `SIGKILL`ed on the next stop request.
* All sockets are `SOCK_SEQPACKET` or `SOCK_STREAM` with the framing above; no datagram
  truncation is possible.

## 7. Compatibility

The v0.5 protocol is not preserved. `systemctl` and the manager are the same binary and
are upgraded together; the only mixed-version scenario is a container where the old
binary lingers under a different name, which the `HELLO` handshake diagnoses:

```
systemctl: protocol version mismatch (client 1, manager 2).
The systemctl binary at /usr/bin/systemctl is stale; re-run the installer or
use /usr/sbin/init-docker-systemd systemctl ...
```
