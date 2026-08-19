// Package journal implements the on-disk log record format, the rotating
// per-unit writer, and the reader that backs journalctl.
package journal

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// TimeFormat is fixed-width UTC RFC3339 with microseconds. Fixed width is what
// makes a lexicographic comparison a chronological one, so --since/--until can
// be answered by binary search over a file rather than a linear parse.
const TimeFormat = "2006-01-02T15:04:05.000000Z"

// Record is one log line.
type Record struct {
	Time       time.Time
	Priority   int
	Unit       string
	Identifier string
	PID        int
	Message    string
	// Legacy marks a record recovered from the v0.5.x un-timestamped format,
	// whose time is inherited from the preceding record.
	Legacy bool
}

// Ident returns the identifier used in the `unit[pid]` field:
// SyslogIdentifier= when set, otherwise the unit name.
func (r Record) Ident() string {
	if r.Identifier != "" {
		return r.Identifier
	}
	return r.Unit
}

// Encode renders a record in the on-disk format:
//
//	2026-08-11T09:14:02.117384Z <6> nginx.service[431]: starting worker processes
func (r Record) Encode() string {
	var b strings.Builder
	b.WriteString(r.Time.UTC().Format(TimeFormat))
	b.WriteString(" <")
	b.WriteString(strconv.Itoa(r.Priority))
	b.WriteString("> ")
	b.WriteString(r.Ident())
	if r.PID > 0 {
		b.WriteByte('[')
		b.WriteString(strconv.Itoa(r.PID))
		b.WriteByte(']')
	}
	b.WriteString(": ")
	b.WriteString(escapeMessage(r.Message))
	b.WriteByte('\n')
	return b.String()
}

// escapeMessage renders bytes that are not valid UTF-8 as \xNN and strips a
// trailing \r. An embedded \n cannot occur: records are produced by line
// assembly.
func escapeMessage(s string) string {
	s = strings.TrimRight(s, "\r")
	if utf8.ValidString(s) && !strings.ContainsAny(s, "\n\r") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\n' || s[i] == '\r' {
			fmt.Fprintf(&b, `\x%02x`, s[i])
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&b, `\x%02x`, s[i])
			i++
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

// Decode parses one on-disk line back into a Record.
//
// A line that does not carry a timestamp is a legacy v0.5.x record: it is
// returned with Legacy set and priority 6, so that a container upgraded in
// place still shows its old logs.
func Decode(line, unit string) Record {
	line = strings.TrimRight(line, "\r\n")
	if len(line) < len(TimeFormat)+4 {
		return Record{Unit: unit, Priority: 6, Message: line, Legacy: true}
	}
	ts, err := time.Parse(TimeFormat, line[:len(TimeFormat)])
	if err != nil {
		return Record{Unit: unit, Priority: 6, Message: line, Legacy: true}
	}
	rest := line[len(TimeFormat):]
	if !strings.HasPrefix(rest, " <") {
		return Record{Unit: unit, Priority: 6, Message: line, Legacy: true}
	}
	close := strings.IndexByte(rest, '>')
	if close < 0 {
		return Record{Unit: unit, Priority: 6, Message: line, Legacy: true}
	}
	prio, err := strconv.Atoi(rest[2:close])
	if err != nil {
		return Record{Unit: unit, Priority: 6, Message: line, Legacy: true}
	}
	rest = strings.TrimPrefix(rest[close+1:], " ")

	rec := Record{Time: ts, Priority: prio, Unit: unit}
	if colon := strings.Index(rest, ": "); colon >= 0 {
		ident := rest[:colon]
		rec.Message = rest[colon+2:]
		if open := strings.LastIndexByte(ident, '['); open >= 0 && strings.HasSuffix(ident, "]") {
			if pid, err := strconv.Atoi(ident[open+1 : len(ident)-1]); err == nil {
				rec.PID = pid
				ident = ident[:open]
			}
		}
		rec.Identifier = ident
	} else {
		rec.Message = rest
	}
	return rec
}

// SplitPriorityPrefix honours the systemd convention of a leading `<N>` on a
// line emitted by a unit: the prefix sets the record's priority and is
// stripped from the message.
func SplitPriorityPrefix(msg string, def int) (int, string) {
	if len(msg) >= 3 && msg[0] == '<' && msg[2] == '>' && msg[1] >= '0' && msg[1] <= '7' {
		return int(msg[1] - '0'), msg[3:]
	}
	return def, msg
}

// PriorityName renders a syslog priority as its short name.
func PriorityName(p int) string {
	switch p {
	case 0:
		return "emerg"
	case 1:
		return "alert"
	case 2:
		return "crit"
	case 3:
		return "err"
	case 4:
		return "warning"
	case 5:
		return "notice"
	case 6:
		return "info"
	case 7:
		return "debug"
	}
	return strconv.Itoa(p)
}

// ParsePriority accepts a name or number, as journalctl -p does.
func ParsePriority(s string) (int, error) {
	names := map[string]int{
		"emerg": 0, "panic": 0, "alert": 1, "crit": 2, "err": 3, "error": 3,
		"warning": 4, "warn": 4, "notice": 5, "info": 6, "debug": 7,
	}
	s = strings.ToLower(strings.TrimSpace(s))
	if n, ok := names[s]; ok {
		return n, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 7 {
		return 0, fmt.Errorf("invalid priority %q", s)
	}
	return n, nil
}
