package manager

import (
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"docker-systemd/internal/graph"
	"docker-systemd/internal/paths"
	"docker-systemd/internal/proctree"
	"docker-systemd/internal/proto"
	"golang.org/x/sys/unix"
)

// shutdownRequest describes why the manager is stopping.
type shutdownRequest struct {
	Reason   string
	ExitCode int
	Reboot   bool
}

// RequestShutdown asks the manager to shut down. It is idempotent: a second
// request is ignored.
func (m *Manager) RequestShutdown(req shutdownRequest) {
	m.shutdownOnce.Do(func() {
		m.shuttingDown.Store(true)
		m.shutdownCh <- req
	})
}

// installSignalHandlers wires the signals of 04 §8 step 9.
func (m *Manager) installSignalHandlers() {
	ch := make(chan os.Signal, 8)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT,
		syscall.SIGHUP, syscall.SIGUSR1)
	go func() {
		for sig := range ch {
			switch sig {
			case syscall.SIGHUP:
				m.DaemonReload()
			case syscall.SIGUSR1:
				m.dumpState()
			default:
				n := m.signalCount.Add(1)
				switch {
				case n == 1:
					m.log.Infof("received %s, shutting down", sig)
					m.RequestShutdown(shutdownRequest{Reason: sig.String()})
				case n == 2:
					m.log.Warnf("received %s again; shutdown is already in progress", sig)
				default:
					// The operator's escape hatch: a third signal forces an
					// immediate kill of everything.
					m.log.Errorf("received %s a third time; killing everything immediately", sig)
					m.killEverything(syscall.SIGKILL)
					os.Exit(1)
				}
			}
		}
	}()
}

// setSubreaper marks the manager as a subreaper. As PID 1 it already is one;
// this matters when the binary is run as a non-init process.
func (m *Manager) setSubreaper() {
	if err := proctree.SetSubreaper(); err != nil {
		m.log.Warnf("PR_SET_CHILD_SUBREAPER: %v", err)
	}
}

// startReaper reaps orphans that land on PID 1 — the `docker exec` that left a
// background process behind, and anything a dead supervisor's tree reparented
// to us.
//
// Supervisor exits are reaped by their own runner goroutine's Wait, so this
// loop must not consume them: it uses a dedicated waiter set rather than a
// blanket wait4(-1) that would race with os.Process.Wait.
func (m *Manager) startReaper() {
	go func() {
		ch := make(chan os.Signal, 16)
		signal.Notify(ch, syscall.SIGCHLD)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ch:
			case <-tick.C:
			}
			m.reapStrays()
		}
	}()
}

// reapStrays reaps zombies whose parent is us but which no runner owns.
//
// os.Process.Wait already reaps our own children, so a blanket wait4(-1) here
// would steal supervisor exit statuses. Instead each zombie is checked against
// the runner set and only unowned ones are waited for.
func (m *Manager) reapStrays() {
	refs, err := proctree.Children(os.Getpid())
	if err != nil {
		return
	}
	owned := map[int]bool{}
	m.runMu.Lock()
	for _, r := range m.runners {
		owned[r.proc.Pid] = true
	}
	m.runMu.Unlock()
	for _, ref := range refs {
		if owned[ref.PID] || !proctree.IsZombie(ref.PID) {
			continue
		}
		var ws unix.WaitStatus
		if _, err := unix.Wait4(ref.PID, &ws, unix.WNOHANG, nil); err == nil {
			m.log.Debugf("reaped stray process %d (%s)", ref.PID, ref.Comm)
		}
	}
}

// dumpState logs a state summary in response to SIGUSR1.
func (m *Manager) dumpState() {
	m.log.Infof("--- state dump ---")
	for _, st := range m.ListUnits(nil, nil, true) {
		m.log.Infof("%s load=%s active=%s(%s) main=%d tasks=%d",
			st.Name, st.LoadState, st.ActiveState, st.SubState, st.MainPID, st.Tasks)
	}
	m.log.Infof("--- end state dump ---")
}

// runShutdown performs the ordered, parallel, deadline-bounded teardown of
// 04 §9 and returns the process exit code.
//
// v0.5.x stopped units serially in map order with 5 s each and no global
// budget, so three slow units blew Docker's default 10 s grace and the
// container was SIGKILLed mid-shutdown (defect C14).
func (m *Manager) runShutdown(req shutdownRequest) int {
	m.log.Infof("shutting down (%s)", req.Reason)
	begin := time.Now()
	budget := m.opts.ShutdownTimeout
	deadline := begin.Add(budget)

	active := m.activeUnits()
	failed := false
	if len(active) > 0 {
		t := graph.BuildStop(m, active, active)
		strata := t.Strata()
		remaining := len(strata)
		for i := len(strata) - 1; i >= 0; i-- {
			if remaining <= 0 {
				remaining = 1
			}
			share := time.Until(deadline) / time.Duration(remaining)
			if share <= 0 {
				share = time.Second
			}
			m.stopStratum(strata[i], share, &failed)
			remaining--
		}
	}

	unitsStopped := time.Since(begin)
	m.log.Infof("all units stopped in %s; sweeping the container",
		unitsStopped.Round(time.Millisecond))

	// After the supervisors are gone, sweep whatever is left in the container.
	m.killEverything(syscall.SIGTERM)
	time.Sleep(2 * time.Second)
	m.killEverything(syscall.SIGKILL)
	m.finalReap(5 * time.Second)

	m.log.Infof("Shutdown finished in %s", time.Since(begin).Round(time.Millisecond))
	m.broker.Close()
	_ = os.Remove(paths.ControlSocket)
	if req.Reboot {
		m.log.Infof("reboot requested; the container will stop. " +
			"Use `docker run --restart=always` for the closest analogue.")
	}
	if req.ExitCode != 0 {
		return req.ExitCode
	}
	if failed {
		return 1
	}
	return 0
}

// stopStratum stops a whole stratum in parallel, each unit bounded by its own
// timeout and by the stratum's share of the global budget.
func (m *Manager) stopStratum(units []string, share time.Duration, failed *bool) {
	done := make(chan string, len(units))
	for _, name := range units {
		go func(name string) {
			timeout := m.stopTimeout(name)
			if timeout > share {
				timeout = share
			}
			if err := m.stopUnit(name, proto.StopShutdown, timeout); err != nil {
				m.log.Warnf("%s: %v", name, err)
				done <- name
				return
			}
			done <- ""
		}(name)
	}
	deadline := time.After(share + 5*time.Second)
	for range units {
		select {
		case bad := <-done:
			if bad != "" {
				*failed = true
			}
		case <-deadline:
			*failed = true
			m.log.Warnf("stratum stop budget exhausted; continuing")
			return
		}
	}
}

// killEverything signals every process in the container except this one.
func (m *Manager) killEverything(sig syscall.Signal) {
	refs, err := proctree.All()
	if err != nil {
		return
	}
	self := os.Getpid()
	for _, ref := range refs {
		if ref.PID == self || ref.PID <= 1 {
			continue
		}
		_ = proctree.Signal(ref, sig)
	}
}

// finalReap collects zombies until none are left or the budget expires.
func (m *Manager) finalReap(budget time.Duration) {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		if err != nil {
			return
		}
		if pid <= 0 {
			refs, err := proctree.All()
			if err != nil || len(refs) <= 1 {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func dedupeSorted(in []string) []string {
	sort.Strings(in)
	return dedupe(in)
}
