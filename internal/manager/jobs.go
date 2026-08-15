package manager

import (
	"fmt"
	"runtime"
	"sync"
	"time"

	"docker-systemd/internal/graph"
	"docker-systemd/internal/proto"
	"docker-systemd/internal/unitfile"
)

// maxParallel bounds how many units start concurrently within one stratum.
func maxParallel() int {
	n := 2 * runtime.NumCPU()
	if n > 8 {
		n = 8
	}
	if n < 1 {
		n = 1
	}
	return n
}

// execute runs a transaction: strata in order, units within a stratum in
// parallel. It returns per-unit success.
//
// A unit becomes eligible only when every After= predecessor in the
// transaction has settled, which is what the strata encode. v0.5.x consulted
// neither Before= nor After= and booted in ReadDir order (defect B11).
func (m *Manager) execute(t *graph.Transaction, progress func(string)) map[string]bool {
	results := map[string]bool{}
	var mu sync.Mutex

	strata := t.Strata()
	for _, w := range t.Warnings {
		m.log.Warnf("%s", w)
	}

	for _, stratum := range strata {
		sem := make(chan struct{}, maxParallel())
		var wg sync.WaitGroup
		for _, name := range stratum {
			job, ok := t.JobFor(name)
			if !ok {
				continue
			}
			wg.Add(1)
			go func(job graph.Job) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				// A panic in one unit's handling must not take the manager
				// down with it.
				defer func() {
					if r := recover(); r != nil {
						m.log.Errorf("%s: panic while running job: %v", job.Unit, r)
						mu.Lock()
						results[job.Unit] = false
						mu.Unlock()
					}
				}()
				ok := m.runJob(job, progress)
				mu.Lock()
				results[job.Unit] = ok
				mu.Unlock()
			}(job)
		}
		wg.Wait()

		// A mandatory job that failed releases its dependents rather than
		// leaving them queued behind it: the remaining strata still run, but
		// units that Requires= a failed unit are skipped.
		mu.Lock()
		for _, name := range stratum {
			if !results[name] {
				m.skipDependents(t, name, results)
			}
		}
		mu.Unlock()
	}
	return results
}

// skipDependents marks the units that mandatorily require a failed unit.
func (m *Manager) skipDependents(t *graph.Transaction, failed string, results map[string]bool) {
	for _, j := range t.Jobs {
		if _, done := results[j.Unit]; done {
			continue
		}
		u := m.Get(j.Unit)
		if u == nil {
			continue
		}
		for _, k := range graph.Kinds {
			if !k.FailOnDepFail {
				continue
			}
			for _, dep := range graph.Deps(u, k.Name) {
				if unitfile.CanonicalName(dep) == failed {
					m.log.Warnf("%s: dependency %s failed; not starting", j.Unit, failed)
					results[j.Unit] = false
				}
			}
		}
	}
}

// runJob performs one unit's job.
func (m *Manager) runJob(job graph.Job, progress func(string)) bool {
	switch job.Type {
	case graph.JobStop:
		if progress != nil {
			progress(fmt.Sprintf("Stopping %s...", job.Unit))
		}
		if err := m.stopUnit(job.Unit, proto.StopNormal, m.stopTimeout(job.Unit)); err != nil {
			m.log.Errorf("%s: %v", job.Unit, err)
			return false
		}
		return true

	case graph.JobReload:
		if err := m.reloadUnit(job.Unit); err != nil {
			m.log.Errorf("%s: %v", job.Unit, err)
			return false
		}
		return true

	default:
		u := m.Get(job.Unit)
		if u == nil {
			return !job.Mandatory
		}
		if job.RequireActive {
			// Requisite=: fail if the dependency is not already active; never
			// start it. v0.5.x wired this into Requires= and started it
			// (defect B8).
			if m.IsActive(job.Unit) {
				return true
			}
			m.log.Errorf("Unit %s is required by %s but is not active (Requisite=)",
				job.Unit, job.Trigger)
			return false
		}
		if progress != nil {
			progress(fmt.Sprintf("Starting %s...", job.Unit))
		}
		if err := m.startUnit(u); err != nil {
			m.log.Errorf("%s: %v", job.Unit, err)
			m.runOnFailure(u)
			return !job.Mandatory && false
		}
		m.runOnSuccess(u)
		return true
	}
}

func (m *Manager) stopTimeout(name string) time.Duration {
	u := m.Get(name)
	if u == nil || u.Service == nil {
		return 90 * time.Second
	}
	return u.Service.EffectiveTimeoutStop()
}

// runOnFailure starts the units named by OnFailure=. There is deliberately no
// inverse edge: the inverse of OnFailure is not OnSuccess (defect B9).
func (m *Manager) runOnFailure(u *unitfile.Unit) {
	for _, dep := range u.Unit.OnFailure {
		m.log.Infof("%s failed; triggering %s", u.Name, dep)
		go m.StartUnits([]string{dep}, nil)
	}
	m.applyAction(u.Unit.FailureAction, u.Name, "FailureAction")
}

// runOnSuccess starts the units named by OnSuccess=.
func (m *Manager) runOnSuccess(u *unitfile.Unit) {
	if len(u.Unit.OnSuccess) == 0 && u.Unit.SuccessAction == "none" {
		return
	}
	st := m.UnitState(u.Name)
	if st.ActiveState != proto.StateInactive {
		return
	}
	for _, dep := range u.Unit.OnSuccess {
		go m.StartUnits([]string{dep}, nil)
	}
	m.applyAction(u.Unit.SuccessAction, u.Name, "SuccessAction")
}

// applyAction handles FailureAction=/SuccessAction=.
func (m *Manager) applyAction(action, unit, directive string) {
	switch action {
	case "", "none":
		return
	case "poweroff", "poweroff-force", "poweroff-immediate", "halt", "halt-force", "halt-immediate":
		m.log.Warnf("%s: %s=%s; powering off", unit, directive, action)
		m.RequestShutdown(shutdownRequest{Reason: directive, ExitCode: 0})
	case "reboot", "reboot-force", "reboot-immediate":
		m.log.Warnf("%s: %s=%s; rebooting (the container will stop)", unit, directive, action)
		m.RequestShutdown(shutdownRequest{Reason: directive, ExitCode: 0, Reboot: true})
	case "exit", "exit-force":
		m.RequestShutdown(shutdownRequest{Reason: directive, ExitCode: 1})
	}
}

// StartUnits starts units and their dependencies.
func (m *Manager) StartUnits(names []string, progress func(string)) error {
	if m.shuttingDown.Load() {
		return fmt.Errorf("Manager is shutting down")
	}
	t, err := graph.BuildStart(m, names)
	if err != nil {
		return err
	}
	res := m.execute(t, progress)
	for _, n := range names {
		if ok, present := res[unitfile.CanonicalName(n)]; present && !ok {
			return fmt.Errorf("Job for %s failed. See \"systemctl status %s\" and \"journalctl -xeu %s\" for details.",
				unitfile.CanonicalName(n), unitfile.CanonicalName(n), unitfile.CanonicalName(n))
		}
	}
	return nil
}

// StopUnits stops units and anything bound to them.
func (m *Manager) StopUnits(names []string, progress func(string)) error {
	t := graph.BuildStop(m, names, m.activeUnits())
	strata := t.Strata()
	// Stopping runs in reverse topological order: dependents before their
	// dependencies.
	var firstErr error
	for i := len(strata) - 1; i >= 0; i-- {
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, name := range strata[i] {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				if progress != nil {
					progress(fmt.Sprintf("Stopping %s...", name))
				}
				if err := m.stopUnit(name, proto.StopNormal, m.stopTimeout(name)); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
			}(name)
		}
		wg.Wait()
	}
	return firstErr
}

// RestartUnits stops then starts units.
func (m *Manager) RestartUnits(names []string, tryOnly bool, progress func(string)) error {
	var toStart []string
	for _, n := range names {
		n = unitfile.CanonicalName(n)
		active := m.IsActive(n)
		if tryOnly && !active {
			continue
		}
		if active {
			if err := m.stopUnit(n, proto.StopRestart, m.stopTimeout(n)); err != nil {
				return err
			}
		}
		toStart = append(toStart, n)
	}
	if len(toStart) == 0 {
		return nil
	}
	return m.StartUnits(toStart, progress)
}

// ReloadOrRestart reloads a unit if it declares ExecReload=, otherwise
// restarts it.
func (m *Manager) ReloadOrRestart(names []string, tryOnly bool, progress func(string)) error {
	for _, n := range names {
		n = unitfile.CanonicalName(n)
		u := m.Get(n)
		if u == nil {
			return fmt.Errorf("Unit %s not found.", n)
		}
		if tryOnly && !m.IsActive(n) {
			continue
		}
		if u.Service != nil && len(u.Service.ExecReload) > 0 && m.IsActive(n) {
			if err := m.reloadUnit(n); err != nil {
				return err
			}
			continue
		}
		if err := m.RestartUnits([]string{n}, tryOnly, progress); err != nil {
			return err
		}
	}
	return nil
}

// Isolate starts the named target and stops everything not required by it.
func (m *Manager) Isolate(name string, progress func(string)) error {
	name = unitfile.CanonicalName(name)
	u := m.Get(name)
	if u == nil {
		return fmt.Errorf("Unit %s not found.", name)
	}
	t, err := graph.BuildStart(m, []string{name})
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, j := range t.Jobs {
		keep[j.Unit] = true
	}
	var stop []string
	for _, active := range m.activeUnits() {
		if keep[active] {
			continue
		}
		if au := m.Get(active); au != nil && au.Unit.IgnoreOnIsolate {
			continue
		}
		stop = append(stop, active)
	}
	if len(stop) > 0 {
		if err := m.StopUnits(stop, progress); err != nil {
			m.log.Warnf("isolate: %v", err)
		}
	}
	return m.StartUnits([]string{name}, progress)
}

// ResetFailed clears the recorded failure state of units.
func (m *Manager) ResetFailed(names []string) {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	if len(names) == 0 {
		m.lastState = map[string]proto.StateReport{}
		return
	}
	for _, n := range names {
		delete(m.lastState, unitfile.CanonicalName(n))
	}
}
