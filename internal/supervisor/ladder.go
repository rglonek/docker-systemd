package supervisor

import (
	"syscall"
	"time"

	"docker-systemd/internal/proctree"
	"docker-systemd/internal/proto"
	"docker-systemd/internal/unitfile"
)

const (
	syscallSIGTERM = syscall.SIGTERM
	syscallSIGKILL = syscall.SIGKILL
)

// ladderInterval is how often the ladder re-enumerates while waiting.
const ladderInterval = 50 * time.Millisecond

// finalKillRounds bounds the SIGKILL escalation.
const finalKillRounds = 10

// stop implements the termination ladder of 03 §8.
//
// Three things distinguish it from v0.5.x, which could not stop a Type=forking
// service at all (defect B1):
//
//   - it signals *all* tree members, not just the direct children;
//   - it re-enumerates every round, so it converges even against a process
//     that keeps forking;
//   - the supervisor's own exit is the completion signal, so the manager needs
//     no polling and no per-unit bookkeeping to know the unit is gone.
func (s *Supervisor) stop(mode proto.StopMode, timeout time.Duration) {
	svc := s.unit.Service
	s.stopping.Store(true)
	s.setState(proto.StateDeactivating, "stop", "")
	s.report()
	deadline := time.Now().Add(timeout)

	// 1. Cooperative stop.
	if len(svc.ExecStop) > 0 {
		env := s.reloadEnv()
		allOK := true
		for _, c := range svc.ExecStop {
			st, err := s.runSync(c, env, deadline)
			if err != nil {
				s.log.Warnf("ExecStop=%s: %v", c.Raw, err)
				allOK = false
				continue
			}
			if st.Failed() && !c.IgnoreFailure {
				s.log.Warnf("ExecStop=%s failed with %s", c.Raw, st)
				allOK = false
			}
		}
		if allOK && s.handle.IsEmpty() {
			s.finishStop(mode, proto.ResultSuccess)
			return
		}
	}

	s.terminateTree(deadline, svc.SendSIGKILL)
	s.finishStop(mode, proto.ResultSuccess)
}

// finishStop runs ExecStopPost=, settles the final state and lets the
// supervisor exit.
func (s *Supervisor) finishStop(mode proto.StopMode, result string) {
	s.runStopPost()
	s.setMainPID(0)
	if result == proto.ResultSuccess {
		s.setState(proto.StateInactive, "dead", result)
	} else {
		s.setState(proto.StateFailed, "failed", result)
		s.exitCode = 1
	}
	s.report()
}

// terminateTree signals every member of the unit's tree until it is empty or
// the deadline passes, escalating to the final kill signal.
func (s *Supervisor) terminateTree(deadline time.Time, sendKill bool) {
	svc := s.unit.Service
	if svc.KillMode == unitfile.KillNone {
		return
	}

	killSig := syscall.Signal(unitfile.SignalNumber(svc.KillSignal))
	if killSig == 0 {
		killSig = syscall.SIGTERM
	}
	finalSig := syscall.Signal(unitfile.SignalNumber(svc.FinalKillSignal))
	if finalSig == 0 {
		finalSig = syscall.SIGKILL
	}

	s.setSubState("stop-sigterm")

	signalled := map[int]bool{}
	signalOne := func(ref proctree.ProcRef) {
		_ = proctree.Signal(ref, killSig)
		// A SIGSTOPped process would otherwise queue SIGTERM without acting on
		// it; SIGCONT lets it run far enough to die.
		_ = proctree.Signal(ref, syscall.SIGCONT)
		if svc.SendSIGHUP {
			_ = proctree.Signal(ref, syscall.SIGHUP)
		}
		signalled[ref.PID] = true
	}

	// 2. Signal the unit.
	targets := s.currentTargets()
	for _, t := range targets {
		signalOne(t)
	}

	// 3. Cheap supplement: also signal every distinct process group in the
	//    tree. Measured to be insufficient on its own (03 §4.3) — a
	//    daemonising process calls setsid() into its own group — but free.
	if svc.KillMode != unitfile.KillProcess {
		for _, g := range proctree.DistinctPGIDs(targets) {
			_ = proctree.SignalGroup(g, killSig)
		}
	}

	// 4. Wait, re-enumerating each round so that processes forked during the
	//    ladder are signalled too.
	for time.Now().Before(deadline) {
		time.Sleep(ladderInterval)
		live, err := s.handle.Members()
		if err != nil || len(live) == 0 {
			if len(s.adopted) == 0 || s.adoptedGone() {
				return
			}
		}
		for _, ref := range live {
			if !signalled[ref.PID] {
				signalOne(ref)
			}
		}
		for _, pid := range s.adopted {
			if ref, ok := proctree.Lookup(pid); ok && !signalled[pid] {
				signalOne(ref)
			}
		}
	}

	// 5. Escalate. SIGKILL cannot be blocked, and each round strictly reduces
	//    the set of signalled-and-still-alive processes, so this converges even
	//    against a fork bomb.
	if !sendKill {
		return
	}
	s.setSubState("stop-sigkill")
	var live []proctree.ProcRef
	for round := 0; round < finalKillRounds; round++ {
		var err error
		live, err = s.handle.Members()
		if err != nil {
			return
		}
		live = append(live, s.adoptedRefs()...)
		if len(live) == 0 {
			return
		}
		for _, ref := range live {
			_ = proctree.Signal(ref, finalSig)
		}
		for _, g := range proctree.DistinctPGIDs(live) {
			_ = proctree.SignalGroup(g, finalSig)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if live, err := s.handle.Members(); err == nil && len(live) > 0 {
		// Only reachable for uninterruptible-sleep processes; log the pids and
		// comms rather than hanging.
		for _, ref := range live {
			s.log.Errorf("process %d (%s) remains after %s", ref.PID, ref.Comm, finalSig)
		}
		s.setState(proto.StateFailed, "failed", proto.ResultTimeout)
		s.exitCode = 1
	}
}

// currentTargets returns the processes the ladder should signal, honouring
// KillMode=.
func (s *Supervisor) currentTargets() []proctree.ProcRef {
	if s.unit.Service.KillMode == unitfile.KillProcess {
		pid := s.MainPID()
		if pid <= 0 {
			return nil
		}
		if ref, ok := proctree.Lookup(pid); ok {
			return []proctree.ProcRef{ref}
		}
		return nil
	}
	members, err := s.handle.Members()
	if err != nil {
		return nil
	}
	if s.unit.Service.KillMode == unitfile.KillMixed {
		// Mixed: the main process gets the kill signal, the rest only the
		// final one. Signalling the whole set here and letting the escalation
		// handle the rest matches systemd closely enough and is simpler than a
		// second target list.
		if pid := s.MainPID(); pid > 0 {
			if ref, ok := proctree.Lookup(pid); ok {
				return []proctree.ProcRef{ref}
			}
		}
	}
	return append(members, s.adoptedRefs()...)
}

// adoptRecovered attaches to processes handed over by the manager after a
// supervisor died (03 §6.8). It cannot become their parent, so it watches them
// by pidfd and re-scans by environment marker while any of them is alive.
func (s *Supervisor) adoptRecovered() {
	s.adopted = append(s.adopted, s.cfg.RecoveredPIDs...)
	if len(s.adopted) == 0 {
		s.setState(proto.StateInactive, "dead", proto.ResultSuccess)
		s.report()
		s.quitOnce.Do(func() { close(s.quit) })
		return
	}
	s.log.Warnf("adopting %d orphaned process(es) from a lost supervisor", len(s.adopted))
	// The earliest-started orphan is the best available guess at the main
	// process.
	best := 0
	var bestStart uint64
	for _, pid := range s.adopted {
		if ref, ok := proctree.Lookup(pid); ok {
			if best == 0 || ref.StartTime < bestStart {
				best, bestStart = pid, ref.StartTime
			}
		}
	}
	s.setMainPID(best)
	s.setState(proto.StateActive, "running", proto.ResultSuccess)
	s.report()
}

// rescanAdopted refreshes the adopted set from the environment marker, because
// P1 does not hold for recovered processes: their own future children can still
// escape to PID 1.
func (s *Supervisor) rescanAdopted() {
	found := proctree.FindByEnv("MANAGED_BY_INVOCATION", s.unit.InvocationID)
	self := 0
	kept := found[:0]
	for _, pid := range found {
		if pid != self {
			kept = append(kept, pid)
		}
	}
	s.adopted = kept
	if len(s.adopted) == 0 && s.isActive() {
		s.log.Infof("all recovered processes have exited")
		s.setState(proto.StateInactive, "dead", proto.ResultSuccess)
		s.report()
		s.quitOnce.Do(func() { close(s.quit) })
	}
}

func (s *Supervisor) adoptedRefs() []proctree.ProcRef {
	var out []proctree.ProcRef
	for _, pid := range s.adopted {
		if ref, ok := proctree.Lookup(pid); ok {
			out = append(out, ref)
		}
	}
	return out
}

func (s *Supervisor) adoptedGone() bool { return len(s.adoptedRefs()) == 0 }
