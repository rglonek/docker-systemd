package unitfile

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func parseUnit(t *testing.T, name, content string) *Unit {
	t.Helper()
	frag, err := LexBytes("/test/"+name, []byte(content))
	if err != nil {
		t.Fatalf("Lex: %v", err)
	}
	p := NewParser(name, DefaultSpecifierContext(name))
	p.unit.LoadState = LoadLoaded
	p.Apply(frag)
	return p.Finish()
}

func TestParseBasicService(t *testing.T) {
	u := parseUnit(t, "test.service", `
[Unit]
Description=A test service
After=network.target
Wants=cron

[Service]
Type=forking
PIDFile=/run/test.pid
RemainAfterExit=yes
ExecStart=/usr/sbin/test -D
TimeoutStopSec=30
LimitNOFILE=100000

[Install]
WantedBy=multi-user.target
`)
	if u.Unit.Description != "A test service" {
		t.Errorf("Description = %q", u.Unit.Description)
	}
	if !reflect.DeepEqual(u.Unit.After, []string{"network.target"}) {
		t.Errorf("After = %v", u.Unit.After)
	}
	// A bare dependency name gains the implicit .service suffix.
	if !reflect.DeepEqual(u.Unit.Wants, []string{"cron.service"}) {
		t.Errorf("Wants = %v", u.Unit.Wants)
	}
	if u.Service.Type != TypeForking {
		t.Errorf("Type = %q", u.Service.Type)
	}
	if !u.Service.RemainAfterExit {
		t.Error("RemainAfterExit=yes must parse as true (defect C2)")
	}
	if u.Service.TimeoutStopSec != 30*time.Second {
		t.Errorf("TimeoutStopSec = %v", u.Service.TimeoutStopSec)
	}
	if l, ok := u.Service.Limits["NOFILE"]; !ok || l.Soft != 100000 {
		t.Errorf("LimitNOFILE = %+v, ok=%v", l, ok)
	}
	if !reflect.DeepEqual(u.Install.WantedBy, []string{"multi-user.target"}) {
		t.Errorf("WantedBy = %v", u.Install.WantedBy)
	}
}

// systemd's defaults, two of which v0.5.x got wrong (C11, C12).
func TestServiceDefaults(t *testing.T) {
	u := parseUnit(t, "d.service", "[Service]\nExecStart=/bin/true\n")
	if u.Service.TimeoutStopSec != 90*time.Second {
		t.Errorf("default TimeoutStopSec = %v; want 90s", u.Service.TimeoutStopSec)
	}
	if u.Service.TimeoutStartSec != 90*time.Second {
		t.Errorf("default TimeoutStartSec = %v; want 90s", u.Service.TimeoutStartSec)
	}
	if u.Service.RestartSec != 100*time.Millisecond {
		t.Errorf("default RestartSec = %v; want 100ms", u.Service.RestartSec)
	}
	if u.Service.KillMode != KillControlGroup {
		t.Errorf("default KillMode = %q; want control-group", u.Service.KillMode)
	}
	if !u.Service.SendSIGKILL {
		t.Error("SendSIGKILL should default to yes")
	}
}

// Defect C10: v0.5.x accepted several ExecStart= lines for a non-oneshot type
// and started them in parallel.
func TestMultipleExecStartRejected(t *testing.T) {
	u := parseUnit(t, "multi.service", `
[Service]
Type=simple
ExecStart=/bin/one
ExecStart=/bin/two
`)
	if u.LoadState != LoadBadSetting {
		t.Errorf("LoadState = %q; want bad-setting", u.LoadState)
	}
	if !strings.Contains(u.LoadError, "only be specified once") {
		t.Errorf("LoadError = %q", u.LoadError)
	}
}

func TestMultipleExecStartAllowedForOneshot(t *testing.T) {
	u := parseUnit(t, "one.service", `
[Service]
Type=oneshot
ExecStart=/bin/one
ExecStart=/bin/two
`)
	if u.LoadState != LoadLoaded {
		t.Errorf("LoadState = %q; want loaded", u.LoadState)
	}
	if len(u.Service.ExecStart) != 2 {
		t.Errorf("got %d ExecStart lines; want 2", len(u.Service.ExecStart))
	}
}

// Defect B20: Type=oneshot must not force RemainAfterExit.
func TestOneshotDoesNotForceRemainAfterExit(t *testing.T) {
	u := parseUnit(t, "one.service", "[Service]\nType=oneshot\nExecStart=/bin/true\n")
	if u.Service.RemainAfterExit {
		t.Error("Type=oneshot must not imply RemainAfterExit=yes")
	}
}

// Defect C4: several space-separated assignments per Environment= line, with
// quoting.
func TestEnvironmentDirective(t *testing.T) {
	u := parseUnit(t, "e.service", `
[Service]
ExecStart=/bin/true
Environment="A=1 2" B=3
Environment=C=4
`)
	want := []string{"A=1 2", "B=3", "C=4"}
	if !reflect.DeepEqual(u.Service.Environment, want) {
		t.Errorf("Environment = %q; want %q", u.Service.Environment, want)
	}
}

// An empty assignment resets a list; that is how a drop-in subtracts (05 §2).
func TestEmptyAssignmentResetsList(t *testing.T) {
	u := parseUnit(t, "r.service", `
[Unit]
Wants=a.service b.service
Wants=
Wants=c.service

[Service]
ExecStart=/bin/one
ExecStart=
ExecStart=/bin/two
`)
	if !reflect.DeepEqual(u.Unit.Wants, []string{"c.service"}) {
		t.Errorf("Wants = %v; want [c.service]", u.Unit.Wants)
	}
	if len(u.Service.ExecStart) != 1 {
		t.Fatalf("got %d ExecStart lines; want 1", len(u.Service.ExecStart))
	}
	if u.LoadState == LoadBadSetting {
		t.Error("the reset should have cleared the multiplicity counter too")
	}
}

// Line continuations join onto one logical assignment.
func TestContinuation(t *testing.T) {
	u := parseUnit(t, "c.service", "[Service]\nExecStart=/bin/foo \\\n  --flag \\\n  --other\n")
	argv := u.Service.ExecStart[0].Expand(nil)
	want := []string{"/bin/foo", "--flag", "--other"}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %q; want %q", argv, want)
	}
}

// Unknown sections are skipped entirely along with their contents.
func TestUnknownSectionSkipped(t *testing.T) {
	u := parseUnit(t, "u.service", `
[Service]
ExecStart=/bin/true

[Socket]
ListenStream=1234
`)
	for _, w := range u.Warnings {
		if w.Directive == "ListenStream" {
			t.Error("a directive inside an unknown section should not be warned about")
		}
	}
}

// Silence is the wrong answer for a hardening directive: an operator will
// assume it took effect (05 §8).
func TestUnsupportedDirectiveWarns(t *testing.T) {
	u := parseUnit(t, "h.service", "[Service]\nExecStart=/bin/true\nPrivateTmp=yes\n")
	found := false
	for _, w := range u.Warnings {
		if w.Directive == "PrivateTmp" && w.Action == "ignored" {
			found = true
			if !strings.Contains(w.Reason, "CAP_SYS_ADMIN") {
				t.Errorf("reason = %q; expected it to name the missing capability", w.Reason)
			}
		}
	}
	if !found {
		t.Error("PrivateTmp= must produce exactly one warning naming the directive")
	}
}

func TestUnknownDirectiveWarns(t *testing.T) {
	u := parseUnit(t, "k.service", "[Service]\nExecStart=/bin/true\nNoSuchThing=1\n")
	found := false
	for _, w := range u.Warnings {
		if w.Directive == "NoSuchThing" {
			found = true
			if w.Line != 3 {
				t.Errorf("warning line = %d; want 3", w.Line)
			}
		}
	}
	if !found {
		t.Error("an unknown directive should produce a warning naming its line")
	}
}

// Defect C9: ExecStopPre= is not a systemd directive.
func TestExecStopPreDeprecated(t *testing.T) {
	u := parseUnit(t, "s.service", "[Service]\nExecStart=/bin/true\nExecStopPre=/bin/pre\n")
	found := false
	for _, w := range u.Warnings {
		if w.Directive == "ExecStopPre" && strings.Contains(w.Reason, "not a systemd directive") {
			found = true
		}
	}
	if !found {
		t.Error("ExecStopPre= should be accepted with a deprecation warning")
	}
	if len(u.Service.ExecStop) != 1 {
		t.Error("ExecStopPre= should map onto ExecStop=")
	}
}

func TestBadBooleanWarnsRatherThanSilentlyFalse(t *testing.T) {
	u := parseUnit(t, "b.service", "[Service]\nExecStart=/bin/true\nRemainAfterExit=maybe\n")
	found := false
	for _, w := range u.Warnings {
		if w.Directive == "RemainAfterExit" {
			found = true
		}
	}
	if !found {
		t.Error("an invalid boolean must warn, not silently read as false")
	}
}

func TestConditionsParsed(t *testing.T) {
	u := parseUnit(t, "c.service", `
[Unit]
ConditionPathExists=/etc/passwd
ConditionVirtualization=!container
AssertPathExists=/etc/hostname

[Service]
ExecStart=/bin/true
`)
	if len(u.Unit.Conditions) != 3 {
		t.Fatalf("got %d conditions; want 3", len(u.Unit.Conditions))
	}
	if u.Unit.Conditions[1].Kind != "Virtualization" || !u.Unit.Conditions[1].Negate {
		t.Errorf("condition 1 = %+v; want a negated Virtualization", u.Unit.Conditions[1])
	}
	if !u.Unit.Conditions[2].Assert {
		t.Error("Assert*= must be marked as an assertion")
	}
}

func TestServiceSectionInTargetIsSkipped(t *testing.T) {
	u := parseUnit(t, "t.target", "[Unit]\nDescription=T\n\n[Service]\nExecStart=/bin/true\n")
	if u.Service != nil {
		t.Error("a target must have no [Service] section")
	}
	if u.Unit.Description != "T" {
		t.Errorf("Description = %q", u.Unit.Description)
	}
}

func TestNoExecStartFailsLoad(t *testing.T) {
	u := parseUnit(t, "n.service", "[Unit]\nDescription=n\n\n[Service]\nType=simple\n")
	if u.LoadState != LoadBadSetting {
		t.Errorf("LoadState = %q; want bad-setting", u.LoadState)
	}
}

func FuzzLex(f *testing.F) {
	f.Add("[Service]\nExecStart=/bin/true\n")
	f.Add("[Unit]\nDescription=x \\\n y\n")
	f.Add("=\n[\n")
	f.Fuzz(func(t *testing.T, s string) {
		frag, err := LexBytes("fuzz", []byte(s))
		if err != nil {
			return
		}
		p := NewParser("fuzz.service", DefaultSpecifierContext("fuzz.service"))
		p.Apply(frag)
		p.Finish()
	})
}
