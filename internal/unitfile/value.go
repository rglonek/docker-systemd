package unitfile

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Infinity is the duration produced by the literal `infinity`, meaning "no
// timeout". It is deliberately large rather than zero, because zero already
// means "immediately" for RestartSec=.
const Infinity = time.Duration(math.MaxInt64)

// ParseBool implements systemd's boolean grammar. The v0.5.x parser accepted
// only the literal `true`, so `RemainAfterExit=yes` — the spelling used by
// essentially every real unit file — silently read as false (defect C2).
func ParseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "yes", "true", "on":
		return true, nil
	case "0", "no", "false", "off":
		return false, nil
	}
	return false, fmt.Errorf("invalid boolean %q (want 1/yes/true/on or 0/no/false/off)", s)
}

// durationUnits maps every spelling systemd accepts onto its length. Longest
// spellings must be tried first when matching, which sortedDurationUnits
// guarantees.
var durationUnits = map[string]time.Duration{
	"usec":    time.Microsecond,
	"us":      time.Microsecond,
	"µs":      time.Microsecond,
	"μs":      time.Microsecond,
	"msec":    time.Millisecond,
	"ms":      time.Millisecond,
	"seconds": time.Second,
	"second":  time.Second,
	"sec":     time.Second,
	"s":       time.Second,
	"minutes": time.Minute,
	"minute":  time.Minute,
	"min":     time.Minute,
	"m":       time.Minute,
	"hours":   time.Hour,
	"hour":    time.Hour,
	"hr":      time.Hour,
	"h":       time.Hour,
	"days":    24 * time.Hour,
	"day":     24 * time.Hour,
	"d":       24 * time.Hour,
	"weeks":   7 * 24 * time.Hour,
	"week":    7 * 24 * time.Hour,
	"w":       7 * 24 * time.Hour,
	"months":  time.Duration(30.44 * 24 * float64(time.Hour)),
	"month":   time.Duration(30.44 * 24 * float64(time.Hour)),
	"M":       time.Duration(30.44 * 24 * float64(time.Hour)),
	"years":   time.Duration(365.25 * 24 * float64(time.Hour)),
	"year":    time.Duration(365.25 * 24 * float64(time.Hour)),
	"y":       time.Duration(365.25 * 24 * float64(time.Hour)),
}

// sortedDurationUnits holds the unit spellings longest-first so that "seconds"
// is matched before "sec" and "s".
var sortedDurationUnits = func() []string {
	out := make([]string, 0, len(durationUnits))
	for k := range durationUnits {
		out = append(out, k)
	}
	// Insertion sort by descending length; the slice is tiny and this keeps
	// package init free of sort.Slice's reflection.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && len(out[j]) > len(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}()

// ParseDuration implements systemd's time-span grammar: a sequence of
// `<value>[unit]` terms, summed, where a bare number means seconds and the
// literal `infinity` means "no timeout".
//
//	"5min 20s"  -> 320s
//	"100ms"     -> 100ms
//	"90"        -> 90s
//	"infinity"  -> Infinity
func ParseDuration(s string) (time.Duration, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return 0, fmt.Errorf("empty time span")
	}
	if strings.EqualFold(in, "infinity") {
		return Infinity, nil
	}
	var total time.Duration
	var terms int
	i := 0
	for i < len(in) {
		for i < len(in) && (in[i] == ' ' || in[i] == '\t') {
			i++
		}
		if i >= len(in) {
			break
		}
		start := i
		for i < len(in) && (in[i] >= '0' && in[i] <= '9') {
			i++
		}
		fracStart := i
		if i < len(in) && in[i] == '.' {
			i++
			for i < len(in) && (in[i] >= '0' && in[i] <= '9') {
				i++
			}
		}
		if i == start {
			return 0, fmt.Errorf("invalid time span %q: expected a number at offset %d", s, start)
		}
		value, err := strconv.ParseFloat(in[start:i], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid time span %q: %v", s, err)
		}
		_ = fracStart
		for i < len(in) && (in[i] == ' ' || in[i] == '\t') {
			i++
		}
		unit := time.Second
		matched := ""
		for _, u := range sortedDurationUnits {
			if strings.HasPrefix(in[i:], u) {
				// A unit must not be a prefix of a longer alphabetic run:
				// "5secx" is invalid, not 5s followed by garbage.
				rest := in[i+len(u):]
				if rest == "" || !isAlpha(rest[0]) {
					matched = u
					break
				}
			}
		}
		if matched != "" {
			unit = durationUnits[matched]
			i += len(matched)
		}
		total += time.Duration(value * float64(unit))
		terms++
	}
	if terms == 0 {
		return 0, fmt.Errorf("invalid time span %q", s)
	}
	if total < 0 {
		return 0, fmt.Errorf("negative time span %q", s)
	}
	return total, nil
}

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// ParseSize implements the size grammar used by LimitFSIZE= and LogSizeMax=:
// K/M/G/T are binary multiples, KB/MB/GB/TB decimal ones.
func ParseSize(s string) (int64, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return 0, fmt.Errorf("empty size")
	}
	if strings.EqualFold(in, "infinity") {
		return math.MaxInt64, nil
	}
	i := 0
	for i < len(in) && (in[i] >= '0' && in[i] <= '9' || in[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	value, err := strconv.ParseFloat(in[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %v", s, err)
	}
	var mult float64 = 1
	switch strings.TrimSpace(in[i:]) {
	case "", "B":
	case "K", "k":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	case "KB", "kB":
		mult = 1e3
	case "MB":
		mult = 1e6
	case "GB":
		mult = 1e9
	case "TB":
		mult = 1e12
	default:
		return 0, fmt.Errorf("invalid size suffix %q", in[i:])
	}
	return int64(value * mult), nil
}

// ExitStatusSet is the value of SuccessExitStatus=,
// RestartPreventExitStatus= and RestartForceExitStatus=: a set of exit codes
// and signal names.
type ExitStatusSet struct {
	Codes   []int    `json:"codes,omitempty"`
	Signals []string `json:"signals,omitempty"`
}

// Contains reports whether an exit is a member of the set. A process killed by
// a signal is matched against Signals; a normal exit against Codes.
func (e ExitStatusSet) Contains(code, signal int) bool {
	if signal != 0 {
		name := SignalName(signal)
		for _, s := range e.Signals {
			if strings.EqualFold(s, name) || strings.EqualFold("SIG"+s, name) {
				return true
			}
		}
		return false
	}
	for _, c := range e.Codes {
		if c == code {
			return true
		}
	}
	return false
}

// Empty reports whether the set has no members.
func (e ExitStatusSet) Empty() bool { return len(e.Codes) == 0 && len(e.Signals) == 0 }

// ParseExitStatusInto adds the space-separated members of s to the set.
func ParseExitStatusInto(set *ExitStatusSet, s string) error {
	for _, f := range strings.Fields(s) {
		if n, err := strconv.Atoi(f); err == nil {
			if n < 0 || n > 255 {
				return fmt.Errorf("exit status %d out of range 0-255", n)
			}
			set.Codes = append(set.Codes, n)
			continue
		}
		if SignalNumber(f) == 0 {
			return fmt.Errorf("unknown exit status or signal %q", f)
		}
		set.Signals = append(set.Signals, strings.TrimPrefix(strings.ToUpper(f), "SIG"))
	}
	return nil
}

// ParseIntRange parses a decimal integer and checks it against bounds.
func ParseIntRange(s string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", s)
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("value %d out of range %d..%d", n, lo, hi)
	}
	return n, nil
}

// ParseMode parses an octal file mode as used by UMask= and
// RuntimeDirectoryMode=.
func ParseMode(s string) (uint32, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 8, 32)
	if err != nil || n > 0o7777 {
		return 0, fmt.Errorf("invalid mode %q (want octal, e.g. 0755)", s)
	}
	return uint32(n), nil
}
