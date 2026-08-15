package unitfile

import (
	"math"
	"testing"
	"time"
)

// Defect C2: v0.5.x accepted only the literal `true`, so RemainAfterExit=yes —
// the spelling in essentially every real unit file — read as false.
func TestParseBool(t *testing.T) {
	trues := []string{"1", "yes", "true", "on", "YES", "True", "ON"}
	falses := []string{"0", "no", "false", "off", "NO", "False", "OFF"}
	for _, s := range trues {
		v, err := ParseBool(s)
		if err != nil || !v {
			t.Errorf("ParseBool(%q) = %v, %v; want true, nil", s, v, err)
		}
	}
	for _, s := range falses {
		v, err := ParseBool(s)
		if err != nil || v {
			t.Errorf("ParseBool(%q) = %v, %v; want false, nil", s, v, err)
		}
	}
	for _, s := range []string{"", "maybe", "2", "yes please"} {
		if _, err := ParseBool(s); err == nil {
			t.Errorf("ParseBool(%q) accepted an invalid boolean", s)
		}
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"90", 90 * time.Second},
		{"5min 20s", 320 * time.Second},
		{"100ms", 100 * time.Millisecond},
		{"1h", time.Hour},
		{"1hour", time.Hour},
		{"2 seconds", 2 * time.Second},
		{"1s 500ms", 1500 * time.Millisecond},
		{"1d", 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"0", 0},
		{"1.5s", 1500 * time.Millisecond},
		{"500us", 500 * time.Microsecond},
		{"infinity", Infinity},
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if err != nil {
			t.Errorf("ParseDuration(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDuration(%q) = %v; want %v", c.in, got, c.want)
		}
	}
	for _, s := range []string{"", "abc", "5secx", "-3s"} {
		if _, err := ParseDuration(s); err == nil {
			t.Errorf("ParseDuration(%q) accepted an invalid span", s)
		}
	}
}

// "seconds" must be matched before "sec" and "s", or "2 seconds" parses as
// 2 s followed by garbage.
func TestParseDurationAliasOrdering(t *testing.T) {
	for _, s := range []string{"2seconds", "2second", "2sec", "2s"} {
		got, err := ParseDuration(s)
		if err != nil {
			t.Fatalf("ParseDuration(%q): %v", s, err)
		}
		if got != 2*time.Second {
			t.Errorf("ParseDuration(%q) = %v; want 2s", s, got)
		}
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1024", 1024},
		{"1K", 1024},
		{"1KB", 1000},
		{"16M", 16 << 20},
		{"1G", 1 << 30},
		{"1GB", 1e9},
		{"infinity", math.MaxInt64},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d; want %d", c.in, got, c.want)
		}
	}
	if _, err := ParseSize("1Q"); err == nil {
		t.Error("ParseSize accepted an unknown suffix")
	}
}

func TestExitStatusSet(t *testing.T) {
	var set ExitStatusSet
	if err := ParseExitStatusInto(&set, "1 2 SIGKILL"); err != nil {
		t.Fatalf("ParseExitStatusInto: %v", err)
	}
	if !set.Contains(1, 0) || !set.Contains(2, 0) {
		t.Error("exit codes 1 and 2 should be members")
	}
	if set.Contains(3, 0) {
		t.Error("exit code 3 should not be a member")
	}
	if !set.Contains(0, int(9)) {
		t.Error("SIGKILL should be a member")
	}
	if err := ParseExitStatusInto(&set, "999"); err == nil {
		t.Error("an out-of-range exit status should be rejected")
	}
}

func TestSignalNumberAndName(t *testing.T) {
	if SignalNumber("SIGTERM") != 15 || SignalNumber("term") != 15 || SignalNumber("15") != 15 {
		t.Error("SIGTERM should resolve to 15 in every spelling")
	}
	if SignalNumber("SIGTREM") != 0 {
		t.Error("an unknown signal name must resolve to 0 so the directive fails loudly")
	}
	if SignalName(9) != "SIGKILL" {
		t.Errorf("SignalName(9) = %q; want SIGKILL", SignalName(9))
	}
}
