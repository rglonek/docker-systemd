package manager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"docker-systemd/internal/logging"
	"docker-systemd/internal/unitfile"
)

// testManager builds a manager rooted at a temporary directory, so enable,
// disable and mask operate on a fake filesystem rather than the host's.
func testManager(t *testing.T, files map[string]string) *Manager {
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
	// Keep %m off the host's /etc/machine-id.
	old := unitfile.MachineIDPath
	unitfile.MachineIDPath = filepath.Join(root, "etc/machine-id")
	t.Cleanup(func() { unitfile.MachineIDPath = old })

	opts := DefaultOptions()
	opts.Root = root
	m := New(opts)
	m.log = logging.New(os.Stderr, logging.LevelError, "test")
	m.machineID = "test-machine"
	m.bootID = "test-boot"
	m.loadRegistry()
	return m
}

func (m *Manager) root() string { return m.opts.Root }

// Defect B18: enable wrote a regular file containing "OK", always into
// multi-user.target.wants regardless of [Install] WantedBy=.
func TestEnableCreatesSymlinkIntoTheNamedTarget(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/web.service": "[Service]\nExecStart=/bin/web\n\n" +
			"[Install]\nWantedBy=graphical.target\n",
	})
	changes, err := m.enable([]string{"web.service"}, false)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes; want 1: %v", len(changes), changes)
	}
	link := filepath.Join(m.root(), "etc/systemd/system/graphical.target.wants/web.service")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("the enable symlink was not created: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("enable must create a symlink, not a regular file")
	}
	dest, _ := os.Readlink(link)
	if filepath.Base(dest) != "web.service" {
		t.Errorf("symlink points at %q", dest)
	}
	// It must not have gone into multi-user.target.wants.
	if _, err := os.Stat(filepath.Join(m.root(),
		"etc/systemd/system/multi-user.target.wants/web.service")); err == nil {
		t.Error("enable ignored [Install] WantedBy= and used multi-user.target")
	}
}

func TestEnableHonoursRequiredByAliasAndAlso(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/a.service": "[Service]\nExecStart=/bin/a\n\n" +
			"[Install]\nRequiredBy=multi-user.target\nAlias=alias-a.service\nAlso=b.service\n",
		"lib/systemd/system/b.service": "[Service]\nExecStart=/bin/b\n\n" +
			"[Install]\nWantedBy=multi-user.target\n",
	})
	if _, err := m.enable([]string{"a.service"}, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	for _, p := range []string{
		"etc/systemd/system/multi-user.target.requires/a.service",
		"etc/systemd/system/alias-a.service",
		"etc/systemd/system/multi-user.target.wants/b.service",
	} {
		if _, err := os.Lstat(filepath.Join(m.root(), p)); err != nil {
			t.Errorf("%s was not created: %v", p, err)
		}
	}
}

// A unit with no [Install] must fail the way systemd does: several maintainer
// scripts branch on that exact message.
func TestEnableWithoutInstallSectionFails(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/static.service": "[Service]\nExecStart=/bin/s\n",
	})
	_, err := m.enable([]string{"static.service"}, false)
	if err == nil {
		t.Fatal("enable should have failed")
	}
	if !strings.Contains(err.Error(), "no installation config") {
		t.Errorf("error = %q; want systemd's wording", err.Error())
	}
}

func TestEnableIsIdempotent(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/i.service": "[Service]\nExecStart=/bin/i\n\n" +
			"[Install]\nWantedBy=multi-user.target\n",
	})
	if _, err := m.enable([]string{"i.service"}, false); err != nil {
		t.Fatalf("first enable: %v", err)
	}
	if _, err := m.enable([]string{"i.service"}, false); err != nil {
		t.Fatalf("second enable should be a no-op, got: %v", err)
	}
}

// disable removes only the link, never the unit file it points at.
func TestDisableRemovesOnlyTheLink(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/d.service": "[Service]\nExecStart=/bin/d\n\n" +
			"[Install]\nWantedBy=multi-user.target\n",
	})
	if _, err := m.enable([]string{"d.service"}, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := m.disable([]string{"d.service"}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(m.root(),
		"etc/systemd/system/multi-user.target.wants/d.service")); err == nil {
		t.Error("the enable symlink should have been removed")
	}
	if _, err := os.Stat(filepath.Join(m.root(), "lib/systemd/system/d.service")); err != nil {
		t.Errorf("disable must not touch the unit file itself: %v", err)
	}
}

// Defect B13: delete-instance called RemoveAll on every path of the unit,
// which deleted the distro's unit file for a non-template unit.
func TestDeleteInstanceOnlyRemovesTheEnableSymlink(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/keep.service": "[Service]\nExecStart=/bin/keep\n\n" +
			"[Install]\nWantedBy=multi-user.target\n",
	})
	if _, err := m.enable([]string{"keep.service"}, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// delete-instance is wired to disable.
	if _, err := m.disable([]string{"keep.service"}); err != nil {
		t.Fatalf("delete-instance: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.root(), "lib/systemd/system/keep.service")); err != nil {
		t.Fatalf("the distro's unit file was deleted: %v", err)
	}
}

// Defect B4: mask symlinked to the literal string "target", creating a file
// called `target` in init's working directory.
func TestMaskCreatesTheRightSymlink(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/m.service": "[Service]\nExecStart=/bin/m\n",
	})
	if _, err := m.mask([]string{"m.service"}); err != nil {
		t.Fatalf("mask: %v", err)
	}
	link := filepath.Join(m.root(), "etc/systemd/system/m.service")
	dest, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("the mask symlink was not created: %v", err)
	}
	if dest != os.DevNull {
		t.Errorf("mask points at %q; want %s", dest, os.DevNull)
	}
	if _, err := os.Lstat("target"); err == nil {
		t.Error("a file named `target` was created in the working directory")
		_ = os.Remove("target")
	}

	// The mask must be visible after a reload, derived from the filesystem
	// rather than cached.
	m.loadRegistry()
	if got := m.Registry().UnitFileState("m.service"); got != "masked" {
		t.Errorf("UnitFileState after reload = %q; want masked", got)
	}

	if _, err := m.unmask([]string{"m.service"}); err != nil {
		t.Fatalf("unmask: %v", err)
	}
	m.loadRegistry()
	if got := m.Registry().UnitFileState("m.service"); got == "masked" {
		t.Error("the unit is still masked after unmask")
	}
}

func TestMaskRefusesToClobberAnExistingFile(t *testing.T) {
	m := testManager(t, map[string]string{
		"etc/systemd/system/x.service": "[Service]\nExecStart=/bin/x\n",
	})
	_, err := m.mask([]string{"x.service"})
	if err == nil {
		t.Fatal("mask should refuse to overwrite an administrator fragment")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestAddWantsCreatesTheEdge(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/w.service": "[Service]\nExecStart=/bin/w\n",
	})
	if _, err := m.addDep([]string{"multi-user.target"}, []string{"w.service"}, "wants"); err != nil {
		t.Fatalf("add-wants: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(m.root(),
		"etc/systemd/system/multi-user.target.wants/w.service")); err != nil {
		t.Errorf("add-wants did not create the link: %v", err)
	}
}

func TestRevertRemovesAdministratorOverrides(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/r.service":             "[Service]\nExecStart=/bin/vendor\n",
		"etc/systemd/system/r.service":             "[Service]\nExecStart=/bin/admin\n",
		"etc/systemd/system/r.service.d/over.conf": "[Service]\nRestart=always\n",
	})
	if _, err := m.revert([]string{"r.service"}); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.root(), "etc/systemd/system/r.service")); err == nil {
		t.Error("the administrator fragment should have been removed")
	}
	if _, err := os.Stat(filepath.Join(m.root(), "etc/systemd/system/r.service.d")); err == nil {
		t.Error("the administrator drop-in directory should have been removed")
	}
	m.loadRegistry()
	u := m.Get("r.service")
	if u == nil {
		t.Fatal("the vendor unit should still be there")
	}
	if argv := u.Service.ExecStart[0].Expand(nil); argv[0] != "/bin/vendor" {
		t.Errorf("ExecStart = %q; the vendor unit should be in effect", argv[0])
	}
}

// A unit's [Install] WantedBy= contributes a Wants= edge once it is enabled.
func TestInjectedWantsFollowsEnableState(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/e.service": "[Service]\nExecStart=/bin/e\n\n" +
			"[Install]\nWantedBy=multi-user.target\n",
	})
	if got := m.InjectedWants("multi-user.target"); contains(got, "e.service") {
		t.Error("a disabled unit must not be wanted by its install target")
	}
	if _, err := m.enable([]string{"e.service"}, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	m.loadRegistry()
	if got := m.InjectedWants("multi-user.target"); !contains(got, "e.service") {
		t.Errorf("InjectedWants = %v; want e.service after enabling", got)
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
