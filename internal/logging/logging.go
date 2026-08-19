// Package logging provides the manager's own levelled log, in the record
// format specified in designs/docs/next/07-logging.md §6:
//
//	2026-08-11T09:14:01.002110Z <6> systemd: starting nginx.service
//	2026-08-11T09:14:02.117000Z <4> systemd[nginx.service]: PrivateTmp=yes ignored
//
// Records are written to stderr so that `docker logs` shows them.
package logging

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Level is a syslog-derived severity.
type Level int

// Levels, ordered from most to least severe.
const (
	LevelError Level = iota
	LevelWarn
	LevelInfo
	LevelDebug
	LevelTrace
)

// Syslog priorities used in the record's <N> field.
const (
	PriErr    = 3
	PriWarn   = 4
	PriNotice = 5
	PriInfo   = 6
	PriDebug  = 7
)

// TimeFormat is the fixed-width UTC timestamp used everywhere in the project.
// Fixed width matters: lexicographic order is chronological order, which is
// what lets journalctl binary-search a log file for --since/--until.
const TimeFormat = "2006-01-02T15:04:05.000000Z"

// ParseLevel maps a --log-level= value onto a Level.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error", "err":
		return LevelError, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "info":
		return LevelInfo, nil
	case "debug":
		return LevelDebug, nil
	case "trace":
		return LevelTrace, nil
	}
	return LevelInfo, fmt.Errorf("unknown log level %q (want error, warn, info, debug or trace)", s)
}

func (l Level) String() string {
	switch l {
	case LevelError:
		return "error"
	case LevelWarn:
		return "warn"
	case LevelInfo:
		return "info"
	case LevelDebug:
		return "debug"
	default:
		return "trace"
	}
}

func (l Level) priority() int {
	switch l {
	case LevelError:
		return PriErr
	case LevelWarn:
		return PriWarn
	case LevelInfo:
		return PriInfo
	default:
		return PriDebug
	}
}

// Logger writes levelled records with a component tag.
type Logger struct {
	mu    sync.Mutex
	w     io.Writer
	level Level
	tag   string
	// now is overridable so tests get deterministic timestamps.
	now func() time.Time
}

// New returns a Logger writing to w with the given tag (e.g. "systemd").
func New(w io.Writer, level Level, tag string) *Logger {
	return &Logger{w: w, level: level, tag: tag, now: time.Now}
}

// Default returns the process-wide stderr logger at info level.
func Default() *Logger { return New(os.Stderr, LevelInfo, "systemd") }

// WithUnit returns a logger whose tag carries a unit name, rendering as
// `systemd[nginx.service]:`.
func (l *Logger) WithUnit(unit string) *Logger {
	base := l.tag
	if i := strings.IndexByte(base, '['); i >= 0 {
		base = base[:i]
	}
	sub := &Logger{w: l.w, level: l.level, tag: base + "[" + unit + "]", now: l.now}
	return sub
}

// SetLevel changes the threshold.
func (l *Logger) SetLevel(lv Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = lv
}

// Level reports the current threshold.
func (l *Logger) Level() Level {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level
}

// Enabled reports whether records at lv would be emitted.
func (l *Logger) Enabled(lv Level) bool { return lv <= l.Level() }

func (l *Logger) log(lv Level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lv > l.level {
		return
	}
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	msg = strings.TrimRight(msg, "\n")
	for _, line := range strings.Split(msg, "\n") {
		fmt.Fprintf(l.w, "%s <%d> %s: %s\n", l.now().UTC().Format(TimeFormat), lv.priority(), l.tag, line)
	}
}

// Errorf logs at error level.
func (l *Logger) Errorf(format string, args ...any) { l.log(LevelError, format, args...) }

// Warnf logs at warning level.
func (l *Logger) Warnf(format string, args ...any) { l.log(LevelWarn, format, args...) }

// Infof logs at info level.
func (l *Logger) Infof(format string, args ...any) { l.log(LevelInfo, format, args...) }

// Debugf logs at debug level.
func (l *Logger) Debugf(format string, args ...any) { l.log(LevelDebug, format, args...) }

// Tracef logs at trace level.
func (l *Logger) Tracef(format string, args ...any) { l.log(LevelTrace, format, args...) }
