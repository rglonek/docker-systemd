package manager

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"docker-systemd/internal/journal"
	"docker-systemd/internal/paths"
	"docker-systemd/internal/proctree"
	"docker-systemd/internal/proto"
	"docker-systemd/internal/supervisor"
	"docker-systemd/internal/unitfile"
)

// runner is the manager's handle on one unit's supervisor process.
//
// Invariant I4: the supervisor's *process exit* is the authoritative "this unit
// is fully stopped" event. The STATE stream is advisory replication, so a
// supervisor that dies without reporting still produces a correct manager-side
// transition.
type runner struct {
	mgr        *Manager
	name       string
	invocation string
	unit       *unitfile.Unit

	conn net.Conn
	proc *os.Process

	mu     sync.Mutex
	report proto.StateReport

	done     chan struct{}
	doneOnce sync.Once
	// exited records the supervisor's own exit status.
	exitCode int
	// stopping marks a deliberate stop, so an exit is not treated as a crash.
	stopping bool
	// ready is closed once the supervisor has reported a terminal activation
	// outcome (active, failed or inactive).
	ready     chan struct{}
	readyOnce sync.Once
}

// Report returns the latest state replica.
func (r *runner) Report() proto.StateReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.report
}

// Done is closed when the supervisor process exits.
func (r *runner) Done() <-chan struct{} { return r.done }

// startUnit spawns a supervisor for a unit and waits for its activation to
// settle, bounded by TimeoutStartSec.
func (m *Manager) startUnit(u *unitfile.Unit) error {
	if u.Service == nil {
		// A target has no processes: it is a synchronisation point.
		m.runMu.Lock()
		m.targets[u.Name] = true
		m.runMu.Unlock()
		return nil
	}
	if u.LoadState == unitfile.LoadMasked {
		return fmt.Errorf("Unit %s is masked.", u.Name)
	}
	if u.LoadState != unitfile.LoadLoaded {
		return fmt.Errorf("Unit %s could not be loaded: %s", u.Name, u.LoadError)
	}
	if u.Unit.RefuseManualStart && !m.shuttingDown.Load() {
		return fmt.Errorf("Operation refused, unit %s may be requested by dependency only.", u.Name)
	}

	m.runMu.Lock()
	if existing, ok := m.runners[u.Name]; ok {
		m.runMu.Unlock()
		st := existing.Report()
		if st.State == proto.StateActive || st.State == proto.StateActivating {
			return nil
		}
		// A finished runner lingering in the map: wait for it to clear.
		<-existing.Done()
		m.runMu.Lock()
	}
	m.runMu.Unlock()

	inst := *u
	inst.InvocationID = unitfile.NewInvocationID()

	r, err := m.spawnSupervisor(&inst, "start", nil)
	if err != nil {
		return err
	}

	timeout := inst.Service.EffectiveTimeoutStart() + 5*time.Second
	select {
	case <-r.ready:
	case <-r.Done():
	case <-time.After(timeout):
		m.log.Warnf("%s: activation did not settle within %s", u.Name, timeout)
	}

	st := r.Report()
	if st.State == proto.StateFailed {
		msg := st.StatusText
		if msg == "" {
			msg = st.Result
		}
		return fmt.Errorf("Job for %s failed: %s", u.Name, msg)
	}
	return nil
}

// spawnSupervisor forks `/proc/self/exe --supervise` with a socketpair on
// fd 3 and sends it the resolved unit configuration.
func (m *Manager) spawnSupervisor(u *unitfile.Unit, mode string, recovered []int) (*runner, error) {
	// SOCK_STREAM rather than SOCK_SEQPACKET: the length-prefixed framing
	// already provides message boundaries, and SOCK_STREAM is what Go's net
	// package handles without special cases.
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "supervisor-control")
	childFile := os.NewFile(uintptr(fds[1]), "supervisor-control-child")

	conn, err := net.FileConn(parentFile)
	parentFile.Close()
	if err != nil {
		childFile.Close()
		return nil, fmt.Errorf("control conn: %w", err)
	}

	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}
	attr := &os.ProcAttr{
		Files: []*os.File{nil, nil, os.Stderr, childFile},
		Env:   os.Environ(),
		Sys:   &syscall.SysProcAttr{},
	}
	proc, err := os.StartProcess(self, []string{"docker-systemd-supervisor", "--supervise", u.Name}, attr)
	childFile.Close()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("cannot start a supervisor: %w", err)
	}

	r := &runner{
		mgr:        m,
		name:       u.Name,
		invocation: u.InvocationID,
		unit:       u,
		conn:       conn,
		proc:       proc,
		done:       make(chan struct{}),
		ready:      make(chan struct{}),
		report: proto.StateReport{
			State: proto.StateActivating, SubState: "start", Result: proto.ResultSuccess,
		},
	}

	m.runMu.Lock()
	m.runners[u.Name] = r
	m.runMu.Unlock()

	cfg := supervisor.Config{
		Unit:          u,
		Environment:   m.unitEnvironment(),
		Backend:       m.be.Name(),
		HeartbeatMS:   m.opts.HeartbeatMS,
		Mode:          mode,
		RecoveredPIDs: recovered,
	}
	if u.Service != nil &&
		(u.Service.Type == unitfile.TypeNotify || u.Service.Type == unitfile.TypeNotifyReload) {
		cfg.NotifySocket = filepath.Join(paths.NotifyDir, u.Name+".sock")
	}
	if err := proto.WriteJSON(conn, proto.TypeConfig, cfg); err != nil {
		_ = proc.Kill()
		conn.Close()
		m.forgetRunner(r)
		return nil, fmt.Errorf("cannot send the unit configuration: %w", err)
	}

	go r.readLoop()
	go r.waitProcess()
	return r, nil
}

// readLoop consumes the supervisor's frames.
func (r *runner) readLoop() {
	for {
		f, err := proto.Read(r.conn)
		if err != nil {
			return
		}
		switch f.Type {
		case proto.TypeState:
			var rep proto.StateReport
			if err := json.Unmarshal(f.Payload, &rep); err != nil {
				continue
			}
			r.mu.Lock()
			r.report = rep
			r.mu.Unlock()
			switch rep.State {
			case proto.StateActive, proto.StateFailed, proto.StateInactive:
				r.readyOnce.Do(func() { close(r.ready) })
			}
		case proto.TypeLogRecord:
			var rec proto.LogRecord
			if err := json.Unmarshal(f.Payload, &rec); err != nil {
				continue
			}
			r.mgr.broker.Write(journal.Record{
				Time:       time.Unix(0, rec.TimeUnixNS),
				Priority:   rec.Priority,
				Unit:       rec.Unit,
				Identifier: rec.Identifier,
				PID:        rec.PID,
				Message:    rec.Message,
			})
		case proto.TypeExited:
			var ex proto.ExitReport
			if err := json.Unmarshal(f.Payload, &ex); err == nil && ex.IsMain {
				r.mgr.log.Debugf("%s: main process %d exited (code=%d signal=%d)",
					r.name, ex.PID, ex.Code, ex.Signal)
			}
		case proto.TypeNotify:
			var n proto.NotifyReport
			if err := json.Unmarshal(f.Payload, &n); err == nil {
				r.mgr.log.Tracef("%s: notify %s=%s from pid %d", r.name, n.Key, n.Value, n.SenderPID)
			}
		}
	}
}

// waitProcess turns the supervisor's exit into the unit's terminal transition.
func (r *runner) waitProcess() {
	state, err := r.proc.Wait()
	code := 0
	if err == nil && state != nil {
		code = state.ExitCode()
	}
	r.exitCode = code
	r.conn.Close()

	r.mu.Lock()
	stopping := r.stopping
	last := r.report
	r.mu.Unlock()

	if !stopping && last.State != proto.StateFailed && last.State != proto.StateInactive {
		// The supervisor died without reporting a terminal state. Its
		// descendants have reparented to PID 1; recover them by their
		// invocation marker and hand them to a replacement (03 §6.8).
		r.mgr.log.Errorf("%s: supervisor exited unexpectedly with code %d", r.name, code)
		r.mu.Lock()
		r.report.State = proto.StateFailed
		r.report.SubState = "failed"
		r.report.Result = proto.ResultProtocol
		r.mu.Unlock()
		r.mgr.recoverOrphans(r)
	}
	r.readyOnce.Do(func() { close(r.ready) })
	r.doneOnce.Do(func() { close(r.done) })
	r.mgr.forgetRunner(r)
}

// forgetRunner removes a runner from the map if it is still the current one.
func (m *Manager) forgetRunner(r *runner) {
	m.runMu.Lock()
	if cur, ok := m.runners[r.name]; ok && cur == r {
		delete(m.runners, r.name)
		m.lastState[r.name] = r.Report()
	}
	m.runMu.Unlock()
}

// recoverOrphans finds the processes of a lost supervisor's unit and hands them
// to a replacement supervisor that adopts them.
func (m *Manager) recoverOrphans(r *runner) {
	if m.shuttingDown.Load() {
		return
	}
	pids := proctree.FindByEnv("MANAGED_BY_INVOCATION", r.invocation)
	if len(pids) == 0 {
		m.log.Infof("%s: no orphaned processes survived the supervisor", r.name)
		return
	}
	m.log.Warnf("%s: recovering %d orphaned process(es)", r.name, len(pids))
	if _, err := m.spawnSupervisor(r.unit, "adopt", pids); err != nil {
		m.log.Errorf("%s: cannot start a replacement supervisor: %v; killing the orphans", r.name, err)
		for _, pid := range pids {
			if ref, ok := proctree.Lookup(pid); ok {
				_ = proctree.Signal(ref, syscall.SIGTERM)
			}
		}
	}
}

// stopUnit asks a unit's supervisor to stop and waits for it to exit.
func (m *Manager) stopUnit(name string, mode proto.StopMode, timeout time.Duration) error {
	name = unitfile.CanonicalName(name)
	m.runMu.Lock()
	r := m.runners[name]
	isTarget := m.targets[name]
	m.runMu.Unlock()

	if r == nil {
		if isTarget {
			m.runMu.Lock()
			delete(m.targets, name)
			m.runMu.Unlock()
		}
		return nil
	}
	if u := m.Get(name); u != nil && u.Unit.RefuseManualStop && !m.shuttingDown.Load() {
		return fmt.Errorf("Operation refused, unit %s may be requested by dependency only.", name)
	}

	r.mu.Lock()
	r.stopping = true
	r.mu.Unlock()

	req := proto.StopRequest{Mode: mode, TimeoutMS: timeout.Milliseconds()}
	if err := proto.WriteJSON(r.conn, proto.TypeStop, req); err != nil {
		// The supervisor is unreachable; SIGKILL it and let orphan recovery
		// clean up. A supervisor that stops responding is not allowed to block
		// a stop request (08 §6).
		_ = r.proc.Signal(syscall.SIGKILL)
	}
	select {
	case <-r.Done():
		return nil
	case <-time.After(timeout + 10*time.Second):
		m.log.Errorf("%s: supervisor did not exit within the stop budget; killing it", name)
		_ = r.proc.Signal(syscall.SIGKILL)
		<-r.Done()
		return fmt.Errorf("unit %s did not stop cleanly", name)
	}
}

// reloadUnit sends a reload request.
func (m *Manager) reloadUnit(name string) error {
	name = unitfile.CanonicalName(name)
	m.runMu.Lock()
	r := m.runners[name]
	m.runMu.Unlock()
	if r == nil {
		return fmt.Errorf("Unit %s is not active, cannot reload.", name)
	}
	return proto.WriteJSON(r.conn, proto.TypeReload, struct{}{})
}

// killUnit sends a signal to a unit's processes.
func (m *Manager) killUnit(name, signal, who string) error {
	name = unitfile.CanonicalName(name)
	m.runMu.Lock()
	r := m.runners[name]
	m.runMu.Unlock()
	if r == nil {
		return fmt.Errorf("Unit %s is not active.", name)
	}
	if who == "" {
		who = "all"
	}
	if signal == "" {
		signal = "SIGTERM"
	}
	if unitfile.SignalNumber(signal) == 0 {
		return fmt.Errorf("Failed to parse signal string %s.", signal)
	}
	return proto.WriteJSON(r.conn, proto.TypeKill, proto.KillRequest{Signal: signal, Who: who})
}

// activeUnits lists the units with a live supervisor or an active target.
func (m *Manager) activeUnits() []string {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	out := make([]string, 0, len(m.runners)+len(m.targets))
	for n := range m.runners {
		out = append(out, n)
	}
	for n := range m.targets {
		out = append(out, n)
	}
	return out
}
