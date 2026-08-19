package supervisor

import (
	"bufio"
	"io"
	"os"
	"strings"
	"time"

	"docker-systemd/internal/journal"
	"docker-systemd/internal/proto"
)

// maxLineLen caps an assembled log line; beyond it the line is broken with a
// truncation marker rather than letting a unit that never emits a newline
// consume unbounded memory.
const maxLineLen = 16 << 10

// stdio opens the three standard descriptors for a unit process according to
// StandardInput=/StandardOutput=/StandardError=.
//
// Two separate pipes are used for stdout and stderr so that the stream can be
// recorded in the log priority: v0.5.x interleaved both into one stream with no
// way to tell them apart, which made `journalctl -p err` impossible.
func (s *Supervisor) stdio(isMain bool) (stdin, stdout, stderr *os.File, closers []*os.File, err error) {
	svc := s.unit.Service

	stdin, err = s.openInput(svc.StandardInput)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if stdin != devNull {
		closers = append(closers, stdin)
	}

	stdout, close1, err := s.openOutput(svc.StandardOutput, false)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if close1 != nil {
		closers = append(closers, close1)
	}

	stderr, close2, err := s.openOutput(svc.StandardError, true)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if close2 != nil {
		closers = append(closers, close2)
	}
	return stdin, stdout, stderr, closers, nil
}

// devNull is opened once and shared; it is never in the closers list.
var devNull *os.File

func init() {
	f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err == nil {
		devNull = f
	}
}

func (s *Supervisor) openInput(spec string) (*os.File, error) {
	switch {
	case spec == "" || spec == "null":
		return devNull, nil
	case spec == "inherit":
		return os.Stdin, nil
	case spec == "tty":
		f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return devNull, nil
		}
		return f, nil
	case strings.HasPrefix(spec, "file:"):
		f, err := os.Open(strings.TrimPrefix(spec, "file:"))
		if err != nil {
			return devNull, nil
		}
		return f, nil
	default:
		return devNull, nil
	}
}

// openOutput returns the descriptor the child writes to, plus the parent-side
// file to close after the spawn.
func (s *Supervisor) openOutput(spec string, isErr bool) (child *os.File, closeAfter *os.File, err error) {
	switch {
	case spec == "" || spec == "journal" || spec == "journal+console":
		r, w, err := os.Pipe()
		if err != nil {
			return nil, nil, err
		}
		prio := 6
		if isErr {
			prio = 3
		}
		go s.pumpLog(r, prio)
		return w, w, nil
	case spec == "null":
		return devNull, nil, nil
	case spec == "inherit":
		if isErr {
			return os.Stderr, nil, nil
		}
		return os.Stdout, nil, nil
	case spec == "tty":
		f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
		if err != nil {
			return devNull, nil, nil
		}
		return f, f, nil
	case strings.HasPrefix(spec, "file:"):
		f, err := os.OpenFile(strings.TrimPrefix(spec, "file:"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
		if err != nil {
			return nil, nil, err
		}
		return f, f, nil
	case strings.HasPrefix(spec, "append:"):
		f, err := os.OpenFile(strings.TrimPrefix(spec, "append:"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return nil, nil, err
		}
		return f, f, nil
	default:
		return devNull, nil, nil
	}
}

// pumpLog performs line assembly on one of the unit's output pipes and
// forwards each assembled line to the manager's broker as a framed record.
//
// Backpressure is the pipe's: if the broker cannot keep up we stop reading,
// the pipe fills, and the unit blocks on write(2) — exactly what systemd does.
// Records are never dropped silently.
func (s *Supervisor) pumpLog(r *os.File, defaultPriority int) {
	defer r.Close()
	br := bufio.NewReaderSize(r, 32<<10)
	var partial strings.Builder
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			partial.WriteString(strings.TrimRight(line, "\n"))
			if strings.HasSuffix(line, "\n") || partial.Len() >= maxLineLen {
				text := partial.String()
				partial.Reset()
				truncated := ""
				if len(text) > maxLineLen {
					truncated = " [truncated]"
					text = text[:maxLineLen]
				}
				s.emitLog(defaultPriority, text+truncated)
			}
		}
		if err != nil {
			if partial.Len() > 0 {
				s.emitLog(defaultPriority, partial.String())
			}
			if err == io.EOF {
				return
			}
			return
		}
	}
}

// emitLog frames one record and sends it to the manager.
func (s *Supervisor) emitLog(defaultPriority int, msg string) {
	prio := defaultPriority
	if s.unit.Service != nil && s.unit.Service.SyslogLevelPrefix {
		prio, msg = journal.SplitPriorityPrefix(msg, defaultPriority)
	}
	ident := ""
	if s.unit.Service != nil {
		ident = s.unit.Service.SyslogIdentifier
	}
	rec := proto.LogRecord{
		Unit:       s.unit.Name,
		Identifier: ident,
		PID:        s.MainPID(),
		Priority:   prio,
		TimeUnixNS: time.Now().UnixNano(),
		Message:    msg,
	}
	s.send(proto.TypeLogRecord, rec)
}
