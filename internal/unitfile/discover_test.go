package unitfile

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// tree builds a fake unit-file layout under a temporary root.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, content := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func loadTree(t *testing.T, root string) (*Loader, *Registry) {
	t.Helper()
	l := &Loader{Root: root, SpecifierBase: DefaultSpecifierContext("")}
	reg, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return l, reg
}

func TestLoadAndPrecedence(t *testing.T) {
	root := tree(t, map[string]string{
		"lib/systemd/system/foo.service": "[Service]\nExecStart=/bin/vendor\n",
		"etc/systemd/system/foo.service": "[Service]\nExecStart=/bin/admin\n",
	})
	_, reg := loadTree(t, root)
	u := reg.Get("foo.service")
	if u == nil {
		t.Fatal("foo.service was not loaded")
	}
	// A higher-precedence fragment completely replaces the lower one; the two
	// are never merged.
	argv := u.Service.ExecStart[0].Expand(nil)
	if argv[0] != "/bin/admin" {
		t.Errorf("ExecStart = %q; want the /etc fragment to win", argv[0])
	}
	if len(u.Service.ExecStart) != 1 {
		t.Errorf("got %d ExecStart lines; fragments must not merge", len(u.Service.ExecStart))
	}
}

func TestDropInsApplyOverFragment(t *testing.T) {
	root := tree(t, map[string]string{
		"lib/systemd/system/foo.service":             "[Service]\nExecStart=/bin/foo\nRestart=no\n",
		"etc/systemd/system/foo.service.d/10-a.conf": "[Service]\nRestart=always\n",
		"etc/systemd/system/foo.service.d/20-b.conf": "[Unit]\nDescription=overridden\n",
	})
	_, reg := loadTree(t, root)
	u := reg.Get("foo.service")
	if u == nil {
		t.Fatal("foo.service was not loaded")
	}
	if u.Service.Restart != RestartAlways {
		t.Errorf("Restart = %q; the drop-in should have won", u.Service.Restart)
	}
	if u.Unit.Description != "overridden" {
		t.Errorf("Description = %q", u.Unit.Description)
	}
	if len(u.DropInPaths) != 2 {
		t.Errorf("got %d drop-ins; want 2", len(u.DropInPaths))
	}
}

// Defect F13: the /run search-path entry and template drop-in directories were
// never scanned.
func TestRunDirectoryIsSearched(t *testing.T) {
	root := tree(t, map[string]string{
		"run/systemd/system/gen.service": "[Service]\nExecStart=/bin/generated\n",
	})
	_, reg := loadTree(t, root)
	if reg.Get("gen.service") == nil {
		t.Error("/run/systemd/system must be part of the search path")
	}
}

// Defect A5: two names resolving to one file are an alias, not a second
// half-loaded unit with a nil configuration.
func TestAliasSymlinkRegistersBothNames(t *testing.T) {
	root := tree(t, map[string]string{
		"lib/systemd/system/bar.service": "[Service]\nExecStart=/bin/bar\n",
	})
	if err := os.Symlink(filepath.Join(root, "lib/systemd/system/bar.service"),
		filepath.Join(root, "etc/systemd/system/foo.service")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, reg := loadTree(t, root)

	bar := reg.Get("bar.service")
	foo := reg.Get("foo.service")
	if bar == nil {
		t.Fatal("bar.service was not loaded")
	}
	if foo == nil {
		t.Fatal("the alias foo.service did not resolve")
	}
	if foo != bar {
		t.Error("an alias must resolve to the same unit, not a second one")
	}
	if bar.LoadState != LoadLoaded {
		t.Errorf("LoadState = %q; want loaded", bar.LoadState)
	}
	if !contains(bar.Names, "foo.service") {
		t.Errorf("Names = %v; the alias should be recorded", bar.Names)
	}
}

func TestMaskedBySymlinkToDevNull(t *testing.T) {
	root := tree(t, map[string]string{
		"lib/systemd/system/m.service": "[Service]\nExecStart=/bin/m\n",
	})
	if err := os.MkdirAll(filepath.Join(root, "etc/systemd/system"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", filepath.Join(root, "etc/systemd/system/m.service")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, reg := loadTree(t, root)
	u := reg.Get("m.service")
	if u == nil {
		t.Fatal("m.service missing from the registry")
	}
	if u.LoadState != LoadMasked {
		t.Errorf("LoadState = %q; want masked", u.LoadState)
	}
	if reg.UnitFileState("m.service") != "masked" {
		t.Errorf("UnitFileState = %q; want masked", reg.UnitFileState("m.service"))
	}
}

func TestMaskedByEmptyFile(t *testing.T) {
	root := tree(t, map[string]string{
		"etc/systemd/system/e.service": "",
	})
	_, reg := loadTree(t, root)
	if u := reg.Get("e.service"); u == nil || u.LoadState != LoadMasked {
		t.Errorf("an empty fragment must be treated as masked; got %+v", u)
	}
}

func TestWantsDirectoryEdges(t *testing.T) {
	root := tree(t, map[string]string{
		"lib/systemd/system/w.service":                            "[Service]\nExecStart=/bin/w\n\n[Install]\nWantedBy=multi-user.target\n",
		"etc/systemd/system/multi-user.target.wants/w.service":    "",
		"etc/systemd/system/multi-user.target.requires/w.service": "",
	})
	_, reg := loadTree(t, root)
	if got := reg.InjectedWants("multi-user.target"); !contains(got, "w.service") {
		t.Errorf("InjectedWants = %v; want w.service", got)
	}
	if got := reg.InjectedRequires("multi-user.target"); !contains(got, "w.service") {
		t.Errorf("InjectedRequires = %v; want w.service", got)
	}
	if reg.UnitFileState("w.service") != "enabled" {
		t.Errorf("a unit linked into a .wants directory should read as enabled, got %q",
			reg.UnitFileState("w.service"))
	}
}

func TestSyntheticTargetsExist(t *testing.T) {
	root := tree(t, map[string]string{})
	_, reg := loadTree(t, root)
	for _, name := range []string{"multi-user.target", "network.target", "network-online.target"} {
		u := reg.Get(name)
		if u == nil {
			t.Errorf("%s should be synthesised on demand", name)
			continue
		}
		if !u.Synthetic || u.LoadState != LoadLoaded {
			t.Errorf("%s: synthetic=%v load=%q", name, u.Synthetic, u.LoadState)
		}
	}
}

func TestRealTargetOverridesSynthetic(t *testing.T) {
	root := tree(t, map[string]string{
		"lib/systemd/system/multi-user.target": "[Unit]\nDescription=Real multi-user\n",
	})
	_, reg := loadTree(t, root)
	u := reg.Get("multi-user.target")
	if u == nil || u.Synthetic {
		t.Fatalf("a shipped target file must win over the synthetic one; got %+v", u)
	}
	if u.Unit.Description != "Real multi-user" {
		t.Errorf("Description = %q", u.Unit.Description)
	}
}

// Instances are resolved from their template, never materialised (05 §6).
func TestTemplateInstanceResolution(t *testing.T) {
	root := tree(t, map[string]string{
		"lib/systemd/system/getty@.service": "[Service]\nExecStart=/sbin/agetty %I\n",
	})
	l, reg := loadTree(t, root)
	u, err := l.ResolveTemplate(reg, "getty@tty1.service")
	if err != nil {
		t.Fatalf("ResolveTemplate: %v", err)
	}
	if u.Instance != "tty1" {
		t.Errorf("Instance = %q", u.Instance)
	}
	argv := u.Service.ExecStart[0].Expand(nil)
	if !reflect.DeepEqual(argv, []string{"/sbin/agetty", "tty1"}) {
		t.Errorf("argv = %q; %%I should have expanded to the instance", argv)
	}
	// The fragment path stays the template's, which is what `systemctl cat`
	// and `ls -l` show.
	if filepath.Base(u.FragmentPath) != "getty@.service" {
		t.Errorf("FragmentPath = %q; want the template file", u.FragmentPath)
	}
	// The template file must still be on disk: v0.5.x's delete-instance
	// removed it (defect B13).
	if _, err := os.Stat(u.FragmentPath); err != nil {
		t.Errorf("the template fragment must not be touched: %v", err)
	}
}

// Defect A6: a dependency naming a non-existent unit must resolve to absent,
// never to a null configuration.
func TestMissingUnitResolvesToNil(t *testing.T) {
	root := tree(t, map[string]string{})
	_, reg := loadTree(t, root)
	if u := reg.Get("nonexistent.service"); u != nil {
		t.Errorf("Get returned %+v for a unit that does not exist", u)
	}
	if reg.Has("nonexistent.service") {
		t.Error("Has should report false")
	}
}

// Defect A7: a unit requiring itself must load without deadlocking.
func TestSelfRequiringUnitLoads(t *testing.T) {
	root := tree(t, map[string]string{
		"etc/systemd/system/self.service": "[Unit]\nRequires=self.service\n\n[Service]\nExecStart=/bin/true\n",
	})
	_, reg := loadTree(t, root)
	u := reg.Get("self.service")
	if u == nil || u.LoadState != LoadLoaded {
		t.Fatalf("self.service should load cleanly; got %+v", u)
	}
	if !contains(u.Unit.Requires, "self.service") {
		t.Errorf("Requires = %v", u.Unit.Requires)
	}
}

// Defect A4: a fragment vanishing between reloads must not leave a unit with a
// null configuration.
func TestReloadAfterFragmentRemoved(t *testing.T) {
	root := tree(t, map[string]string{
		"etc/systemd/system/gone.service": "[Service]\nExecStart=/bin/gone\n",
	})
	l, reg := loadTree(t, root)
	if reg.Get("gone.service") == nil {
		t.Fatal("gone.service should be present before removal")
	}
	if err := os.Remove(filepath.Join(root, "etc/systemd/system/gone.service")); err != nil {
		t.Fatal(err)
	}
	reg2, err := l.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if u := reg2.Get("gone.service"); u != nil {
		t.Errorf("after removal Get should return nil, got %+v", u)
	}
	// The first snapshot is immutable and still usable.
	if reg.Get("gone.service") == nil {
		t.Error("an existing snapshot must not be mutated by a reload")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
