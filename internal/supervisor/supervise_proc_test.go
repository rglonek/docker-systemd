//go:build proc && linux

package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"docker-systemd/internal/proctree"
	"docker-systemd/internal/proto"
	"docker-systemd/internal/unitfile"
)

func TestSimpleServiceStartsAndStops(t *testing.T) {
	u := unitFor(t, "simple-test.service", nil)
	setExecStart(t, u, daemonPath(t)+" foreground")

	h := start(t, u)
	st := h.waitState(10*time.Second, proto.StateActive)
	if st.MainPID <= 0 {
		t.Fatalf("MainPID = %d; want the direct child", st.MainPID)
	}
	if len(h.tree()) != 1 {
		t.Errorf("tree = %d members; want 1", len(h.tree()))
	}
	h.stop(5 * time.Second)
	h.assertNoSurvivors()
}

// The unit's own children are inside the tree and must be killed too. v0.5.x
// signalled only the direct child, orphaning the rest (defect B2).
func TestSimpleServiceChildrenAreKilled(t *testing.T) {
	u := unitFor(t, "children-test.service", nil)
	setExecStart(t, u, daemonPath(t)+" spawn-child 3")

	h := start(t, u)
	h.waitState(10*time.Second, proto.StateActive)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(h.tree()) < 4 {
		time.Sleep(50 * time.Millisecond)
	}
	if n := len(h.tree()); n != 4 {
		t.Errorf("tree = %d members; want 4 (the daemon plus three children)", n)
	}
	h.stop(5 * time.Second)
	h.assertNoSurvivors()
}

// The classic double-fork daemonisation: the escaped grandchild reparents to
// the supervisor, not to PID 1 (property P1).
func TestForkingDoubleForkIsTracked(t *testing.T) {
	u := unitFor(t, "double-fork-test.service", func(u *unitfile.Unit) {
		u.Service.Type = unitfile.TypeForking
	})
	setExecStart(t, u, daemonPath(t)+" double-fork")

	h := start(t, u)
	st := h.waitState(15*time.Second, proto.StateActive)
	if st.MainPID <= 0 {
		t.Fatalf("MainPID = %d; the escaped daemon should have been found", st.MainPID)
	}
	tree := h.tree()
	if len(tree) == 0 {
		t.Fatal("the escaped daemon is not in the tree")
	}
	found := false
	for _, r := range tree {
		if r.PID == st.MainPID {
			found = true
		}
	}
	if !found {
		t.Errorf("MainPID %d is not a member of the tree %v", st.MainPID, treePIDs(tree))
	}
	h.stop(5 * time.Second)
	h.assertNoSurvivors()
}

// daemon(3) is the exact case the retired LD_PRELOAD shim scored zero on
// (03 §4.2). If this test fails, the design's premise is broken.
func TestForkingLibcDaemonIsTrackedAndStopped(t *testing.T) {
	path := libcPath(t)
	u := unitFor(t, "libc-daemon-test.service", func(u *unitfile.Unit) {
		u.Service.Type = unitfile.TypeForking
	})
	setExecStart(t, u, path+" libc-daemon")

	h := start(t, u)
	st := h.waitState(15*time.Second, proto.StateActive)
	if st.MainPID <= 0 {
		t.Fatalf("MainPID = %d; daemon(3)'s survivor should have been found", st.MainPID)
	}
	if len(h.tree()) == 0 {
		t.Fatal("the daemon(3) survivor is not in the tree")
	}
	h.stop(5 * time.Second)
	h.assertNoSurvivors()
}

func TestPosixSpawnIsTracked(t *testing.T) {
	path := libcPath(t)
	u := unitFor(t, "posix-spawn-test.service", func(u *unitfile.Unit) {
		u.Service.Type = unitfile.TypeForking
	})
	setExecStart(t, u, path+" posix-spawn")

	h := start(t, u)
	h.waitState(15*time.Second, proto.StateActive)
	if len(h.tree()) == 0 {
		t.Fatal("the posix_spawn child is not in the tree")
	}
	h.stop(5 * time.Second)
	h.assertNoSurvivors()
}

func TestVforkIsTracked(t *testing.T) {
	path := libcPath(t)
	u := unitFor(t, "vfork-test.service", func(u *unitfile.Unit) {
		u.Service.Type = unitfile.TypeForking
	})
	setExecStart(t, u, path+" vfork")

	h := start(t, u)
	h.waitState(15*time.Second, proto.StateActive)
	if len(h.tree()) == 0 {
		t.Fatal("the vfork child is not in the tree")
	}
	h.stop(5 * time.Second)
	h.assertNoSurvivors()
}

// A process that setsid()s into its own session is why kill(-pgid) alone is
// insufficient (03 §4.3).
func TestSetsidEscapeIsStopped(t *testing.T) {
	u := unitFor(t, "setsid-test.service", func(u *unitfile.Unit) {
		u.Service.Type = unitfile.TypeForking
	})
	setExecStart(t, u, daemonPath(t)+" setsid-escape")

	h := start(t, u)
	h.waitState(15*time.Second, proto.StateActive)
	if len(h.tree()) == 0 {
		t.Fatal("the escaped session leader is not in the tree")
	}
	h.stop(5 * time.Second)
	h.assertNoSurvivors()
}

// A process that ignores SIGTERM must still be gone within TimeoutStopSec plus
// the escalation rounds.
func TestIgnoreSigtermEscalatesToSigkill(t *testing.T) {
	u := unitFor(t, "ignore-sigterm-test.service", func(u *unitfile.Unit) {
		u.Service.TimeoutStopSec = 2 * time.Second
	})
	setExecStart(t, u, daemonPath(t)+" ignore-sigterm")

	h := start(t, u)
	h.waitState(10*time.Second, proto.StateActive)

	begin := time.Now()
	h.stop(2 * time.Second)
	elapsed := time.Since(begin)
	h.assertNoSurvivors()
	if elapsed > 6*time.Second {
		t.Errorf("stop took %s; TimeoutStopSec was 2s", elapsed)
	}
}

// The ladder re-enumerates every round, so it converges even against a process
// that forks a new child on every signal (03 §10.5).
func TestForkBombOnTermConverges(t *testing.T) {
	u := unitFor(t, "fork-bomb-test.service", func(u *unitfile.Unit) {
		u.Service.TimeoutStopSec = 2 * time.Second
	})
	setExecStart(t, u, daemonPath(t)+" fork-bomb-on-term")

	h := start(t, u)
	h.waitState(10*time.Second, proto.StateActive)

	begin := time.Now()
	h.stop(2 * time.Second)
	elapsed := time.Since(begin)
	h.assertNoSurvivors()
	if elapsed > 8*time.Second {
		t.Errorf("stop took %s; the ladder should converge well inside that", elapsed)
	}
}

// A correct pid file is accepted only after it is validated as a tree member.
func TestPIDFileCorrect(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "test.pid")
	u := unitFor(t, "pidfile-ok-test.service", func(u *unitfile.Unit) {
		u.Service.Type = unitfile.TypeForking
		u.Service.PIDFile = pidfile
	})
	setExecStart(t, u, fmt.Sprintf("%s pidfile %s correct", daemonPath(t), pidfile))

	h := start(t, u)
	st := h.waitState(15*time.Second, proto.StateActive)
	data, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("reading the pid file: %v", err)
	}
	var want int
	fmt.Sscanf(string(data), "%d", &want)
	if st.MainPID != want {
		t.Errorf("MainPID = %d; the pid file says %d", st.MainPID, want)
	}
	h.stop(5 * time.Second)
	h.assertNoSurvivors()
}

// A stale pid file must fail the unit and leave the unrelated process alive
// (defects C13/E4). Here the "unrelated process" is the test binary itself.
func TestPIDFileHostileFailsTheUnitAndSparesTheTarget(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "hostile.pid")
	victim := os.Getpid()

	u := unitFor(t, "pidfile-hostile-test.service", func(u *unitfile.Unit) {
		u.Service.Type = unitfile.TypeForking
		u.Service.PIDFile = pidfile
		u.Service.TimeoutStartSec = 3 * time.Second
		u.Service.TimeoutStopSec = 2 * time.Second
	})
	setExecStart(t, u, fmt.Sprintf("%s pidfile %s hostile %d", daemonPath(t), pidfile, victim))

	h := start(t, u)
	st := h.waitState(20*time.Second, proto.StateFailed)
	if st.State != proto.StateFailed {
		t.Fatalf("state = %q; a pid file naming a process outside the unit must fail it", st.State)
	}
	// The victim — this test process — must be untouched.
	if !proctree.Alive(victim) {
		t.Fatal("the unrelated process named by the pid file was signalled")
	}
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		t.Error("the supervisor did not exit after failing")
	}
	h.assertNoSurvivors()
}

func TestPIDFileMalformedFailsTheUnit(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "bad.pid")
	u := unitFor(t, "pidfile-bad-test.service", func(u *unitfile.Unit) {
		u.Service.Type = unitfile.TypeForking
		u.Service.PIDFile = pidfile
		u.Service.TimeoutStartSec = 3 * time.Second
		u.Service.TimeoutStopSec = 2 * time.Second
	})
	setExecStart(t, u, fmt.Sprintf("%s pidfile %s malformed", daemonPath(t), pidfile))

	h := start(t, u)
	h.waitState(20*time.Second, proto.StateFailed)
	h.assertNoSurvivors()
}

// Type=notify: MainPID comes from the sender of READY=1 or from MAINPID=,
// validated as a tree member (defect B12).
func TestNotifyServiceReportsReady(t *testing.T) {
	u := unitFor(t, "notify-test.service", func(u *unitfile.Unit) {
		u.Service.Type = unitfile.TypeNotify
		u.Service.TimeoutStartSec = 10 * time.Second
	})
	setExecStart(t, u, daemonPath(t)+" notify 200ms")

	h := start(t, u)
	st := h.waitState(15*time.Second, proto.StateActive)
	if st.MainPID <= 0 {
		t.Fatalf("MainPID = %d", st.MainPID)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.last().StatusText == "accepting connections" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if h.last().StatusText != "accepting connections" {
		t.Errorf("STATUS= was not recorded; got %q", h.last().StatusText)
	}
	h.stop(5 * time.Second)
	h.assertNoSurvivors()
}

// A Type=notify unit that never reports readiness must fail at
// TimeoutStartSec rather than hanging.
func TestNotifyStartTimeout(t *testing.T) {
	u := unitFor(t, "notify-timeout-test.service", func(u *unitfile.Unit) {
		u.Service.Type = unitfile.TypeNotify
		u.Service.TimeoutStartSec = 1500 * time.Millisecond
		u.Service.TimeoutStopSec = 2 * time.Second
	})
	// `foreground` never sends READY=1.
	setExecStart(t, u, daemonPath(t)+" foreground")

	h := start(t, u)
	begin := time.Now()
	h.waitState(15*time.Second, proto.StateFailed)
	if elapsed := time.Since(begin); elapsed > 8*time.Second {
		t.Errorf("the start timeout fired after %s; TimeoutStartSec was 1.5s", elapsed)
	}
	h.assertNoSurvivors()
}

// Type=oneshot runs its ExecStart lines sequentially and goes inactive when
// they finish — it no longer force-sets RemainAfterExit (defect B20).
func TestOneshotGoesInactive(t *testing.T) {
	u := unitFor(t, "oneshot-test.service", func(u *unitfile.Unit) {
		u.Service.Type = unitfile.TypeOneshot
	})
	first, _ := unitfile.ParseCommand(daemonPath(t) + " echo one")
	second, _ := unitfile.ParseCommand(daemonPath(t) + " echo two")
	u.Service.ExecStart = []unitfile.Command{first, second}

	h := start(t, u)
	h.waitState(10*time.Second, proto.StateInactive)
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Error("the supervisor should exit once a oneshot unit finishes")
	}
}

func TestOneshotRemainAfterExit(t *testing.T) {
	u := unitFor(t, "oneshot-remain-test.service", func(u *unitfile.Unit) {
		u.Service.Type = unitfile.TypeOneshot
		u.Service.RemainAfterExit = true
	})
	cmd, _ := unitfile.ParseCommand(daemonPath(t) + " echo done")
	u.Service.ExecStart = []unitfile.Command{cmd}

	h := start(t, u)
	st := h.waitState(10*time.Second, proto.StateActive)
	if st.SubState != "exited" {
		t.Errorf("SubState = %q; want exited", st.SubState)
	}
	h.stop(5 * time.Second)
}

// A oneshot line failing without the `-` prefix fails the unit and skips the
// rest.
func TestOneshotFailureStopsTheSequence(t *testing.T) {
	u := unitFor(t, "oneshot-fail-test.service", func(u *unitfile.Unit) {
		u.Service.Type = unitfile.TypeOneshot
	})
	bad, _ := unitfile.ParseCommand(daemonPath(t) + " exit 3")
	after, _ := unitfile.ParseCommand(daemonPath(t) + " echo should-not-run")
	u.Service.ExecStart = []unitfile.Command{bad, after}

	h := start(t, u)
	h.waitState(10*time.Second, proto.StateFailed)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.logs {
		if l.Message == "should-not-run" {
			t.Error("the sequence continued past a failing ExecStart line")
		}
	}
}

// The `-` prefix means "ignore a non-zero exit status".
func TestIgnoreFailurePrefix(t *testing.T) {
	u := unitFor(t, "ignore-fail-test.service", func(u *unitfile.Unit) {
		u.Service.Type = unitfile.TypeOneshot
		u.Service.RemainAfterExit = true
	})
	bad, _ := unitfile.ParseCommand("-" + daemonPath(t) + " exit 3")
	u.Service.ExecStart = []unitfile.Command{bad}

	h := start(t, u)
	h.waitState(10*time.Second, proto.StateActive)
	h.stop(5 * time.Second)
}

// A hanging ExecStartPre must fail its own unit at TimeoutStartSec rather than
// hanging the boot (defect C11).
func TestExecStartPreTimeout(t *testing.T) {
	u := unitFor(t, "startpre-timeout-test.service", func(u *unitfile.Unit) {
		u.Service.TimeoutStartSec = 1500 * time.Millisecond
		u.Service.TimeoutStopSec = 2 * time.Second
	})
	pre, _ := unitfile.ParseCommand(daemonPath(t) + " foreground")
	u.Service.ExecStartPre = []unitfile.Command{pre}
	setExecStart(t, u, daemonPath(t)+" foreground")

	h := start(t, u)
	begin := time.Now()
	h.waitState(15*time.Second, proto.StateFailed)
	if elapsed := time.Since(begin); elapsed > 8*time.Second {
		t.Errorf("ExecStartPre= hung for %s; TimeoutStartSec was 1.5s", elapsed)
	}
	h.assertNoSurvivors()
}

// A missing executable must report 203/EXEC rather than a shell error.
func TestMissingExecutableFailsWith203(t *testing.T) {
	u := unitFor(t, "noexec-test.service", nil)
	setExecStart(t, u, "/nonexistent/binary --flag")

	h := start(t, u)
	st := h.waitState(10*time.Second, proto.StateFailed)
	if st.StatusText == "" {
		t.Error("the failure should carry a diagnostic naming the missing path")
	}
}

// Defect A1: `ExecStart=-/nonexistent/binary` must not crash the supervisor.
func TestIgnoredMissingExecutableDoesNotCrash(t *testing.T) {
	u := unitFor(t, "noexec-ignore-test.service", func(u *unitfile.Unit) {
		u.Service.Type = unitfile.TypeOneshot
		u.Service.RemainAfterExit = true
	})
	bad, _ := unitfile.ParseCommand("-/nonexistent/binary")
	good, _ := unitfile.ParseCommand(daemonPath(t) + " echo survived")
	u.Service.ExecStart = []unitfile.Command{bad, good}

	h := start(t, u)
	h.waitState(10*time.Second, proto.StateActive, proto.StateFailed)
	// Whatever the outcome, the supervisor must have stayed alive long enough
	// to report it rather than dying on a nil dereference.
	if h.last().State == "" {
		t.Error("the supervisor produced no state report at all")
	}
	h.stop(5 * time.Second)
}

// A crash-looping unit must trip StartLimitBurst rather than restarting for
// ever (defect C12).
func TestStartLimitBurstTrips(t *testing.T) {
	u := unitFor(t, "crashloop-test.service", func(u *unitfile.Unit) {
		u.Service.Restart = unitfile.RestartAlways
		u.Service.RestartSec = 50 * time.Millisecond
		u.Service.TimeoutStopSec = 2 * time.Second
		u.Unit.StartLimitBurst = 3
		u.Unit.StartLimitInterval = 30 * time.Second
	})
	setExecStart(t, u, daemonPath(t)+" crash-loop")

	h := start(t, u)
	st := h.waitState(30*time.Second, proto.StateFailed)
	if st.Result != proto.ResultStartLimitHit {
		t.Errorf("Result = %q; want start-limit-hit", st.Result)
	}
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		t.Error("the supervisor should exit once the start limit is hit")
	}
}

// Restart=on-failure brings the unit back after a crash.
func TestRestartOnFailure(t *testing.T) {
	u := unitFor(t, "restart-test.service", func(u *unitfile.Unit) {
		u.Service.Restart = unitfile.RestartOnFailure
		u.Service.RestartSec = 100 * time.Millisecond
		u.Unit.StartLimitBurst = 0
	})
	setExecStart(t, u, daemonPath(t)+" foreground")

	h := start(t, u)
	st := h.waitState(10*time.Second, proto.StateActive)
	first := st.MainPID
	if first <= 0 {
		t.Fatal("no MainPID")
	}
	// Kill the main process; the supervisor should bring it back.
	if ref, ok := proctree.Lookup(first); ok {
		_ = proctree.Signal(ref, 9)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		cur := h.last()
		if cur.State == proto.StateActive && cur.MainPID != 0 && cur.MainPID != first {
			h.stop(5 * time.Second)
			h.assertNoSurvivors()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the unit was not restarted; last state %q main=%d", h.last().State, h.last().MainPID)
}

// A unit whose Condition*= is not met is skipped, not failed.
func TestUnmetConditionSkipsTheUnit(t *testing.T) {
	u := unitFor(t, "condition-test.service", func(u *unitfile.Unit) {
		u.Unit.Conditions = []unitfile.Condition{
			{Kind: "PathExists", Value: "/definitely/not/here"},
		}
	})
	setExecStart(t, u, daemonPath(t)+" foreground")

	h := start(t, u)
	st := h.waitState(10*time.Second, proto.StateInactive)
	if st.State != proto.StateInactive {
		t.Errorf("state = %q; an unmet Condition*= skips the unit rather than failing it", st.State)
	}
	if st.ConditionOK {
		t.Error("ConditionResult should be false")
	}
}

// An unmet Assert*= fails the unit.
func TestUnmetAssertFailsTheUnit(t *testing.T) {
	u := unitFor(t, "assert-test.service", func(u *unitfile.Unit) {
		u.Unit.Conditions = []unitfile.Condition{
			{Kind: "PathExists", Value: "/definitely/not/here", Assert: true},
		}
	})
	setExecStart(t, u, daemonPath(t)+" foreground")

	h := start(t, u)
	h.waitState(10*time.Second, proto.StateFailed)
}

// The supervisor is a subreaper, so every process the unit abandons is
// eventually reported to it by wait(2) and reaped (property P2).
//
// While the unit's own process is alive its unreaped children are zombies
// parented to it, and nobody else can collect them — that is the kernel's
// rule, not a defect. What must hold is that once the unit stops, nothing from
// its tree is left in /proc, zombie or otherwise.
func TestZombiesAreReapedWhenTheUnitStops(t *testing.T) {
	u := unitFor(t, "zombie-test.service", nil)
	setExecStart(t, u, daemonPath(t)+" zombie-maker")

	h := start(t, u)
	h.waitState(10*time.Second, proto.StateActive)

	deadline := time.Now().Add(5 * time.Second)
	var before []int
	for time.Now().Before(deadline) {
		before = treePIDs(h.tree())
		if len(before) >= 6 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(before) < 2 {
		t.Fatalf("expected the daemon plus its abandoned children in the tree, got %v", before)
	}

	h.stop(5 * time.Second)

	settle := time.Now().Add(3 * time.Second)
	for time.Now().Before(settle) {
		if allGone(before) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, pid := range before {
		if _, ok := proctree.Lookup(pid); ok {
			t.Errorf("pid %d is still present after the unit stopped (zombie=%v)",
				pid, proctree.IsZombie(pid))
		}
	}
}

func allGone(pids []int) bool {
	for _, pid := range pids {
		if _, ok := proctree.Lookup(pid); ok {
			return false
		}
	}
	return true
}

// Unit output is captured with the stream's priority: stdout as info, stderr
// as err, which is what makes `journalctl -p err` possible.
func TestOutputIsCapturedWithPriority(t *testing.T) {
	u := unitFor(t, "logging-test.service", func(u *unitfile.Unit) {
		u.Service.Type = unitfile.TypeOneshot
		u.Service.RemainAfterExit = true
	})
	cmd, _ := unitfile.ParseCommand(daemonPath(t) + " echo hello world")
	u.Service.ExecStart = []unitfile.Command{cmd}

	h := start(t, u)
	h.waitState(10*time.Second, proto.StateActive)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		n := len(h.logs)
		h.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	found := false
	for _, l := range h.logs {
		if l.Message == "hello world" {
			found = true
			if l.Priority != 6 {
				t.Errorf("stdout priority = %d; want 6 (info)", l.Priority)
			}
			if l.Unit != u.Name {
				t.Errorf("record unit = %q; want %q", l.Unit, u.Name)
			}
		}
	}
	if !found {
		t.Errorf("the unit's stdout was not forwarded; got %d records", len(h.logs))
	}
	h.stop(5 * time.Second)
}

// A unit whose User= cannot be resolved fails with 217/USER rather than
// falling back to root.
func TestUnresolvableUserFailsTheUnit(t *testing.T) {
	u := unitFor(t, "baduser-test.service", func(u *unitfile.Unit) {
		u.Service.User = "definitely-no-such-user"
	})
	setExecStart(t, u, daemonPath(t)+" foreground")

	h := start(t, u)
	st := h.waitState(10*time.Second, proto.StateFailed)
	if st.StatusText == "" {
		t.Error("the failure should name the unresolvable user")
	}
}

// KillMode=none leaves the processes running; the ladder must honour it.
func TestKillModeNoneLeavesProcesses(t *testing.T) {
	u := unitFor(t, "killmode-none-test.service", func(u *unitfile.Unit) {
		u.Service.KillMode = unitfile.KillNone
		u.Service.TimeoutStopSec = time.Second
	})
	setExecStart(t, u, daemonPath(t)+" foreground")

	h := start(t, u)
	st := h.waitState(10*time.Second, proto.StateActive)
	pid := st.MainPID
	h.stop(3 * time.Second)
	if !proctree.Alive(pid) {
		t.Error("KillMode=none must not signal the unit's processes")
	}
	// Clean up what the ladder deliberately left behind.
	if ref, ok := proctree.Lookup(pid); ok {
		_ = proctree.Signal(ref, 9)
	}
}

func treePIDs(refs []proctree.ProcRef) []int {
	out := make([]int, len(refs))
	for i, r := range refs {
		out[i] = r.PID
	}
	return out
}
