// Package manager implements PID 1: the unit registry, the dependency graph,
// the job scheduler, the control server, the log broker and the shutdown
// orchestrator.
//
// PID 1 is a thin dispatcher (ADR-3): it never execs a unit command and never
// blocks on one, so a crash in unit handling can no longer kill the container.
package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"docker-systemd/internal/backend"
	"docker-systemd/internal/graph"
	"docker-systemd/internal/journal"
	"docker-systemd/internal/logging"
	"docker-systemd/internal/paths"
	"docker-systemd/internal/proto"
	"docker-systemd/internal/unitfile"
)

// Options are the manager's command-line switches (04 §11).
type Options struct {
	LogToStderr      bool
	LogToStderrUnits []string
	NoLogfile        bool
	LogLevel         logging.Level
	DefaultTarget    string
	NoInstall        bool
	NoAutoReload     bool
	ShutdownTimeout  time.Duration
	Backend          backend.Name
	HeartbeatMS      int
	CompatTmpSocket  bool
	CompatShellExec  bool
	ControlAllowUID  []int
	ControlAllowGID  []int
	PassEnvironment  string
	Version          string
	Rotate           journal.RotateConfig
	Root             string
}

// DefaultOptions returns the shipped defaults.
func DefaultOptions() Options {
	return Options{
		LogLevel:        logging.LevelInfo,
		DefaultTarget:   "multi-user.target",
		ShutdownTimeout: 90 * time.Second,
		Backend:         backend.Auto,
		HeartbeatMS:     1000,
		PassEnvironment: "all",
		Rotate:          journal.DefaultRotateConfig(),
	}
}

// Manager is the PID 1 process state.
type Manager struct {
	opts   Options
	log    *logging.Logger
	broker *journal.Broker
	be     backend.Backend
	probes map[string]string

	// regMu guards the registry pointer. The registry itself is immutable, so
	// a reader takes a reference and can hold it across any operation: a
	// reload can never expose a half-built graph.
	regMu    sync.RWMutex
	registry *unitfile.Registry
	loader   *unitfile.Loader

	// runMu guards the runners map.
	runMu   sync.Mutex
	runners map[string]*runner
	// targets records synthetic/target units that are "active" without a
	// process.
	targets map[string]bool
	// lastState keeps the final report of a unit whose supervisor has exited,
	// so `systemctl status` can still explain why it failed.
	lastState map[string]proto.StateReport

	envMu      sync.Mutex
	managerEnv map[string]string

	bootID    string
	bootTime  time.Time
	machineID string

	shutdownOnce sync.Once
	shutdownCh   chan shutdownRequest
	shuttingDown atomic.Bool
	signalCount  atomic.Int32

	installed []string
	warnings  []string
}

// New builds a manager.
func New(opts Options) *Manager {
	log := logging.New(os.Stderr, opts.LogLevel, "systemd")
	return &Manager{
		opts:       opts,
		log:        log,
		runners:    map[string]*runner{},
		targets:    map[string]bool{},
		lastState:  map[string]proto.StateReport{},
		managerEnv: map[string]string{},
		shutdownCh: make(chan shutdownRequest, 4),
	}
}

// Log exposes the manager's logger.
func (m *Manager) Log() *logging.Logger { return m.log }

// Registry returns the current immutable snapshot.
func (m *Manager) Registry() *unitfile.Registry {
	m.regMu.RLock()
	defer m.regMu.RUnlock()
	return m.registry
}

// --- graph.Resolver -------------------------------------------------------

// Get resolves a unit name against the current snapshot, materialising a
// template instance on demand.
func (m *Manager) Get(name string) *unitfile.Unit {
	// The loader is read with the snapshot, under the same lock: it is the one
	// that produced this registry, and a concurrent reload replaces both.
	m.regMu.RLock()
	reg, loader := m.registry, m.loader
	m.regMu.RUnlock()
	if reg == nil || loader == nil {
		return nil
	}
	if u := reg.Get(name); u != nil {
		return u
	}
	// A template instance that is not yet in the registry is resolved from its
	// template rather than materialised on disk (05 §6).
	u, err := loader.ResolveTemplate(reg, name)
	if err != nil {
		return nil
	}
	reg.AddInstance(u)
	return u
}

// InjectedWants returns the .wants-directory edges for a unit.
func (m *Manager) InjectedWants(name string) []string {
	reg := m.Registry()
	if reg == nil {
		return nil
	}
	out := append([]string{}, reg.InjectedWants(name)...)
	// Every enabled unit whose [Install] WantedBy= names this target is also
	// wanted by it (05 §5).
	for _, n := range reg.Names() {
		u := reg.Get(n)
		if u == nil || reg.UnitFileState(n) != "enabled" {
			continue
		}
		for _, w := range u.Install.WantedBy {
			if unitfile.CanonicalName(w) == unitfile.CanonicalName(name) {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return dedupe(out)
}

// InjectedRequires returns the .requires-directory edges for a unit.
func (m *Manager) InjectedRequires(name string) []string {
	reg := m.Registry()
	if reg == nil {
		return nil
	}
	out := append([]string{}, reg.InjectedRequires(name)...)
	for _, n := range reg.Names() {
		u := reg.Get(n)
		if u == nil || reg.UnitFileState(n) != "enabled" {
			continue
		}
		for _, w := range u.Install.RequiredBy {
			if unitfile.CanonicalName(w) == unitfile.CanonicalName(name) {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return dedupe(out)
}

// IsActive reports whether a unit is currently active.
func (m *Manager) IsActive(name string) bool {
	st := m.UnitState(name)
	return st.ActiveState == proto.StateActive || st.ActiveState == proto.StateReloading
}

func dedupe(in []string) []string {
	out := in[:0]
	var last string
	for i, s := range in {
		if i > 0 && s == last {
			continue
		}
		out = append(out, s)
		last = s
	}
	return out
}

// --- boot -----------------------------------------------------------------

// Boot performs the sequence of 04 §8 and then serves until shutdown. It
// returns the process exit code.
func (m *Manager) Boot() int {
	m.bootTime = time.Now()

	if os.Getpid() != 1 {
		m.log.Warnf("not running as PID 1: reaping will be incomplete and " +
			"escaped processes may not be attributed to their unit")
	}
	// Belt and braces if we are not pid 1: orphans still land on us rather
	// than on the container's real init.
	m.setSubreaper()

	m.startReaper()

	be, probes := backend.Select(m.opts.Backend)
	m.be, m.probes = be, probes
	m.log.Infof("process tracking backend: %s", be.Name())
	for k, v := range probes {
		m.log.Debugf("probe %s: %s", k, v)
	}
	if be.Name() == backend.Degraded {
		m.log.Warnf("DEGRADED process tracking: only direct children of a unit " +
			"will be tracked. Escaped daemons will not be stopped reliably.")
		m.warnings = append(m.warnings, "degraded process tracking backend")
	}

	if err := m.prepareFilesystem(); err != nil {
		m.log.Errorf("cannot prepare the runtime directories: %v", err)
		return 1
	}

	m.broker = journal.NewBroker(paths.LogDir, m.opts.Rotate)
	if m.opts.NoLogfile {
		m.broker.Disable()
	}
	if m.opts.LogToStderr {
		m.broker.SetMirror(os.Stderr, m.opts.LogToStderrUnits)
	}

	if !m.opts.NoInstall {
		m.selfInstall()
	}

	// The control socket is bound before any unit starts, because package
	// installation inside a `docker build` runs maintainer scripts that call
	// systemctl while units are still coming up.
	ln, err := m.listenControl()
	if err != nil {
		m.log.Errorf("cannot bind the control socket: %v", err)
		return 1
	}
	go m.serveControl(ln)

	m.installSignalHandlers()

	m.loadRegistry()

	// Started before the boot transaction, not after it: a `docker exec apt
	// install` can land while slow units are still coming up, and the boot
	// transaction is planned from an immutable registry snapshot, so a reload
	// underneath it is no different from an operator running daemon-reload.
	m.startAutoReload()

	target := m.opts.DefaultTarget
	m.log.Infof("booting %s", target)
	started, failed := m.bootTransaction(target)
	m.log.Infof("Startup finished in %s (%d units started, %d failed)",
		time.Since(m.bootTime).Round(time.Millisecond), started, failed)

	req := <-m.shutdownCh
	return m.runShutdown(req)
}

// prepareFilesystem creates the runtime layout with the right modes and clears
// v0.5.x leftovers.
func (m *Manager) prepareFilesystem() error {
	// The directory is created and chmod'ed before any socket is bound; the
	// umask is deliberately not relied on (09 §4).
	dirs := []struct {
		path string
		mode os.FileMode
	}{
		{paths.RuntimeDir, paths.ModeRuntimeDir},
		{paths.NotifyDir, paths.ModeNotifyDir},
		{paths.UnitStateDir, paths.ModeStateDir},
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return err
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			return err
		}
	}
	// Maintainer scripts gate on this directory existing; without it, packages
	// skip unit registration entirely (defect B19).
	if err := os.MkdirAll(paths.SystemdMarkerDir, paths.ModeMarkerDir); err != nil {
		m.log.Warnf("cannot create %s: %v; distro maintainer scripts may skip unit registration",
			paths.SystemdMarkerDir, err)
	}
	// The administrator unit directory is where enable symlinks, drop-ins and
	// hand-copied units land. systemd ships it, `systemctl enable` creates it
	// on demand, and the auto-reload watcher cannot watch a directory that is
	// not there — so create it up front rather than at first write.
	if err := os.MkdirAll(paths.WritableUnitDir(m.opts.Root), 0o755); err != nil {
		m.log.Warnf("cannot create %s: %v", paths.WritableUnitDir(m.opts.Root), err)
	}
	if err := os.MkdirAll(paths.LogDir, paths.ModeLogDir); err != nil {
		return err
	}
	_ = os.Chmod(paths.LogDir, paths.ModeLogDir)

	m.bootID = unitfile.NewInvocationID()
	m.machineID = unitfile.MachineID()
	boot := fmt.Sprintf("%s %s\n", m.bootTime.UTC().Format(journal.TimeFormat), m.bootID)
	if err := os.WriteFile(paths.BootIDFile, []byte(boot), 0o644); err != nil {
		return err
	}
	// v0.5.x wrote boot state into /etc, which pollutes committed images.
	_ = os.Remove(paths.LegacyBootFile)

	m.cleanStalePreload()
	return nil
}

// cleanStalePreload removes our own entry from /etc/ld.so.preload. An image
// committed from a v0.5.x container carries a dangling entry naming a .so that
// no longer exists, which makes every dynamically linked binary print a loader
// error (defect F6).
func (m *Manager) cleanStalePreload() {
	data, err := os.ReadFile(paths.LdSoPreload)
	if err != nil {
		return
	}
	var kept []string
	removed := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == paths.LegacyPreloadLib {
			removed = true
			continue
		}
		if strings.TrimSpace(line) != "" {
			kept = append(kept, line)
		}
	}
	if !removed {
		return
	}
	m.log.Infof("removing the stale %s entry for %s left by an older version",
		paths.LdSoPreload, paths.LegacyPreloadLib)
	if len(kept) == 0 {
		_ = os.Remove(paths.LdSoPreload)
		return
	}
	_ = os.WriteFile(paths.LdSoPreload, []byte(strings.Join(kept, "\n")+"\n"), 0o644)
}

// loadRegistry builds a fresh immutable snapshot and publishes it.
func (m *Manager) loadRegistry() {
	ctx := unitfile.DefaultSpecifierContext("")
	ctx.MachineID = m.machineID
	ctx.BootID = m.bootID
	loader := &unitfile.Loader{Root: m.opts.Root, SpecifierBase: ctx}

	reg, err := loader.Load()
	if err != nil {
		// The previous snapshot and its loader stay in place: a reload that
		// fails must not leave the manager with no units at all.
		m.log.Errorf("cannot load unit files: %v", err)
		return
	}
	// Published together, so a reader never pairs a new registry with the
	// loader of the previous generation.
	m.regMu.Lock()
	m.registry, m.loader = reg, loader
	m.regMu.Unlock()

	loaded, masked, bad := 0, 0, 0
	for _, n := range reg.Names() {
		u := reg.Get(n)
		switch u.LoadState {
		case unitfile.LoadLoaded:
			loaded++
		case unitfile.LoadMasked:
			masked++
		case unitfile.LoadBadSetting:
			bad++
			m.log.Errorf("%s failed to load: %s", n, u.LoadError)
		}
		for _, w := range u.Warnings {
			m.log.Warnf("%s", w)
		}
	}
	m.log.Infof("loaded %d units (%d masked, %d failed to load)", loaded, masked, bad)
}

// UnitState returns the manager's replica of a unit's state.
func (m *Manager) UnitState(name string) proto.UnitStatus {
	name = unitfile.CanonicalName(name)
	reg := m.Registry()
	st := proto.UnitStatus{
		Name:        name,
		LoadState:   unitfile.LoadNotFound,
		ActiveState: proto.StateInactive,
		SubState:    "dead",
		Result:      proto.ResultSuccess,
	}
	u := m.Get(name)
	if u != nil {
		st.Names = u.Names
		st.Description = u.Unit.Description
		st.LoadState = u.LoadState
		st.LoadError = u.LoadError
		st.FragmentPath = u.FragmentPath
		st.DropInPaths = u.DropInPaths
		st.InvocationID = u.InvocationID
		if u.Service != nil {
			st.Type = u.Service.Type
			st.Restart = u.Service.Restart
		}
		for _, w := range u.Warnings {
			st.Warnings = append(st.Warnings, w.String())
		}
		if reg != nil {
			st.UnitFileState = reg.UnitFileState(name)
		}
		if u.Unit.Description == "" {
			st.Description = name
		}
	}

	m.runMu.Lock()
	r := m.runners[name]
	isTarget := m.targets[name]
	last, hasLast := m.lastState[name]
	m.runMu.Unlock()

	if r != nil || hasLast {
		rep := last
		if r != nil {
			rep = r.Report()
		}
		st.ActiveState = rep.State
		st.SubState = rep.SubState
		st.MainPID = rep.MainPID
		st.ExecMainPID = rep.MainPID
		st.ExecMainStatus = rep.ExecMainStatus
		st.Result = rep.Result
		st.StatusText = rep.StatusText
		st.NRestarts = rep.NRestarts
		st.ConditionOK = rep.ConditionOK
		st.Tasks = rep.Tasks
		st.Processes = rep.Processes
		st.Since = rep.Since
	} else if isTarget {
		st.ActiveState = proto.StateActive
		st.SubState = "active"
	}
	return st
}

// ListUnits returns every unit the manager knows about.
func (m *Manager) ListUnits(types, states []string, all bool) []proto.UnitStatus {
	reg := m.Registry()
	if reg == nil {
		return nil
	}
	var out []proto.UnitStatus
	for _, n := range reg.Names() {
		st := m.UnitState(n)
		if len(types) > 0 && !matchAny(types, strings.TrimPrefix(unitfile.UnitSuffix(n), ".")) {
			continue
		}
		if len(states) > 0 && !matchAny(states, st.ActiveState) && !matchAny(states, st.SubState) {
			continue
		}
		if !all && st.ActiveState == proto.StateInactive && st.LoadState != unitfile.LoadBadSetting {
			continue
		}
		out = append(out, st)
	}
	return out
}

func matchAny(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// ListUnitFiles enumerates unit files and their enable state.
func (m *Manager) ListUnitFiles(types []string) []proto.UnitFileInfo {
	reg := m.Registry()
	if reg == nil {
		return nil
	}
	var out []proto.UnitFileInfo
	for _, n := range reg.Names() {
		u := reg.Get(n)
		if u == nil || u.Synthetic {
			continue
		}
		if len(types) > 0 && !matchAny(types, strings.TrimPrefix(unitfile.UnitSuffix(n), ".")) {
			continue
		}
		out = append(out, proto.UnitFileInfo{
			Name:  n,
			State: reg.UnitFileState(n),
			Path:  u.FragmentPath,
		})
	}
	return out
}

// Capabilities reports the probe results and runtime paths, so a bug report can
// carry them in one paste.
func (m *Manager) Capabilities() proto.Capabilities {
	c := proto.Capabilities{
		Backend:       string(m.be.Name()),
		Probes:        m.probes,
		RuntimeDir:    paths.RuntimeDir,
		ControlSocket: paths.ControlSocket,
		LogDir:        paths.LogDir,
		Version:       m.opts.Version,
		Warnings:      m.warnings,
	}
	reg := m.Registry()
	if reg != nil {
		for _, n := range reg.Names() {
			u := reg.Get(n)
			if u == nil {
				continue
			}
			for _, w := range u.Warnings {
				if w.Action == "ignored" && w.Directive != "" {
					c.Warnings = append(c.Warnings, w.String())
				}
			}
		}
	}
	return c
}

// Cat returns the fragment and drop-ins of a unit with `# <path>` headers.
func (m *Manager) Cat(name string) (string, error) {
	u := m.Get(name)
	if u == nil {
		return "", fmt.Errorf("No files found for %s.", unitfile.CanonicalName(name))
	}
	var b strings.Builder
	if u.FragmentPath != "" {
		data, err := os.ReadFile(u.FragmentPath)
		if err == nil {
			fmt.Fprintf(&b, "# %s\n%s", u.FragmentPath, string(data))
			if !strings.HasSuffix(string(data), "\n") {
				b.WriteString("\n")
			}
		}
	}
	for _, d := range u.DropInPaths {
		data, err := os.ReadFile(d)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n# %s\n%s", d, string(data))
		if !strings.HasSuffix(string(data), "\n") {
			b.WriteString("\n")
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("No files found for %s.", u.Name)
	}
	return b.String(), nil
}

// unitEnvironment assembles the environment block passed to a supervisor.
func (m *Manager) unitEnvironment() []string {
	var base []string
	switch m.opts.PassEnvironment {
	case "none":
		base = nil
	case "all", "":
		base = os.Environ()
	default:
		want := strings.Split(m.opts.PassEnvironment, ",")
		for _, e := range os.Environ() {
			if eq := strings.IndexByte(e, '='); eq > 0 && matchAny(want, e[:eq]) {
				base = append(base, e)
			}
		}
	}
	m.envMu.Lock()
	for k, v := range m.managerEnv {
		base = append(base, k+"="+v)
	}
	m.envMu.Unlock()
	return base
}

// SetEnvironment records a manager environment variable applied to
// subsequently started units.
//
// v0.5.x had set-environment and unset-environment swapped: cmdSetEnvironment
// called os.Unsetenv and vice versa (defect B3).
func (m *Manager) SetEnvironment(assignments []string) error {
	m.envMu.Lock()
	defer m.envMu.Unlock()
	for _, a := range assignments {
		eq := strings.IndexByte(a, '=')
		if eq <= 0 {
			return fmt.Errorf("%q is not a KEY=VALUE assignment", a)
		}
		m.managerEnv[a[:eq]] = a[eq+1:]
	}
	return nil
}

// UnsetEnvironment removes manager environment variables.
func (m *Manager) UnsetEnvironment(names []string) {
	m.envMu.Lock()
	defer m.envMu.Unlock()
	for _, n := range names {
		delete(m.managerEnv, n)
	}
}

// ShowEnvironment returns the manager environment block, sorted.
func (m *Manager) ShowEnvironment() []string {
	m.envMu.Lock()
	defer m.envMu.Unlock()
	return unitfile.EnvSlice(m.managerEnv)
}

// DaemonReload rebuilds the registry. Running units keep the configuration
// they were started with: their supervisor already holds it, so a reload can
// never make ExecStop= come from a different generation than ExecStart= did
// (defect C17).
func (m *Manager) DaemonReload() {
	m.log.Infof("reloading unit files")
	m.loadRegistry()
}

// bootTransaction starts the default target and reports how many units came up.
func (m *Manager) bootTransaction(target string) (started, failed int) {
	t, err := graph.BuildStart(m, []string{target})
	if err != nil {
		m.log.Errorf("cannot build the boot transaction: %v", err)
		return 0, 0
	}
	for _, w := range t.Warnings {
		m.log.Warnf("%s", w)
	}
	res := m.execute(t, nil)
	for _, ok := range res {
		if ok {
			started++
		} else {
			failed++
		}
	}
	return started, failed
}

// resolveLogFile is used by journalctl-side helpers running in the manager.
func (m *Manager) resolveLogFile(unit string) string {
	return filepath.Join(paths.LogDir, unit+".log")
}
