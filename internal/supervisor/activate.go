package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"docker-systemd/internal/paths"
	"docker-systemd/internal/proctree"
	"docker-systemd/internal/proto"
	"docker-systemd/internal/unitfile"
)

// activate runs the startup sequence of 03 §6.3. It reports true when the unit
// reached ACTIVE.
func (s *Supervisor) activate() bool {
	svc := s.unit.Service
	s.setState(proto.StateActivating, "start-pre", "")
	s.report()

	cred, err := resolveCredentials(svc)
	if err != nil {
		s.fail(proto.ResultResources, "%v (status=%d/USER)", err, ExitUser)
		return false
	}
	s.cred = cred

	env, warns := s.buildEnvironment(cred)
	for _, w := range warns {
		s.log.Warnf("%s", w)
	}
	s.env = env

	// Conditions are evaluated after the environment is assembled, because
	// ConditionEnvironment= reads it.
	res := unitfile.EvaluateConditions(s.unit.Unit.Conditions, env)
	s.stateMu.Lock()
	s.conditionOK = res.OK
	s.stateMu.Unlock()
	if !res.OK {
		if res.Assert {
			s.fail(proto.ResultCondition, "%s", res.Message)
			return false
		}
		// A failed Condition*= skips the unit: it goes inactive, not failed.
		s.log.Infof("%s, skipping", res.Message)
		s.setState(proto.StateInactive, "dead", proto.ResultSuccess)
		s.report()
		return false
	}

	if err := s.createRuntimeDirs(cred); err != nil {
		s.fail(proto.ResultResources, "%v", err)
		return false
	}

	if svc.Type == unitfile.TypeNotify || svc.Type == unitfile.TypeNotifyReload {
		path := s.cfg.NotifySocket
		if path == "" {
			path = filepath.Join(paths.NotifyDir, s.unit.Name+".sock")
		}
		// activate() runs again on every Restart=; the previous listener still
		// holds the now-unlinked socket, and the run loop still selects on its
		// channel, so it has to go before a new one takes the path.
		if s.notify != nil {
			s.notify.Close()
			s.notify = nil
		}
		l, err := NewNotifyListener(path, cred)
		if err != nil {
			s.fail(proto.ResultResources, "cannot create $NOTIFY_SOCKET: %v", err)
			return false
		}
		s.notify = l
		s.env["NOTIFY_SOCKET"] = l.Path()
	}

	startDeadline := time.Now().Add(svc.EffectiveTimeoutStart())

	// ExecCondition=: a non-zero exit skips the unit without failing it.
	for _, c := range svc.ExecCondition {
		st, err := s.runSync(c, s.env, startDeadline)
		if err != nil {
			s.fail(proto.ResultResources, "ExecCondition= failed: %v", err)
			return false
		}
		if st.Failed() {
			s.log.Infof("ExecCondition= returned %s, skipping", st)
			s.setState(proto.StateInactive, "dead", proto.ResultSuccess)
			s.report()
			return false
		}
	}

	for _, c := range svc.ExecStartPre {
		st, err := s.runSync(c, s.env, startDeadline)
		if err != nil {
			s.fail(proto.ResultTimeout, "ExecStartPre= failed: %v", err)
			return false
		}
		if st.Failed() && !c.IgnoreFailure {
			s.fail(proto.ResultExitCode, "ExecStartPre=%s failed with %s", c.Raw, st)
			return false
		}
	}

	s.setSubState("start")
	s.report()

	if svc.Type == unitfile.TypeOneshot {
		if !s.runOneshot(startDeadline) {
			return false
		}
	} else if !s.startMain(startDeadline) {
		return false
	}

	for _, c := range svc.ExecStartPost {
		st, err := s.runSync(c, s.reloadEnv(), startDeadline)
		if err != nil {
			s.fail(proto.ResultTimeout, "ExecStartPost= failed: %v", err)
			return false
		}
		if st.Failed() && !c.IgnoreFailure {
			s.fail(proto.ResultExitCode, "ExecStartPost=%s failed with %s", c.Raw, st)
			return false
		}
	}

	if svc.Type == unitfile.TypeOneshot {
		if svc.RemainAfterExit {
			s.setState(proto.StateActive, "exited", proto.ResultSuccess)
		} else {
			// Type=oneshot no longer force-sets RemainAfterExit: a oneshot
			// unit goes inactive after it finishes, as in systemd (B20).
			s.setState(proto.StateInactive, "dead", proto.ResultSuccess)
		}
		s.report()
		return svc.RemainAfterExit
	}

	s.setState(proto.StateActive, "running", proto.ResultSuccess)
	s.report()
	s.writeStateFile()
	return true
}

// fail records a failure, logs it and reports it.
func (s *Supervisor) fail(result, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	s.log.Errorf("%s", msg)
	s.setStatusText(msg)
	s.setState(proto.StateFailed, "failed", result)
	s.exitCode = 1
	s.report()
}

// runOneshot runs every ExecStart= line sequentially, each to completion. A
// non-zero exit without the `-` prefix fails the unit and skips the rest.
func (s *Supervisor) runOneshot(deadline time.Time) bool {
	for _, c := range s.unit.Service.ExecStart {
		st, err := s.runSync(c, s.env, deadline)
		if err != nil {
			s.fail(proto.ResultTimeout, "ExecStart=%s: %v", c.Raw, err)
			return false
		}
		if st.Failed() && !c.IgnoreFailure {
			s.stateMu.Lock()
			s.execMainStatus = st.Code
			s.stateMu.Unlock()
			s.fail(proto.ResultExitCode, "ExecStart=%s failed with %s", c.Raw, st)
			return false
		}
	}
	return true
}

// startMain spawns the unit's main process and determines MainPID according to
// the table in 03 §7.
func (s *Supervisor) startMain(deadline time.Time) bool {
	svc := s.unit.Service
	if len(svc.ExecStart) == 0 {
		s.fail(proto.ResultProtocol, "no ExecStart=")
		return false
	}
	cmd := svc.ExecStart[0]
	p, err := s.spawn(cmd, s.env, s.cred, true)
	if err != nil {
		s.fail(proto.ResultResources, "%v", err)
		return false
	}
	s.mainProc = p
	s.mainExit = p.exit

	switch svc.Type {
	case unitfile.TypeSimple, unitfile.TypeIdle, unitfile.TypeDBus:
		// Type=dbus degrades to simple: there is no bus here.
		s.setMainPID(p.pid)
		return true

	case unitfile.TypeExec:
		// Wait for a successful execve before declaring the unit active. The
		// trampoline's status pipe gives an exact answer: EOF means the exec
		// happened, a message means it did not.
		code, msg := p.execOutcome()
		if code != 0 {
			s.setMainPID(0)
			s.fail(proto.ResultExitCode, "%s (status=%d/%s)", msg, code, execCodeName(code))
			return false
		}
		s.setMainPID(p.pid)
		return true

	case unitfile.TypeNotify, unitfile.TypeNotifyReload:
		return s.waitReady(p, deadline)

	case unitfile.TypeForking:
		return s.waitForking(p, deadline)

	default:
		s.setMainPID(p.pid)
		return true
	}
}

func execCodeName(code int) string {
	switch code {
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

// waitReady implements Type=notify: MainPID is the sender of READY=1, or the
// pid given in MAINPID=, validated as a tree member. TimeoutStartSec applies to
// the wait.
func (s *Supervisor) waitReady(p *process, deadline time.Time) bool {
	s.setMainPID(p.pid)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			s.fail(proto.ResultTimeout, "start timed out waiting for READY=1 on $NOTIFY_SOCKET")
			s.terminateTree(time.Now().Add(s.unit.Service.EffectiveTimeoutStop()), true)
			return false
		}
		select {
		case n := <-s.notify.Notifications():
			s.send(proto.TypeNotify, proto.NotifyReport{
				SenderPID: n.SenderPID, Key: n.Key, Value: n.Value,
			})
			switch n.Key {
			case "STATUS":
				s.setStatusText(n.Value)
			case "MAINPID":
				pid, err := strconv.Atoi(strings.TrimSpace(n.Value))
				if err == nil && s.inTree(pid) {
					s.setMainPID(pid)
				} else if err == nil {
					s.log.Warnf("ignoring MAINPID=%d: not a member of this unit's process tree", pid)
				}
			case "READY":
				if n.Value != "1" {
					break
				}
				if !s.notifierAllowed(n.SenderPID) {
					s.log.Warnf("ignoring READY=1 from pid %d: not a member of this unit's process tree",
						n.SenderPID)
					break
				}
				if s.MainPID() == p.pid && n.SenderPID != p.pid && s.inTree(n.SenderPID) {
					s.setMainPID(n.SenderPID)
				}
				return true
			}
		case st, ok := <-s.mainExit:
			if !ok {
				s.mainExit = nil
				continue
			}
			s.mainExit = nil
			// The direct child exiting before READY=1 is fine if the daemon
			// forked: the tree is still populated and one of its members will
			// send the notification.
			if s.handle.IsEmpty() {
				s.stateMu.Lock()
				s.execMainStatus = st.Code
				s.stateMu.Unlock()
				s.fail(proto.ResultExitCode, "main process exited before READY=1 (%s)", st)
				return false
			}
		case <-time.After(minDuration(remaining, 250*time.Millisecond)):
		case <-s.quit:
			return false
		}
	}
}

// waitForking implements Type=forking: the direct child exits, and MainPID is
// then resolved from PIDFile= or from the tree.
func (s *Supervisor) waitForking(p *process, deadline time.Time) bool {
	svc := s.unit.Service
	select {
	case st, ok := <-s.mainExit:
		s.mainExit = nil
		if ok && st.Failed() {
			s.stateMu.Lock()
			s.execMainStatus = st.Code
			s.stateMu.Unlock()
			s.fail(proto.ResultExitCode, "control process exited with %s", st)
			s.terminateTree(time.Now().Add(svc.EffectiveTimeoutStop()), true)
			return false
		}
	case <-time.After(time.Until(deadline)):
		s.fail(proto.ResultTimeout, "start timed out waiting for the forking parent to exit")
		s.terminateTree(time.Now().Add(svc.EffectiveTimeoutStop()), true)
		return false
	case <-s.quit:
		return false
	}

	if svc.PIDFile != "" {
		pid, err := s.readPIDFile(deadline)
		if err != nil {
			s.fail(proto.ResultProtocol, "%v", err)
			s.terminateTree(time.Now().Add(svc.EffectiveTimeoutStop()), true)
			return false
		}
		s.setMainPID(pid)
		return true
	}

	if !svc.GuessMainPID {
		return true
	}

	members, err := s.settledTree(deadline)
	if err != nil {
		s.fail(proto.ResultResources, "cannot enumerate the process tree: %v", err)
		return false
	}
	switch len(members) {
	case 0:
		if svc.RemainAfterExit {
			s.setMainPID(0)
			return true
		}
		s.fail(proto.ResultProtocol, "the unit exited during startup and left no processes")
		return false
	case 1:
		s.setMainPID(members[0].PID)
		return true
	default:
		// More than one: the main process is the member parented directly by
		// the supervisor with the earliest start time.
		best := -1
		var bestStart uint64
		self := os.Getpid()
		for _, m := range members {
			if m.PPID != self {
				continue
			}
			if best < 0 || m.StartTime < bestStart {
				best, bestStart = m.PID, m.StartTime
			}
		}
		if best < 0 {
			best = members[0].PID
		}
		s.setMainPID(best)
		return true
	}
}

// settledTree waits for the unit's membership to stop changing before guessing
// a main PID.
//
// A daemonising process exits through one or two intermediate generations, and
// enumerating the instant the direct child exits can catch an intermediate that
// is about to disappear — which would leave MainPID pointing at a pid that no
// longer exists. Two consecutive identical samples is a cheap, sufficient
// settle condition, bounded by TimeoutStartSec.
func (s *Supervisor) settledTree(deadline time.Time) ([]proctree.ProcRef, error) {
	settleBy := time.Now().Add(2 * time.Second)
	if settleBy.After(deadline) {
		settleBy = deadline
	}
	prev, err := s.handle.Members()
	if err != nil {
		return nil, err
	}
	for time.Now().Before(settleBy) {
		select {
		case <-time.After(50 * time.Millisecond):
		case <-s.quit:
			return prev, nil
		}
		cur, err := s.handle.Members()
		if err != nil {
			return nil, err
		}
		if sameMembers(prev, cur) {
			return cur, nil
		}
		prev = cur
	}
	return prev, nil
}

func sameMembers(a, b []proctree.ProcRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].PID != b[i].PID || a[i].StartTime != b[i].StartTime {
			return false
		}
	}
	return true
}

// readPIDFile polls for the pid file, parses it strictly, and validates that
// the pid is a member of this unit's tree.
//
// v0.5.x extracted the pid by keeping every ASCII digit found anywhere in the
// file and never checked it, so a stale or hostile pid file made init adopt an
// unrelated process — and, once stopping actually signalled it, SIGKILL it
// (defects C13/E4).
func (s *Supervisor) readPIDFile(deadline time.Time) (int, error) {
	path := s.unit.Service.PIDFile
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, perr := parsePIDFile(string(data))
			if perr != nil {
				return 0, fmt.Errorf("PIDFile %s: %w", path, perr)
			}
			if !s.inTree(pid) {
				return 0, fmt.Errorf(
					"PIDFile %s points to pid %d which is not part of this unit", path, pid)
			}
			return pid, nil
		}
		if !time.Now().Before(deadline) {
			return 0, fmt.Errorf("PIDFile %s did not appear within TimeoutStartSec", path)
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-s.quit:
			return 0, fmt.Errorf("aborted")
		}
	}
}

// parsePIDFile accepts optional whitespace, one decimal integer, and optional
// trailing whitespace — and nothing else.
func parsePIDFile(content string) (int, error) {
	s := strings.TrimSpace(content)
	if s == "" {
		return 0, fmt.Errorf("is empty")
	}
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("does not contain a single decimal pid")
	}
	if pid <= 1 {
		return 0, fmt.Errorf("contains an implausible pid %d", pid)
	}
	return pid, nil
}

// runSync runs one command to completion, bounded by the deadline.
func (s *Supervisor) runSync(c unitfile.Command, env map[string]string, deadline time.Time) (ExitStatus, error) {
	p, err := s.spawn(c, env, s.cred, false)
	if err != nil {
		return ExitStatus{}, err
	}
	if code, msg := p.execOutcome(); code != 0 {
		<-p.exit
		return ExitStatus{PID: p.pid, Code: code}, fmt.Errorf("%s (status=%d/%s)", msg, code, execCodeName(code))
	}
	timeout := time.Until(deadline)
	if timeout <= 0 {
		timeout = time.Millisecond
	}
	select {
	case st := <-p.exit:
		return st, nil
	case <-time.After(timeout):
		// A hanging ExecStartPre= must fail its own unit, not the whole boot;
		// v0.5.x had no TimeoutStartSec at all and boot was serial under a
		// read lock, so one hang stopped everything (defect C11/A8).
		if ref, ok := proctree.Lookup(p.pid); ok {
			_ = proctree.Signal(ref, syscallSIGKILL)
		}
		<-p.exit
		return ExitStatus{PID: p.pid, Code: 1}, fmt.Errorf("timed out after %s", timeout)
	case <-s.abortCh():
		if ref, ok := proctree.Lookup(p.pid); ok {
			_ = proctree.Signal(ref, syscallSIGTERM)
		}
		return <-p.exit, nil
	}
}

// createRuntimeDirs creates the RuntimeDirectory= family. Stock units failing
// because their runtime directory did not exist is a frequent complaint
// against v0.5.x, which implemented none of these.
func (s *Supervisor) createRuntimeDirs(cred credentials) error {
	svc := s.unit.Service
	sets := []struct {
		base string
		dirs []string
		mode uint32
	}{
		{"/run", svc.RuntimeDirectory, svc.RuntimeDirectoryMode},
		{"/var/lib", svc.StateDirectory, svc.StateDirectoryMode},
		{"/var/cache", svc.CacheDirectory, svc.CacheDirectoryMode},
		{"/var/log", svc.LogsDirectory, svc.LogsDirectoryMode},
		{"/etc", svc.ConfigurationDirectory, svc.ConfigurationDirMode},
	}
	for _, set := range sets {
		for _, d := range set.dirs {
			p := filepath.Join(set.base, d)
			if err := os.MkdirAll(p, os.FileMode(set.mode)); err != nil {
				return fmt.Errorf("cannot create %s: %w", p, err)
			}
			if err := os.Chmod(p, os.FileMode(set.mode)); err != nil {
				return fmt.Errorf("cannot chmod %s: %w", p, err)
			}
			if cred.uid != nil || cred.gid != nil {
				uid, gid := -1, -1
				if cred.uid != nil {
					uid = *cred.uid
				}
				if cred.gid != nil {
					gid = *cred.gid
				}
				if err := os.Chown(p, uid, gid); err != nil {
					return fmt.Errorf("cannot chown %s: %w", p, err)
				}
			}
		}
	}
	return nil
}

// cleanupRuntimeDirs removes RuntimeDirectory= entries on stop unless
// RuntimeDirectoryPreserve= says otherwise. The other *Directory= families
// deliberately persist.
func (s *Supervisor) cleanupRuntimeDirs() {
	svc := s.unit.Service
	if svc.RuntimeDirectoryPreserve == "yes" || svc.RuntimeDirectoryPreserve == "restart" {
		return
	}
	for _, d := range svc.RuntimeDirectory {
		_ = os.RemoveAll(filepath.Join("/run", d))
	}
}

// writeStateFile records enough for daemon-reexec to reattach.
func (s *Supervisor) writeStateFile() {
	if err := os.MkdirAll(paths.UnitStateDir, paths.ModeStateDir); err != nil {
		return
	}
	content := fmt.Sprintf("unit=%s\ninvocation=%s\nsupervisor=%d\nmain=%d\n",
		s.unit.Name, s.unit.InvocationID, os.Getpid(), s.MainPID())
	_ = os.WriteFile(s.stateFilePath(), []byte(content), 0o600)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
