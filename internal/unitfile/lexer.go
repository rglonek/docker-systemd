package unitfile

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Assignment is one `Key=value` line, carrying the location it came from so
// that every diagnostic can name a file and line (05 §9).
type Assignment struct {
	Section string
	Key     string
	Value   string
	File    string
	Line    int
}

// Fragment is the result of lexing one unit file or drop-in.
type Fragment struct {
	Path        string
	Assignments []Assignment
	Warnings    []Warning
}

// Warning is a structured load-time diagnostic.
type Warning struct {
	Unit      string `json:"unit,omitempty"`
	File      string `json:"file,omitempty"`
	Line      int    `json:"line,omitempty"`
	Directive string `json:"directive,omitempty"`
	Value     string `json:"value,omitempty"`
	Reason    string `json:"reason"`
	Action    string `json:"action,omitempty"`
}

// String renders a warning in the format specified in 05 §9.
func (w Warning) String() string {
	var b strings.Builder
	b.WriteString("unit=")
	b.WriteString(w.Unit)
	if w.File != "" {
		fmt.Fprintf(&b, " file=%s:%d", w.File, w.Line)
	}
	if w.Directive != "" {
		fmt.Fprintf(&b, " directive=%s", w.Directive)
	}
	if w.Value != "" {
		fmt.Fprintf(&b, " value=%s", w.Value)
	}
	fmt.Fprintf(&b, " reason=%q", w.Reason)
	if w.Action != "" {
		fmt.Fprintf(&b, " action=%s", w.Action)
	}
	return b.String()
}

// Lex parses the ini-like unit-file grammar of 05 §2.
//
// Notably it does no quote processing on the value: quotes are the business of
// the value's own grammar, and stripping them here changed the meaning of
// `ExecStart="/opt/my app/bin" --flag` (defect C5).
func Lex(path string, r io.Reader) (*Fragment, error) {
	frag := &Fragment{Path: path}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	section := ""
	lineNo := 0
	var pending strings.Builder
	pendingLine := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		if !utf8.ValidString(raw) {
			raw = strings.ToValidUTF8(raw, "�")
		}
		// A trailing backslash continues onto the next line; the backslash is
		// replaced by a single space.
		if cont, body := splitContinuation(raw); cont {
			if pending.Len() == 0 {
				pendingLine = lineNo
			}
			pending.WriteString(body)
			pending.WriteString(" ")
			continue
		}
		line := raw
		startLine := lineNo
		if pending.Len() > 0 {
			pending.WriteString(raw)
			line = pending.String()
			startLine = pendingLine
			pending.Reset()
		}

		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			continue
		case trimmed[0] == '#' || trimmed[0] == ';':
			continue
		case trimmed[0] == '[':
			end := strings.IndexByte(trimmed, ']')
			if end < 0 {
				frag.Warnings = append(frag.Warnings, Warning{
					File: path, Line: startLine,
					Reason: "malformed section header", Action: "ignored",
				})
				continue
			}
			section = trimmed[1:end]
			continue
		}

		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			frag.Warnings = append(frag.Warnings, Warning{
				File: path, Line: startLine, Value: trimmed,
				Reason: "line is neither a comment, a section header nor an assignment",
				Action: "ignored",
			})
			continue
		}
		if section == "" {
			frag.Warnings = append(frag.Warnings, Warning{
				File: path, Line: startLine, Directive: strings.TrimSpace(line[:eq]),
				Reason: "assignment before the first section header", Action: "ignored",
			})
			continue
		}
		frag.Assignments = append(frag.Assignments, Assignment{
			Section: section,
			Key:     strings.TrimSpace(line[:eq]),
			Value:   strings.TrimSpace(line[eq+1:]),
			File:    path,
			Line:    startLine,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if pending.Len() > 0 {
		// A file ending in a continuation: treat the accumulated text as a
		// final line rather than discarding it.
		line := strings.TrimSpace(pending.String())
		if eq := strings.IndexByte(line, '='); eq > 0 && section != "" {
			frag.Assignments = append(frag.Assignments, Assignment{
				Section: section,
				Key:     strings.TrimSpace(line[:eq]),
				Value:   strings.TrimSpace(line[eq+1:]),
				File:    path,
				Line:    pendingLine,
			})
		}
	}
	return frag, nil
}

// LexBytes is Lex over an in-memory buffer.
func LexBytes(path string, b []byte) (*Fragment, error) { return Lex(path, bytes.NewReader(b)) }

// splitContinuation reports whether the line ends in an unescaped backslash and
// returns the body without it.
func splitContinuation(line string) (bool, string) {
	trimmed := strings.TrimRight(line, " \t")
	if !strings.HasSuffix(trimmed, `\`) {
		return false, line
	}
	// An even number of trailing backslashes is an escaped backslash, not a
	// continuation.
	n := 0
	for i := len(trimmed) - 1; i >= 0 && trimmed[i] == '\\'; i-- {
		n++
	}
	if n%2 == 0 {
		return false, line
	}
	return true, trimmed[:len(trimmed)-1]
}
