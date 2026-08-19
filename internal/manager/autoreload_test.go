package manager

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// waitForUnit polls the registry until the named unit appears, which is what
// an operator does after installing a package.
func waitForUnit(t *testing.T, m *Manager, name string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if m.Get(name) != nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return m.Get(name) != nil
}

// The watch set is the unit search path plus the subdirectories that also
// contribute to the registry. A `.wants` symlink produces no event on the
// parent directory, so watching only the top level would miss an enable.
func TestWatchDirsCoversDropinDirectories(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/a.service":                         "[Service]\nExecStart=/bin/a\n",
		"etc/systemd/system/multi-user.target.wants/a.service": "",
		"etc/systemd/system/a.service.d/override.conf":         "[Service]\nNice=5\n",
		"etc/systemd/system/notaunitdir/readme":                "",
	})
	root := m.root()

	got := map[string]bool{}
	for _, d := range m.watchDirs() {
		got[d] = true
	}
	for _, want := range []string{
		"lib/systemd/system",
		"etc/systemd/system",
		"etc/systemd/system/multi-user.target.wants",
		"etc/systemd/system/a.service.d",
	} {
		if !got[filepath.Join(root, want)] {
			t.Errorf("%s is not watched; watch set = %v", want, m.watchDirs())
		}
	}
	if got[filepath.Join(root, "etc/systemd/system/notaunitdir")] {
		t.Error("a subdirectory that is not a .wants/.requires/.d directory should not be watched")
	}
}

// A unit directory that does not exist yet cannot be watched, so its parent
// stands in until the directory appears. Without this, the first package to
// create /etc/systemd/system on an image that lacks it would go unnoticed.
func TestWatchDirsFallsBackToTheParentOfAMissingDirectory(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/a.service": "[Service]\nExecStart=/bin/a\n",
		"etc/systemd/keep":             "",
	})
	root := m.root()

	got := map[string]bool{}
	for _, d := range m.watchDirs() {
		got[d] = true
	}
	if got[filepath.Join(root, "etc/systemd/system")] {
		t.Error("a directory that does not exist should not be reported as watched")
	}
	if !got[filepath.Join(root, "etc/systemd")] {
		t.Errorf("the parent of the missing directory is not watched; watch set = %v", m.watchDirs())
	}
	// One level only: /etc is where every package writes.
	if got[filepath.Join(root, "etc")] {
		t.Error("the fallback must not climb as far as /etc")
	}
}

// The point of the feature: a unit file that appears on disk becomes visible
// without anyone running `systemctl daemon-reload`.
func TestAutoReloadPicksUpANewUnitFile(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/present.service": "[Service]\nExecStart=/bin/present\n",
	})
	m.startAutoReload()

	if m.Get("installed.service") != nil {
		t.Fatal("installed.service exists before the test wrote it")
	}
	writeUnit(t, m, "lib/systemd/system/installed.service",
		"[Unit]\nDescription=Installed by a package\n\n[Service]\nExecStart=/bin/installed\n")

	if !waitForUnit(t, m, "installed.service", 10*time.Second) {
		t.Fatal("a new unit file did not show up without an explicit daemon-reload")
	}
	if got := m.Get("installed.service").Unit.Description; got != "Installed by a package" {
		t.Errorf("Description = %q; the reload did not parse the new file", got)
	}
	if m.Get("present.service") == nil {
		t.Error("the reload lost a unit that was already loaded")
	}
}

// dpkg and rpm unpack to a temporary name and rename into place, so the only
// event on the directory is IN_MOVED_TO.
func TestAutoReloadSeesARenameIntoPlace(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/present.service": "[Service]\nExecStart=/bin/present\n",
	})
	m.startAutoReload()

	dir := filepath.Join(m.root(), "lib/systemd/system")
	tmp := filepath.Join(dir, "renamed.service.dpkg-new")
	if err := os.WriteFile(tmp, []byte("[Service]\nExecStart=/bin/renamed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "renamed.service")); err != nil {
		t.Fatal(err)
	}

	if !waitForUnit(t, m, "renamed.service", 10*time.Second) {
		t.Fatal("a unit file renamed into place was not picked up")
	}
}

// A first install creates the target's .wants directory as well as the unit
// file; the enable symlink that lands in it afterwards has to be seen too.
func TestAutoReloadWatchesADirectoryCreatedAfterStart(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/present.service": "[Service]\nExecStart=/bin/present\n",
		// The administrator directory exists but is empty, as it is on a
		// stock image and as prepareFilesystem guarantees at boot.
		"etc/systemd/system/.keep": "",
	})
	m.startAutoReload()

	// The package's unit file: this triggers the first reload, after which the
	// freshly created .wants directory is added to the watch set.
	writeUnit(t, m, "lib/systemd/system/late.service",
		"[Unit]\nDescription=v1\n\n[Service]\nExecStart=/bin/late\n")
	if !waitForUnit(t, m, "late.service", 10*time.Second) {
		t.Fatal("the unit file was not picked up")
	}

	// A drop-in written into a directory that did not exist when the watcher
	// started must still be noticed.
	writeUnit(t, m, "etc/systemd/system/late.service.d/override.conf",
		"[Unit]\nDescription=v2\n")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if u := m.Get("late.service"); u != nil && u.Unit.Description == "v2" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the drop-in was not applied; Description = %q", m.Get("late.service").Unit.Description)
}

// --no-auto-reload restores the systemd behaviour of only reloading when asked.
func TestNoAutoReloadLeavesTheRegistryAlone(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/present.service": "[Service]\nExecStart=/bin/present\n",
	})
	m.opts.NoAutoReload = true
	m.startAutoReload()

	writeUnit(t, m, "lib/systemd/system/ignored.service", "[Service]\nExecStart=/bin/ignored\n")
	if waitForUnit(t, m, "ignored.service", 4*autoReloadQuiet+autoReloadMax/5) {
		t.Error("--no-auto-reload should not reload the registry on its own")
	}
	m.DaemonReload()
	if m.Get("ignored.service") == nil {
		t.Error("an explicit daemon-reload must still pick the unit up")
	}
}

func writeUnit(t *testing.T, m *Manager, rel, content string) {
	t.Helper()
	full := filepath.Join(m.root(), rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
