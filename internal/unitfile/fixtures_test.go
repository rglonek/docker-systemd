package unitfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The harvested unit files from the supported base images must parse with no
// unexpected warnings — the Phase 2 acceptance criterion. A warning is expected
// only when it names a directive we deliberately reject (the sandboxing and
// resource-control families) or one we silently ignore.
func TestHarvestedUnitFilesParseCleanly(t *testing.T) {
	dir := filepath.Join("..", "..", "test", "units")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no fixture directory: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.Contains(e.Name(), ".service") {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			path := filepath.Join(dir, e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			frag, err := LexBytes(path, data)
			if err != nil {
				t.Fatalf("Lex: %v", err)
			}
			name := e.Name()
			if strings.Contains(name, "@") {
				// Parse a template as a concrete instance so specifier
				// expansion is exercised.
				name = strings.Replace(name, "@.", "@test.", 1)
			}
			p := NewParser(name, DefaultSpecifierContext(name))
			p.unit.LoadState = LoadLoaded
			p.Apply(frag)
			u := p.Finish()

			if u.LoadState != LoadLoaded {
				t.Fatalf("LoadState = %q (%s)", u.LoadState, u.LoadError)
			}
			for _, w := range u.Warnings {
				if _, known := unsupportedDirectives[w.Directive]; known {
					continue
				}
				if w.Directive == "Type" || w.Directive == "BusName" {
					continue // the documented Type=dbus degradation
				}
				t.Errorf("unexpected warning: %s", w)
			}
			if u.Service != nil && u.Service.Type != TypeOneshot && len(u.Service.ExecStart) != 1 {
				t.Errorf("got %d ExecStart lines; a non-oneshot unit must have exactly one",
					len(u.Service.ExecStart))
			}
		})
	}
}

// Specific properties of the harvested units that the design calls out.
func TestNginxFixtureShape(t *testing.T) {
	u := loadFixture(t, "nginx.service")
	if u.Service.Type != TypeForking {
		t.Errorf("Type = %q", u.Service.Type)
	}
	if u.Service.PIDFile != "/run/nginx.pid" {
		t.Errorf("PIDFile = %q", u.Service.PIDFile)
	}
	if u.Service.KillMode != KillMixed {
		t.Errorf("KillMode = %q", u.Service.KillMode)
	}
	// The single-quoted argument must survive as one argv element.
	argv := u.Service.ExecStart[0].Expand(nil)
	if len(argv) != 3 || argv[2] != "daemon on; master_process on;" {
		t.Errorf("argv = %q; the quoted argument must stay intact", argv)
	}
	// ExecStop carries the `-` prefix.
	if len(u.Service.ExecStop) != 1 || !u.Service.ExecStop[0].IgnoreFailure {
		t.Errorf("ExecStop should carry the ignore-failure prefix: %+v", u.Service.ExecStop)
	}
}

func TestRedisFixtureShape(t *testing.T) {
	u := loadFixture(t, "redis-server.service")
	if u.Service.Type != TypeNotify {
		t.Errorf("Type = %q; Type=notify must be honoured, not treated as forking", u.Service.Type)
	}
	if u.Service.User != "redis" {
		t.Errorf("User = %q", u.Service.User)
	}
	// LimitNOFILE is the one that matters in practice and must be parsed
	// rather than warned away.
	if l, ok := u.Service.Limits["NOFILE"]; !ok || l.Soft != 65535 {
		t.Errorf("LimitNOFILE = %+v ok=%v", l, ok)
	}
	if !u.Service.NoNewPrivileges {
		t.Error("NoNewPrivileges=true should be honoured; it is free")
	}
	if u.Service.UMask == nil || *u.Service.UMask != 0o077 {
		t.Errorf("UMask = %v", u.Service.UMask)
	}
	if len(u.Service.RuntimeDirectory) != 1 || u.Service.RuntimeDirectory[0] != "redis" {
		t.Errorf("RuntimeDirectory = %v", u.Service.RuntimeDirectory)
	}
	// The sandboxing directives must each produce a warning rather than
	// silence.
	warned := map[string]bool{}
	for _, w := range u.Warnings {
		warned[w.Directive] = true
	}
	for _, d := range []string{"PrivateTmp", "ProtectHome", "CapabilityBoundingSet",
		"MemoryDenyWriteExecute", "RestrictAddressFamilies"} {
		if !warned[d] {
			t.Errorf("%s= was ignored silently; it must warn", d)
		}
	}
	if !contains(u.Install.Alias, "redis.service") {
		t.Errorf("Alias = %v", u.Install.Alias)
	}
}

func TestSSHFixtureShape(t *testing.T) {
	u := loadFixture(t, "ssh.service")
	if u.Service.Type != TypeNotify {
		t.Errorf("Type = %q", u.Service.Type)
	}
	if len(u.Service.ExecReload) != 2 {
		t.Errorf("got %d ExecReload lines; want 2", len(u.Service.ExecReload))
	}
	if len(u.Unit.Conditions) != 1 || !u.Unit.Conditions[0].Negate {
		t.Errorf("the negated ConditionPathExists was not parsed: %+v", u.Unit.Conditions)
	}
	if len(u.Service.EnvironmentFiles) != 1 || !u.Service.EnvironmentFiles[0].Optional {
		t.Errorf("EnvironmentFile= should be optional: %+v", u.Service.EnvironmentFiles)
	}
	if u.Service.RestartPreventExitStatus.Empty() {
		t.Error("RestartPreventExitStatus=255 was not parsed")
	}
}

func TestGettyTemplateFixtureShape(t *testing.T) {
	u := loadFixtureAs(t, "getty@.service", "getty@tty1.service")
	if u.Instance != "tty1" {
		t.Errorf("Instance = %q", u.Instance)
	}
	if u.Unit.Description != "Getty on tty1" {
		t.Errorf("Description = %q; %%I should have expanded", u.Unit.Description)
	}
	if u.Service.Type != TypeIdle {
		t.Errorf("Type = %q", u.Service.Type)
	}
	if !u.Service.ExecStart[0].IgnoreFailure {
		t.Error("the `-` prefix on ExecStart= was not honoured")
	}
	if !u.Service.SendSIGHUP {
		t.Error("SendSIGHUP=yes was not honoured")
	}
	if u.Install.DefaultInstance != "tty1" {
		t.Errorf("DefaultInstance = %q", u.Install.DefaultInstance)
	}
}

func TestMariaDBFixtureShape(t *testing.T) {
	u := loadFixture(t, "mariadb.service")
	if u.Service.TimeoutStartSec.Seconds() != 900 {
		t.Errorf("TimeoutStartSec = %v", u.Service.TimeoutStartSec)
	}
	if u.Service.Restart != RestartOnAbort {
		t.Errorf("Restart = %q", u.Service.Restart)
	}
	if u.Service.OOMScoreAdjust == nil || *u.Service.OOMScoreAdjust != -600 {
		t.Errorf("OOMScoreAdjust = %v", u.Service.OOMScoreAdjust)
	}
	if l, ok := u.Service.Limits["CORE"]; !ok || l.Soft == 0 {
		t.Errorf("LimitCORE=infinity was not parsed: %+v ok=%v", l, ok)
	}
	if len(u.Service.ExecStartPre) != 2 {
		t.Errorf("got %d ExecStartPre lines; want 2", len(u.Service.ExecStartPre))
	}
	if !u.Service.ExecStartPre[1].IgnoreFailure {
		t.Error("the `-` prefix on the second ExecStartPre was not honoured")
	}
}

func loadFixture(t *testing.T, name string) *Unit {
	t.Helper()
	return loadFixtureAs(t, name, name)
}

func loadFixtureAs(t *testing.T, file, unitName string) *Unit {
	t.Helper()
	path := filepath.Join("..", "..", "test", "units", file)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("fixture %s is unavailable: %v", file, err)
	}
	frag, err := LexBytes(path, data)
	if err != nil {
		t.Fatalf("Lex: %v", err)
	}
	p := NewParser(unitName, DefaultSpecifierContext(unitName))
	p.unit.LoadState = LoadLoaded
	p.Apply(frag)
	return p.Finish()
}
