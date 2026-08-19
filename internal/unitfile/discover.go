package unitfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"docker-systemd/internal/paths"
)

// syntheticTargets are the well-known synchronisation points a container image
// may not ship. Creating them on demand is what makes `After=network.target`
// resolve and order correctly instead of dangling (defect C18).
var syntheticTargets = []string{
	"basic.target", "sysinit.target", "local-fs.target", "local-fs-pre.target",
	"network.target", "network-pre.target", "network-online.target",
	"remote-fs.target", "remote-fs-pre.target", "time-sync.target",
	"sockets.target", "timers.target", "paths.target", "slices.target",
	"multi-user.target", "graphical.target", "default.target",
	"shutdown.target", "final.target", "umount.target", "getty.target",
	"getty-pre.target", "nss-lookup.target", "nss-user-lookup.target",
	"rpcbind.target", "syslog.target", "dbus.socket", "systemd-journald.socket",
	"rescue.target", "emergency.target",
}

// SyntheticTargets returns the names of targets synthesised on demand.
func SyntheticTargets() []string {
	out := make([]string, len(syntheticTargets))
	copy(out, syntheticTargets)
	return out
}

// fragment describes one candidate unit file found on disk.
type fragment struct {
	name    string
	path    string
	masked  bool
	aliasOf string // set when the file is a symlink to another unit file
}

// Loader discovers and parses unit files.
type Loader struct {
	// Root prefixes every path; "" means the real filesystem. Tests set it.
	Root string
	// SpecifierBase supplies the host-derived %-specifier values.
	SpecifierBase SpecifierContext
	// SearchPath overrides paths.UnitSearchPath; nil means use the default.
	SearchPath []string
}

// Registry is an immutable snapshot of every known unit. It is replaced
// wholesale on daemon-reload (copy-on-write), so a reader holding a snapshot
// can never observe a half-built graph.
type Registry struct {
	units    map[string]*Unit  // canonical name -> unit
	aliases  map[string]string // alias name -> canonical name
	enabled  map[string]string // canonical name -> unit file state
	warnings []Warning
	// wantsDirs records the .wants/.requires edges harvested from the
	// filesystem, keyed by the target unit.
	wantsDirs    map[string][]string
	requiresDirs map[string][]string
}

// Names returns every canonical unit name, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.units))
	for n := range r.units {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Warnings returns registry-level load diagnostics.
func (r *Registry) Warnings() []Warning { return r.warnings }

// Get resolves a name — canonical, alias, or template instance — to a unit.
// It returns nil when nothing matches; callers must handle absence rather than
// receiving a half-built unit, which is what made the A4/A6 nil-dereference
// family possible.
func (r *Registry) Get(name string) *Unit {
	name = CanonicalName(name)
	if u, ok := r.units[name]; ok {
		return u
	}
	if canon, ok := r.aliases[name]; ok {
		return r.units[canon]
	}
	return nil
}

// Has reports whether a name resolves.
func (r *Registry) Has(name string) bool { return r.Get(name) != nil }

// UnitFileState returns the enable state of a unit (enabled, disabled, static,
// masked, generated, alias or bad).
func (r *Registry) UnitFileState(name string) string {
	u := r.Get(name)
	if u == nil {
		return "not-found"
	}
	if s, ok := r.enabled[u.Name]; ok {
		return s
	}
	return "static"
}

// InjectedWants returns the .wants-directory edges for a unit.
func (r *Registry) InjectedWants(name string) []string { return r.wantsDirs[CanonicalName(name)] }

// InjectedRequires returns the .requires-directory edges for a unit.
func (r *Registry) InjectedRequires(name string) []string {
	return r.requiresDirs[CanonicalName(name)]
}

// Load scans the search path and builds a registry snapshot.
func (l *Loader) Load() (*Registry, error) {
	search := l.SearchPath
	if search == nil {
		search = paths.UnitSearchPath(l.Root)
	}

	reg := &Registry{
		units:        map[string]*Unit{},
		aliases:      map[string]string{},
		enabled:      map[string]string{},
		wantsDirs:    map[string][]string{},
		requiresDirs: map[string][]string{},
	}

	// Pass 1: collect the highest-precedence fragment for each name, plus the
	// .wants/.requires edges from every directory in the search path.
	frags := map[string]fragment{}
	for _, dir := range search {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				switch {
				case strings.HasSuffix(name, ".wants"):
					target := CanonicalName(strings.TrimSuffix(name, ".wants"))
					reg.wantsDirs[target] = appendUnique(reg.wantsDirs[target], readLinkDir(filepath.Join(dir, name))...)
				case strings.HasSuffix(name, ".requires"):
					target := CanonicalName(strings.TrimSuffix(name, ".requires"))
					reg.requiresDirs[target] = appendUnique(reg.requiresDirs[target], readLinkDir(filepath.Join(dir, name))...)
				}
				continue
			}
			if !isUnitFileName(name) {
				continue
			}
			if _, taken := frags[name]; taken {
				// A higher-precedence directory already provided this name;
				// the fragment completely replaces the lower one and the two
				// are never merged.
				continue
			}
			frags[name] = l.classify(dir, name)
		}
	}

	// Pass 2: build the units.
	names := make([]string, 0, len(frags))
	for n := range frags {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		f := frags[name]
		if f.aliasOf != "" {
			// A symlink pointing at another unit file is an alias, not a
			// second half-loaded unit (defect A5).
			if _, ok := frags[f.aliasOf]; ok {
				reg.aliases[name] = f.aliasOf
				continue
			}
			// The target is outside the search path; fall through and load
			// the file itself.
		}
		u, err := l.loadUnit(name, f, search)
		if err != nil {
			u = NewUnit(name)
			u.LoadState = LoadBadSetting
			u.LoadError = err.Error()
			u.FragmentPath = f.path
		}
		reg.units[name] = u
	}

	// Record alias names on their canonical unit so `systemctl status` can
	// report both.
	for alias, canon := range reg.aliases {
		if u := reg.units[canon]; u != nil {
			u.Names = appendUnique(u.Names, alias)
		}
	}
	for _, u := range reg.units {
		u.Names = appendUnique([]string{u.Name}, u.Names...)
	}

	// Pass 3: derive enable state from the filesystem on every load, never
	// cached across a reload (defect B4).
	l.deriveEnableState(reg, search)

	// Pass 4: synthesise the well-known targets that are absent.
	for _, t := range syntheticTargets {
		if _, ok := reg.units[t]; ok {
			continue
		}
		if _, ok := reg.aliases[t]; ok {
			continue
		}
		u := NewUnit(t)
		u.LoadState = LoadLoaded
		u.Synthetic = true
		u.Names = []string{t}
		u.Unit.Description = strings.TrimSuffix(t, ".target") + " (synthesised)"
		reg.units[t] = u
	}

	return reg, nil
}

// classify inspects one candidate file and decides whether it is a real
// fragment, a mask, or an alias.
func (l *Loader) classify(dir, name string) fragment {
	p := filepath.Join(dir, name)
	f := fragment{name: name, path: p}
	fi, err := os.Lstat(p)
	if err != nil {
		return f
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(p)
		if err != nil {
			return f
		}
		if target == "/dev/null" {
			f.masked = true
			return f
		}
		base := filepath.Base(target)
		if base != name && isUnitFileName(base) {
			f.aliasOf = base
		}
		return f
	}
	if fi.Size() == 0 {
		// systemd treats an empty regular file as masked.
		f.masked = true
	}
	return f
}

// loadUnit parses one fragment plus its drop-ins.
func (l *Loader) loadUnit(name string, f fragment, search []string) (*Unit, error) {
	ctx := l.SpecifierBase
	ctx.UnitName = name
	p := NewParser(name, ctx)
	u := p.unit
	u.FragmentPath = f.path

	if f.masked {
		u.LoadState = LoadMasked
		return p.Finish(), nil
	}

	data, err := os.ReadFile(f.path)
	if err != nil {
		return nil, err
	}
	frag, err := LexBytes(f.path, data)
	if err != nil {
		return nil, err
	}
	p.Apply(frag)
	u.LoadState = LoadLoaded

	for _, d := range l.dropInPaths(name, search) {
		data, err := os.ReadFile(d)
		if err != nil {
			continue
		}
		df, err := LexBytes(d, data)
		if err != nil {
			continue
		}
		p.Apply(df)
		u.DropInPaths = append(u.DropInPaths, d)
	}
	return p.Finish(), nil
}

// dropInPaths returns the .conf files to overlay on a unit, in application
// order: lowest priority first (05 §1).
func (l *Loader) dropInPaths(name string, search []string) []string {
	var dirs []string
	// Type-wide drop-ins are the lowest priority of all.
	kind := strings.TrimPrefix(UnitSuffix(name), ".")
	dirs = append(dirs,
		filepath.Join(l.Root, "/usr/lib/systemd/system", kind+".d"),
		filepath.Join(l.Root, "/etc/systemd/system", kind+".d"),
	)
	// For an instance, the template's drop-in directory applies before the
	// instance's own.
	prefix, instance := SplitTemplate(name)
	if instance != "" {
		tmpl := prefix + "@" + UnitSuffix(name)
		for _, base := range reverse(search) {
			dirs = append(dirs, filepath.Join(base, tmpl+".d"))
		}
	}
	for _, base := range reverse(search) {
		dirs = append(dirs, filepath.Join(base, name+".d"))
	}

	var out []string
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		var confs []string
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
				continue
			}
			confs = append(confs, filepath.Join(d, e.Name()))
		}
		sort.Strings(confs)
		out = append(out, confs...)
	}
	return out
}

// deriveEnableState walks the .wants/.requires directories under the
// administrator directory to decide which units count as enabled.
func (l *Loader) deriveEnableState(reg *Registry, search []string) {
	for name, u := range reg.units {
		switch {
		case u.LoadState == LoadMasked:
			reg.enabled[name] = "masked"
		case u.LoadState == LoadBadSetting:
			reg.enabled[name] = "bad"
		case u.Synthetic:
			reg.enabled[name] = "static"
		case u.Install.Empty():
			reg.enabled[name] = "static"
		default:
			reg.enabled[name] = "disabled"
		}
	}
	for _, links := range reg.wantsDirs {
		for _, n := range links {
			if u := reg.Get(n); u != nil && reg.enabled[u.Name] == "disabled" {
				reg.enabled[u.Name] = "enabled"
			}
		}
	}
	for _, links := range reg.requiresDirs {
		for _, n := range links {
			if u := reg.Get(n); u != nil && reg.enabled[u.Name] == "disabled" {
				reg.enabled[u.Name] = "enabled"
			}
		}
	}
	for alias := range reg.aliases {
		reg.enabled[alias] = "alias"
	}
}

// ResolveTemplate finds the fragment backing a possibly-templated name.
// Instances are resolved, never materialised: v0.5.x's delete-instance called
// RemoveAll on every path of the unit, which deleted the distro's unit file
// (defect B13).
func (l *Loader) ResolveTemplate(reg *Registry, name string) (*Unit, error) {
	name = CanonicalName(name)
	if u := reg.Get(name); u != nil {
		return u, nil
	}
	prefix, instance := SplitTemplate(name)
	if instance == "" {
		return nil, fmt.Errorf("unit %s not found", name)
	}
	tmplName := prefix + "@" + UnitSuffix(name)
	tmpl := reg.Get(tmplName)
	if tmpl == nil {
		return nil, fmt.Errorf("unit %s not found (no template %s)", name, tmplName)
	}
	if tmpl.FragmentPath == "" {
		return nil, fmt.Errorf("template %s has no fragment", tmplName)
	}
	f := l.classify(filepath.Dir(tmpl.FragmentPath), filepath.Base(tmpl.FragmentPath))
	search := l.SearchPath
	if search == nil {
		search = paths.UnitSearchPath(l.Root)
	}
	u, err := l.loadUnit(name, f, search)
	if err != nil {
		return nil, err
	}
	// The fragment path stays the template's, which is what `systemctl cat`
	// and `ls -l` in the container show.
	u.FragmentPath = tmpl.FragmentPath
	return u, nil
}

// AddInstance registers a resolved instance into a snapshot. It is used by the
// manager when a job names an instance that is not yet in the registry.
func (r *Registry) AddInstance(u *Unit) {
	r.units[u.Name] = u
	if _, ok := r.enabled[u.Name]; !ok {
		r.enabled[u.Name] = "static"
	}
}

// SetUnit replaces a unit in the snapshot. Only the manager's registry-building
// path calls this, before the snapshot is published.
func (r *Registry) SetUnit(u *Unit) { r.units[u.Name] = u }

// SetEnabled records a unit file state.
func (r *Registry) SetEnabled(name, state string) { r.enabled[CanonicalName(name)] = state }

// AddWarning records a registry-level diagnostic.
func (r *Registry) AddWarning(w Warning) { r.warnings = append(r.warnings, w) }

// isUnitFileName reports whether a filename looks like a unit we handle.
// Only .service and .target are first-class in v1; other suffixes are listed so
// that a `.socket` in a `.wants` directory does not become a bogus `.service`.
func isUnitFileName(name string) bool {
	switch filepath.Ext(name) {
	case ".service", ".target":
		return true
	}
	return false
}

// readLinkDir lists the unit names symlinked (or copied) into a .wants or
// .requires directory.
func readLinkDir(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !isUnitFileName(e.Name()) {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func appendUnique(dst []string, add ...string) []string {
	for _, a := range add {
		found := false
		for _, d := range dst {
			if d == a {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, a)
		}
	}
	return dst
}

func reverse(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
}
