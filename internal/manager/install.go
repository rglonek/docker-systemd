package manager

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"docker-systemd/internal/paths"
	"docker-systemd/internal/proto"
	"docker-systemd/internal/unitfile"
)

// aliasNames are the argv[0] spellings the manager installs symlinks for
// (06 §1).
var aliasNames = []string{
	"init", "systemd", "systemctl", "journalctl", "service",
	"poweroff", "halt", "reboot", "shutdown", "telinit", "runlevel",
	"systemd-notify", "systemd-detect-virt",
}

// installDirs are searched in order for the first writable one.
var installDirs = []string{"/usr/local/sbin", "/usr/sbin", "/sbin", "/usr/bin", "/bin"}

// selfInstall symlinks the alias names at self.
//
// Three changes from v0.5.x, which renamed the distro's binaries
// unconditionally and irreversibly with log.Fatalf on failure (defect F8):
// it is idempotent, it never overwrites an existing .dist, and a failure to
// install one name is a warning rather than fatal.
func (m *Manager) selfInstall() {
	self, err := os.Executable()
	if err != nil {
		m.log.Warnf("cannot determine our own path; skipping self-install: %v", err)
		return
	}
	self, _ = filepath.EvalSymlinks(self)

	dir := ""
	for _, d := range installDirs {
		if isWritableDir(d) {
			dir = d
			break
		}
	}
	if dir == "" {
		m.log.Warnf("no writable directory among %v; skipping self-install", installDirs)
		return
	}

	for _, name := range aliasNames {
		target := filepath.Join(dir, name)
		if m.installOne(self, target, name) {
			m.installed = append(m.installed, target)
		}
	}
	if len(m.installed) > 0 {
		content := strings.Join(m.installed, "\n") + "\n"
		_ = os.WriteFile(paths.InstalledFile, []byte(content), 0o600)
		m.log.Infof("installed %d command symlinks into %s", len(m.installed), dir)
	}
}

// installOne links target at self, preserving any existing real binary as
// <name>.dist exactly once.
func (m *Manager) installOne(self, target, name string) bool {
	fi, err := os.Lstat(target)
	switch {
	case err != nil && os.IsNotExist(err):
		// Nothing there; just link.
	case err != nil:
		m.log.Warnf("cannot stat %s: %v", target, err)
		return false
	case fi.Mode()&os.ModeSymlink != 0:
		dest, _ := os.Readlink(target)
		if dest == self {
			m.log.Debugf("%s already points at us", target)
			return true
		}
		if err := os.Remove(target); err != nil {
			m.log.Warnf("cannot replace %s: %v", target, err)
			return false
		}
	default:
		dist := target + ".dist"
		if _, err := os.Lstat(dist); err == nil {
			// A .dist already exists from an earlier run; never overwrite it,
			// or the distro's original binary is lost forever.
			if err := os.Remove(target); err != nil {
				m.log.Warnf("cannot replace %s: %v", target, err)
				return false
			}
		} else if err := os.Rename(target, dist); err != nil {
			m.log.Warnf("cannot preserve %s as %s: %v", target, dist, err)
			return false
		} else {
			m.log.Infof("preserved the distro's %s as %s", target, dist)
		}
	}
	if err := os.Symlink(self, target); err != nil {
		m.log.Warnf("cannot install %s: %v", target, err)
		return false
	}
	return true
}

func isWritableDir(d string) bool {
	fi, err := os.Stat(d)
	if err != nil || !fi.IsDir() {
		return false
	}
	probe := filepath.Join(d, ".docker-systemd-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	_ = os.Remove(probe)
	return true
}

// verbInstall implements the enablement verbs.
func (m *Manager) verbInstall(conn net.Conn, req proto.Request, progress func(string)) proto.Result {
	var changes []string
	var err error
	switch req.Verb {
	case "enable", "reenable", "preset":
		changes, err = m.enable(req.Units, req.Verb == "reenable")
	case "disable":
		changes, err = m.disable(req.Units)
	case "preset-all":
		changes, err = m.presetAll()
	case "mask":
		changes, err = m.mask(req.Units)
	case "unmask":
		changes, err = m.unmask(req.Units)
	case "link":
		changes, err = m.link(req.Units)
	case "revert":
		changes, err = m.revert(req.Units)
	case "add-wants":
		changes, err = m.addDep(req.Units, req.Args, "wants")
	case "add-requires":
		changes, err = m.addDep(req.Units, req.Args, "requires")
	case "create-instance":
		changes, err = m.enable(req.Units, false)
	case "delete-instance":
		// Compatibility alias: removes the enable symlink only. v0.5.x called
		// DeleteService(), which RemoveAll'd every path of the unit — running
		// it on a non-template unit deleted the distro's unit file (B13).
		changes, err = m.disable(req.Units)
	}
	if err != nil {
		return m.resultOf(err)
	}
	for _, c := range changes {
		progress(c)
	}
	m.DaemonReload()
	_ = proto.WriteJSON(conn, proto.TypeData, changes)

	if req.Options.Now {
		switch req.Verb {
		case "enable", "reenable", "preset", "unmask":
			return m.resultOf(m.StartUnits(req.Units, progress))
		case "disable", "mask":
			return m.resultOf(m.StopUnits(req.Units, progress))
		}
	}
	return ok()
}

// enable writes real symlinks into the target named by [Install], honouring
// WantedBy=, RequiredBy=, Alias= and Also=.
//
// v0.5.x wrote a regular file containing "OK" and always into
// multi-user.target.wants regardless of WantedBy= (defect B18).
func (m *Manager) enable(names []string, force bool) ([]string, error) {
	var changes []string
	for _, name := range names {
		name = unitfile.CanonicalName(name)
		u := m.Get(name)
		if u == nil {
			return changes, fmt.Errorf("Unit %s not found.", name)
		}
		if u.Install.Empty() {
			return changes, fmt.Errorf(
				"The unit files have no installation config (WantedBy=, RequiredBy=, Also=, Alias=\n" +
					"settings in the [Install] section, and DefaultInstance= for template units).")
		}
		fragment := u.FragmentPath
		if fragment == "" {
			return changes, fmt.Errorf("Unit %s has no fragment to link.", name)
		}

		for _, target := range u.Install.WantedBy {
			c, err := m.linkInto(target, "wants", name, fragment, force)
			if err != nil {
				return changes, err
			}
			changes = append(changes, c...)
		}
		for _, target := range u.Install.RequiredBy {
			c, err := m.linkInto(target, "requires", name, fragment, force)
			if err != nil {
				return changes, err
			}
			changes = append(changes, c...)
		}
		for _, alias := range u.Install.Alias {
			dst := filepath.Join(paths.WritableUnitDir(m.opts.Root), alias)
			if err := replaceSymlink(dst, fragment, force); err != nil {
				return changes, err
			}
			changes = append(changes, fmt.Sprintf("Created symlink %s → %s.", dst, fragment))
		}
		for _, also := range u.Install.Also {
			c, err := m.enable([]string{also}, force)
			if err != nil {
				return changes, err
			}
			changes = append(changes, c...)
		}
	}
	return changes, nil
}

// linkInto creates <target>.wants/<name> → <fragment>.
func (m *Manager) linkInto(target, kind, name, fragment string, force bool) ([]string, error) {
	target = unitfile.CanonicalName(target)
	dir := filepath.Join(paths.WritableUnitDir(m.opts.Root), target+"."+kind)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	dst := filepath.Join(dir, name)
	if err := replaceSymlink(dst, fragment, force); err != nil {
		return nil, err
	}
	return []string{fmt.Sprintf("Created symlink %s → %s.", dst, fragment)}, nil
}

func replaceSymlink(dst, src string, force bool) error {
	if fi, err := os.Lstat(dst); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			if cur, _ := os.Readlink(dst); cur == src {
				return nil
			}
		} else if !force {
			return fmt.Errorf("Failed to create symlink %s: file exists", dst)
		}
		if err := os.Remove(dst); err != nil {
			return err
		}
	}
	return os.Symlink(src, dst)
}

// disable removes every enable symlink pointing at a unit.
func (m *Manager) disable(names []string) ([]string, error) {
	var changes []string
	root := paths.WritableUnitDir(m.opts.Root)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil
	}
	want := map[string]bool{}
	for _, n := range names {
		want[unitfile.CanonicalName(n)] = true
	}
	for _, e := range entries {
		if !e.IsDir() ||
			(!strings.HasSuffix(e.Name(), ".wants") && !strings.HasSuffix(e.Name(), ".requires")) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		links, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, l := range links {
			if !want[l.Name()] {
				continue
			}
			p := filepath.Join(dir, l.Name())
			// Only the link is removed, never the unit file it points at.
			if err := os.Remove(p); err == nil {
				changes = append(changes, fmt.Sprintf("Removed %s.", p))
			}
		}
	}
	// Aliases living directly in the administrator directory.
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(root, e.Name())
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		dest, _ := os.Readlink(p)
		if want[filepath.Base(dest)] && want[e.Name()] {
			if err := os.Remove(p); err == nil {
				changes = append(changes, fmt.Sprintf("Removed %s.", p))
			}
		}
	}
	return changes, nil
}

// presetAll enables every unit that declares an [Install] section. Without a
// preset policy file to consult, "enable everything installable" is the
// honest interpretation, and it is what RPM's %systemd_post expects to be
// able to call.
func (m *Manager) presetAll() ([]string, error) {
	reg := m.Registry()
	if reg == nil {
		return nil, nil
	}
	var changes []string
	for _, n := range reg.Names() {
		u := reg.Get(n)
		if u == nil || u.Synthetic || u.Install.Empty() {
			continue
		}
		c, err := m.enable([]string{n}, false)
		if err != nil {
			m.log.Warnf("preset-all: %s: %v", n, err)
			continue
		}
		changes = append(changes, c...)
	}
	return changes, nil
}

// mask symlinks a unit name to /dev/null in the administrator directory.
//
// v0.5.x symlinked to the literal string "target" instead of the target
// variable, so a file called `target` appeared in init's working directory and
// the mask was lost on restart (defect B4).
func (m *Manager) mask(names []string) ([]string, error) {
	var changes []string
	dir := paths.WritableUnitDir(m.opts.Root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for _, n := range names {
		n = unitfile.CanonicalName(n)
		target := filepath.Join(dir, n)
		if fi, err := os.Lstat(target); err == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				if cur, _ := os.Readlink(target); cur == os.DevNull {
					continue
				}
			}
			return changes, fmt.Errorf("Failed to mask unit: File %s already exists", target)
		}
		if err := os.Symlink(os.DevNull, target); err != nil {
			return changes, err
		}
		changes = append(changes, fmt.Sprintf("Created symlink %s → %s.", target, os.DevNull))
	}
	return changes, nil
}

func (m *Manager) unmask(names []string) ([]string, error) {
	var changes []string
	dir := paths.WritableUnitDir(m.opts.Root)
	for _, n := range names {
		n = unitfile.CanonicalName(n)
		target := filepath.Join(dir, n)
		fi, err := os.Lstat(target)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if cur, _ := os.Readlink(target); cur != os.DevNull {
			continue
		}
		if err := os.Remove(target); err != nil {
			return changes, err
		}
		changes = append(changes, fmt.Sprintf("Removed %s.", target))
	}
	return changes, nil
}

// link makes a unit file outside the search path available by symlinking it
// into the administrator directory.
func (m *Manager) link(pathsIn []string) ([]string, error) {
	var changes []string
	dir := paths.WritableUnitDir(m.opts.Root)
	for _, p := range pathsIn {
		if !filepath.IsAbs(p) {
			return changes, fmt.Errorf("Link path %s is not absolute.", p)
		}
		dst := filepath.Join(dir, filepath.Base(p))
		if err := replaceSymlink(dst, p, true); err != nil {
			return changes, err
		}
		changes = append(changes, fmt.Sprintf("Created symlink %s → %s.", dst, p))
	}
	return changes, nil
}

// revert removes administrator-level fragments and drop-ins so the vendor unit
// takes effect again.
func (m *Manager) revert(names []string) ([]string, error) {
	var changes []string
	dir := paths.WritableUnitDir(m.opts.Root)
	for _, n := range names {
		n = unitfile.CanonicalName(n)
		frag := filepath.Join(dir, n)
		if _, err := os.Lstat(frag); err == nil {
			if err := os.Remove(frag); err == nil {
				changes = append(changes, fmt.Sprintf("Removed %s.", frag))
			}
		}
		dropins := filepath.Join(dir, n+".d")
		if _, err := os.Stat(dropins); err == nil {
			if err := os.RemoveAll(dropins); err == nil {
				changes = append(changes, fmt.Sprintf("Removed %s.", dropins))
			}
		}
	}
	return changes, nil
}

// addDep implements add-wants/add-requires.
func (m *Manager) addDep(targets, units []string, kind string) ([]string, error) {
	if len(targets) == 0 || len(units) == 0 {
		return nil, fmt.Errorf("add-%s takes a target and at least one unit", kind)
	}
	var changes []string
	for _, unit := range units {
		unit = unitfile.CanonicalName(unit)
		u := m.Get(unit)
		if u == nil || u.FragmentPath == "" {
			return changes, fmt.Errorf("Unit %s not found.", unit)
		}
		c, err := m.linkInto(targets[0], kind, unit, u.FragmentPath, true)
		if err != nil {
			return changes, err
		}
		changes = append(changes, c...)
	}
	return changes, nil
}
