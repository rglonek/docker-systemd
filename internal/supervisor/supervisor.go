// Package supervisor implements the per-unit supervisor process: the
// PR_SET_CHILD_SUBREAPER host that owns one unit's processes.
//
// The whole design rests on two properties of subreaper adoption (03 §6.2):
//
//	P1 (closure)     no descendant can leave the subtree, so the unit's
//	                 membership is exactly the set of processes whose ppid
//	                 chain reaches this process;
//	P2 (attribution) every exit inside the subtree is reported here by wait(2),
//	                 including exits of processes this supervisor never spawned.
//
// A supervisor crash is contained: it takes down one unit, not the container.
package supervisor

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"docker-systemd/internal/backend"
	"docker-systemd/internal/logging"
	"docker-systemd/internal/paths"
	"docker-systemd/internal/proctree"
	"docker-systemd/internal/proto"
	"docker-systemd/internal/unitfile"
)

// ControlFD is the manager<->supervisor socketpair, inherited at spawn.
const ControlFD = 3

// Supervisor owns one unit.
type Supervisor struct {
	cfg  *Config
	unit *unitfile.Unit
	ctrl net.Conn
	log  *logging.Logger

	reaper  *Reaper
	backend backend.Backend
	handle  backend.Handle
	notify  *NotifyListener

	// sendMu serialises writes to the control socket.
	sendMu sync.Mutex

	// state and everything below it is owned by the run loop; the only
	// concurrent readers take stateMu. There is no shared mutable state
	// between units at all, which is what removes the A9 defect family.
	stateMu        sync.Mutex
	state          string
	subState       string
	mainPID        int
	execMainStatus int
	result         string
	statusText     string
	nRestarts      int
	since          time.Time
	conditionOK    bool
	// restartTimes is the sliding window backing StartLimitBurst=.
	restartTimes []time.Time

	mainProc *process
	mainExit <-chan ExitStatus
	// adopted holds pidfds for processes recovered from a dead supervisor.
	// P1 does not hold for them, so they are re-scanned by environment marker
	// on the heartbeat while any of them is alive.
	adopted []int

	env  map[string]string
	cred credentials

	ctrlFrames chan proto.Frame
	quit       chan struct{}
	quitOnce   sync.Once
	// exitCode is the supervisor's own exit status; the manager reads it as
	// the authoritative "unit fully stopped" event (invariant I4).
	exitCode int
}

// Run is the `--supervise` mode entry point. It returns the process exit code.
func Run() int {
	log := logging.New(os.Stderr, logging.LevelInfo, "supervisor")

	// PR_SET_CHILD_SUBREAPER must be set before anything is spawned.
	if err := proctree.SetSubreaper(); err != nil {
		log.Warnf("PR_SET_CHILD_SUBREAPER failed: %v; escaped processes will reparent to PID 1", err)
	}

	ctrlFile := os.NewFile(ControlFD, "control")
	if ctrlFile == nil {
		log.Errorf("no control socket on fd %d", ControlFD)
		return 1
	}
	conn, err := net.FileConn(ctrlFile)
	if err != nil {
		log.Errorf("control socket: %v", err)
		return 1
	}
	ctrlFile.Close()

	s := &Supervisor{
		ctrl:       conn,
		log:        log,
		ctrlFrames: make(chan proto.Frame, 32),
		quit:       make(chan struct{}),
		state:      proto.StateInactive,
		subState:   "dead",
		result:     proto.ResultSuccess,
	}

	// The first frame is the unit configuration.
	f, err := proto.Read(conn)
	if err != nil || f.Type != proto.TypeConfig {
		log.Errorf("expected CONFIG as the first frame: %v", err)
		return 1
	}
	var cfg Config
	if err := json.Unmarshal(f.Payload, &cfg); err != nil {
		log.Errorf("malformed CONFIG: %v", err)
		return 1
	}
	s.cfg = &cfg
	s.unit = cfg.Unit
	s.log = log.WithUnit(s.unit.Name)

	// A panic in unit handling must degrade one unit, not kill the container.
	defer func() {
		if r := recover(); r != nil {
			s.log.Errorf("supervisor panic: %v", r)
			s.setState(proto.StateFailed, "panic", proto.ResultProtocol)
			s.report()
			s.exitCode = 1
		}
	}()

	s.reaper = NewReaper()
	defer s.reaper.Stop()

	be, _ := backend.Select(cfg.Backend)
	s.backend = be
	h, err := be.Attach(s.unit.Name, os.Getpid())
	if err != nil {
		s.log.Warnf("backend %s attach failed: %v; falling back to subreaper tracking", be.Name(), err)
		fallback, _ := backend.Select(backend.Subreaper)
		s.backend = fallback
		h, err = fallback.Attach(s.unit.Name, os.Getpid())
		if err != nil {
			s.log.Errorf("cannot attach a membership backend: %v", err)
			return 1
		}
	}
	s.handle = h
	defer s.handle.Release()

	go s.readControl()
	s.serve()
	return s.exitCode
}

// readControl pumps frames from the manager onto the run loop's channel.
func (s *Supervisor) readControl() {
	for {
		f, err := proto.Read(s.ctrl)
		if err != nil {
			// The manager is gone. Stop the unit rather than leaving its
			// processes running with nobody watching.
			s.log.Warnf("control channel closed: %v", err)
			s.quitOnce.Do(func() { close(s.quit) })
			return
		}
		select {
		case s.ctrlFrames <- f:
		case <-s.quit:
			return
		}
	}
}

// send writes one frame to the manager.
func (s *Supervisor) send(typ uint8, v any) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	_ = proto.WriteJSON(s.ctrl, typ, v)
}

// report replicates the current state to the manager. The manager's copy is a
// replica (invariant I5); the authoritative signal remains this process's exit.
func (s *Supervisor) report() {
	s.stateMu.Lock()
	r := proto.StateReport{
		State:          s.state,
		SubState:       s.subState,
		MainPID:        s.mainPID,
		ExecMainStatus: s.execMainStatus,
		Result:         s.result,
		StatusText:     s.statusText,
		NRestarts:      s.nRestarts,
		ConditionOK:    s.conditionOK,
	}
	if !s.since.IsZero() {
		r.Since = s.since.UTC().Format(time.RFC3339Nano)
	}
	s.stateMu.Unlock()
	if members, err := s.handle.Members(); err == nil {
		r.Tasks = len(members)
		for _, m := range members {
			r.Processes = append(r.Processes, proto.ProcessInfo{
				PID: m.PID, Cmdline: proctree.Cmdline(m.PID),
			})
		}
	}
	s.send(proto.TypeState, r)
}

func (s *Supervisor) setState(state, sub, result string) {
	s.stateMu.Lock()
	s.state, s.subState = state, sub
	if result != "" {
		s.result = result
	}
	s.since = time.Now()
	s.stateMu.Unlock()
}

func (s *Supervisor) setSubState(sub string) {
	s.stateMu.Lock()
	s.subState = sub
	s.stateMu.Unlock()
}

// MainPID returns the unit's main process id.
func (s *Supervisor) MainPID() int {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.mainPID
}

func (s *Supervisor) setMainPID(pid int) {
	s.stateMu.Lock()
	s.mainPID = pid
	s.stateMu.Unlock()
}

func (s *Supervisor) setStatusText(t string) {
	s.stateMu.Lock()
	s.statusText = t
	s.stateMu.Unlock()
}

// serve runs the unit's lifecycle: activation, then the steady-state loop.
func (s *Supervisor) serve() {
	if s.cfg.Mode == "adopt" {
		s.adoptRecovered()
	} else if !s.activate() {
		// Activation failed or was skipped by a condition; the state is
		// already reported. Run ExecStopPost= and leave.
		s.runStopPost()
		s.report()
		return
	}

	heartbeat := time.Duration(s.cfg.HeartbeatMS) * time.Millisecond
	if heartbeat <= 0 {
		heartbeat = time.Second
	}
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()

	var notifyCh <-chan Notification
	if s.notify != nil {
		notifyCh = s.notify.Notifications()
	}

	for {
		select {
		case <-s.quit:
			s.stop(proto.StopShutdown, s.unit.Service.EffectiveTimeoutStop())
			return

		case f := <-s.ctrlFrames:
			if done := s.handleControl(f); done {
				return
			}

		case st, ok := <-s.mainExit:
			if !ok {
				s.mainExit = nil
				continue
			}
			s.mainExit = nil
			if done := s.handleMainExit(st); done {
				return
			}

		case n := <-notifyCh:
			s.handleNotification(n)

		case st := <-s.reaper.Adopted():
			// An escaped descendant exited. For ExitType=cgroup the unit is
			// only done when the whole tree is empty, so this is where that
			// is checked.
			s.send(proto.TypeExited, proto.ExitReport{PID: st.PID, Code: st.Code, Signal: st.Signal})
			if s.unit.Service.ExitType == "cgroup" && s.isActive() && s.handle.IsEmpty() {
				if done := s.handleMainExit(st); done {
					return
				}
			}

		case <-tick.C:
			s.heartbeat()
		}
	}
}

func (s *Supervisor) isActive() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.state == proto.StateActive
}

// heartbeat re-scans the tree only while the unit has members that are not
// direct children, and reports the current task count. For the overwhelmingly
// common Type=simple case with no escapees this costs nothing.
func (s *Supervisor) heartbeat() {
	members, err := s.handle.Members()
	if err != nil {
		return
	}
	if len(s.adopted) > 0 {
		s.rescanAdopted()
	}
	if len(members) == 0 && s.isActive() && s.unit.Service.Type == unitfile.TypeForking {
		// A forking unit whose whole tree vanished without us seeing a main
		// exit: treat it as the unit having stopped.
		s.log.Warnf("process tree is empty; unit stopped")
		s.setState(proto.StateInactive, "dead", proto.ResultSuccess)
		s.report()
		s.quitOnce.Do(func() { close(s.quit) })
		return
	}
	s.report()
}

// handleControl processes one manager request, returning true when the
// supervisor should exit.
func (s *Supervisor) handleControl(f proto.Frame) bool {
	switch f.Type {
	case proto.TypeStop:
		var req proto.StopRequest
		_ = json.Unmarshal(f.Payload, &req)
		timeout := s.unit.Service.EffectiveTimeoutStop()
		if req.TimeoutMS > 0 {
			timeout = time.Duration(req.TimeoutMS) * time.Millisecond
		}
		s.stop(req.Mode, timeout)
		return true

	case proto.TypeReload:
		s.reload()

	case proto.TypeKill:
		var req proto.KillRequest
		_ = json.Unmarshal(f.Payload, &req)
		s.kill(req)

	case proto.TypeQuery:
		s.report()
	}
	return false
}

// handleMainExit applies Restart= policy, returning true when the supervisor
// should exit.
func (s *Supervisor) handleMainExit(st ExitStatus) bool {
	svc := s.unit.Service
	s.stateMu.Lock()
	s.execMainStatus = st.Code
	s.stateMu.Unlock()
	s.send(proto.TypeExited, proto.ExitReport{
		PID: st.PID, Code: st.Code, Signal: st.Signal, IsMain: true,
	})

	success := s.isSuccess(st)
	if success {
		s.log.Infof("main process exited cleanly (%s)", st)
	} else {
		s.log.Warnf("main process exited, %s/%s", st, exitName(st))
	}

	if s.shouldRestart(st, success) {
		s.stateMu.Lock()
		s.nRestarts++
		n := s.nRestarts
		s.stateMu.Unlock()
		if s.startLimitHit(n) {
			s.log.Errorf("start request repeated too quickly (StartLimitBurst=%d); refusing to restart",
				s.unit.Unit.StartLimitBurst)
			s.setState(proto.StateFailed, "failed", proto.ResultStartLimitHit)
			s.report()
			s.runStopPost()
			s.exitCode = 1
			return true
		}
		s.setState(proto.StateAutoRestart, "auto-restart", "")
		s.report()
		// Terminate any survivors before restarting: a restart that leaves the
		// old tree running is how a service ends up with two masters.
		s.terminateTree(time.Now().Add(svc.EffectiveTimeoutStop()), true)
		select {
		case <-time.After(svc.RestartSec):
		case <-s.quit:
			return true
		}
		if s.activate() {
			return false
		}
		s.runStopPost()
		s.exitCode = 1
		return true
	}

	// No restart: settle into the final state.
	if svc.RemainAfterExit && success {
		s.setState(proto.StateActive, "exited", proto.ResultSuccess)
		s.setMainPID(0)
		s.report()
		// The unit stays active with no process; wait for an explicit stop.
		return false
	}
	deadline := time.Now().Add(svc.EffectiveTimeoutStop())
	s.setState(proto.StateDeactivating, "stop-sigterm", "")
	s.terminateTree(deadline, true)
	s.runStopPost()
	if success {
		s.setState(proto.StateInactive, "dead", proto.ResultSuccess)
	} else {
		s.setState(proto.StateFailed, "failed", resultFor(st))
		s.exitCode = 1
	}
	s.setMainPID(0)
	s.report()
	return true
}

func resultFor(st ExitStatus) string {
	if st.Signal != 0 {
		return proto.ResultSignal
	}
	return proto.ResultExitCode
}

func exitName(st ExitStatus) string {
	if st.Signal != 0 {
		return signalString(st.Signal)
	}
	switch st.Code {
	case ExitExec:
		return "EXEC"
	case ExitUser:
		return "USER"
	case ExitGroup:
		return "GROUP"
	case ExitChdir:
		return "CHDIR"
	case ExitLimits:
		return "LIMITS"
	case ExitNoNewPrivs:
		return "NO_NEW_PRIVILEGES"
	}
	return "n/a"
}

// isSuccess applies SuccessExitStatus= on top of the default "0 is success".
func (s *Supervisor) isSuccess(st ExitStatus) bool {
	svc := s.unit.Service
	if svc.SuccessExitStatus.Contains(st.Code, st.Signal) {
		return true
	}
	if st.Signal != 0 {
		// A unit stopped by our own termination ladder exits by signal and
		// that is not a failure.
		switch syscall.Signal(st.Signal) {
		case syscall.SIGTERM, syscall.SIGINT, syscall.SIGPIPE, syscall.SIGHUP:
			return true
		}
		return false
	}
	return st.Code == 0
}

// shouldRestart implements the Restart= table.
func (s *Supervisor) shouldRestart(st ExitStatus, success bool) bool {
	svc := s.unit.Service
	if svc.RestartPreventExitStatus.Contains(st.Code, st.Signal) {
		return false
	}
	if svc.RestartForceExitStatus.Contains(st.Code, st.Signal) {
		return true
	}
	abnormal := st.Signal != 0
	switch svc.Restart {
	case unitfile.RestartAlways:
		return true
	case unitfile.RestartOnSuccess:
		return success
	case unitfile.RestartOnFailure:
		return !success
	case unitfile.RestartOnAbnormal:
		return abnormal
	case unitfile.RestartOnAbort:
		return abnormal && (syscall.Signal(st.Signal) == syscall.SIGABRT)
	case unitfile.RestartOnWatchdog:
		return false
	default:
		return false
	}
}

// startLimitHit implements StartLimitIntervalSec=/StartLimitBurst=, which
// v0.5.x lacked entirely, so a crash-looping unit restarted forever (C12).
func (s *Supervisor) startLimitHit(n int) bool {
	burst := s.unit.Unit.StartLimitBurst
	if burst <= 0 {
		return false
	}
	if s.unit.Unit.StartLimitInterval <= 0 {
		return false
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.restartTimes = append(s.restartTimes, time.Now())
	cutoff := time.Now().Add(-s.unit.Unit.StartLimitInterval)
	kept := s.restartTimes[:0]
	for _, t := range s.restartTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	s.restartTimes = kept
	return len(s.restartTimes) > burst
}

// handleNotification applies one sd_notify assignment.
func (s *Supervisor) handleNotification(n Notification) {
	s.send(proto.TypeNotify, proto.NotifyReport{SenderPID: n.SenderPID, Key: n.Key, Value: n.Value})
	switch n.Key {
	case "STATUS":
		s.setStatusText(n.Value)
	case "MAINPID":
		var pid int
		if _, err := fmt.Sscanf(n.Value, "%d", &pid); err != nil {
			return
		}
		if !s.inTree(pid) {
			s.log.Warnf("ignoring MAINPID=%d: not a member of this unit's process tree", pid)
			return
		}
		s.setMainPID(pid)
	case "READY":
		if n.Value == "1" && s.MainPID() == 0 && n.SenderPID > 0 && s.inTree(n.SenderPID) {
			s.setMainPID(n.SenderPID)
		}
	case "STOPPING":
		if n.Value == "1" {
			s.setSubState("stop")
		}
	case "RELOADING":
		if n.Value == "1" {
			s.setState(proto.StateReloading, "reload", "")
		}
	case "ERRNO":
		s.setStatusText("errno " + n.Value)
	}
	s.report()
}

// inTree reports whether a pid is a member of this unit. Every pid obtained
// from outside our own fork — a pid file, a MAINPID= assignment — is validated
// this way before it is waited on or signalled (09 §4).
func (s *Supervisor) inTree(pid int) bool {
	members, err := s.handle.Members()
	if err != nil {
		return false
	}
	for _, m := range members {
		if m.PID == pid {
			return true
		}
	}
	return false
}

// kill implements `systemctl kill`.
func (s *Supervisor) kill(req proto.KillRequest) {
	sig := syscall.Signal(unitfile.SignalNumber(req.Signal))
	if sig == 0 {
		sig = syscall.SIGTERM
	}
	switch req.Who {
	case "main":
		if pid := s.MainPID(); pid > 0 {
			if ref, ok := proctree.Lookup(pid); ok {
				_ = proctree.Signal(ref, sig)
			}
		}
	default:
		_ = s.handle.KillAll(sig)
	}
}

// reload runs ExecReload= or, in its absence, sends SIGHUP to the main
// process — systemd's default.
func (s *Supervisor) reload() {
	svc := s.unit.Service
	s.stateMu.Lock()
	prev := s.state
	s.stateMu.Unlock()
	s.setState(proto.StateReloading, "reload", "")
	s.report()
	if len(svc.ExecReload) > 0 {
		deadline := time.Now().Add(svc.EffectiveTimeoutStart())
		env := s.reloadEnv()
		for _, c := range svc.ExecReload {
			if _, err := s.runSync(c, env, deadline); err != nil {
				s.log.Warnf("ExecReload= failed: %v", err)
			}
		}
	} else if pid := s.MainPID(); pid > 0 {
		if ref, ok := proctree.Lookup(pid); ok {
			_ = proctree.Signal(ref, syscall.SIGHUP)
		}
	}
	s.setState(prev, "running", "")
	s.report()
}

// reloadEnv is the unit environment with MAINPID injected, as systemd does for
// ExecReload=.
func (s *Supervisor) reloadEnv() map[string]string {
	env := map[string]string{}
	for k, v := range s.env {
		env[k] = v
	}
	env["MAINPID"] = fmt.Sprintf("%d", s.MainPID())
	return env
}

// runStopPost runs ExecStopPost= regardless of how the unit ended.
func (s *Supervisor) runStopPost() {
	svc := s.unit.Service
	if len(svc.ExecStopPost) == 0 {
		return
	}
	s.setSubState("stop-post")
	s.report()
	deadline := time.Now().Add(svc.EffectiveTimeoutStop())
	for _, c := range svc.ExecStopPost {
		if _, err := s.runSync(c, s.reloadEnv(), deadline); err != nil && !c.IgnoreFailure {
			s.log.Warnf("ExecStopPost= failed: %v", err)
		}
	}
	s.cleanupRuntimeDirs()
	if s.notify != nil {
		s.notify.Close()
	}
	_ = os.Remove(s.stateFilePath())
}

func (s *Supervisor) stateFilePath() string {
	return paths.UnitStateDir + "/" + s.unit.Name + ".state"
}
