//go:build linux

package backend

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"docker-systemd/internal/proctree"
)

// The empirical results of designs/docs/next/03-process-model.md §4 are a
// permanent, runnable probe suite rather than a one-off investigation: kernel
// and runtime behaviour changes, and the whole design rests on these facts
// (11 §6).

// probe-subreaper: prctl(PR_SET_CHILD_SUBREAPER) succeeds and an orphan
// reparents to us rather than to PID 1.
func TestProbeSubreaper(t *testing.T) {
	ok, err := probeSubreaper()
	if !ok {
		t.Fatalf("PR_SET_CHILD_SUBREAPER is unavailable (%v); the design's primary "+
			"mechanism does not work on this kernel", err)
	}

	// Fork a child that itself forks and exits, so the grandchild is orphaned.
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no /bin/sh to build an orphan with")
	}
	cmd := exec.Command(sh, "-c", "sleep 5 & exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the orphan generator: %v", err)
	}
	_ = cmd.Wait()

	self := os.Getpid()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		refs, err := proctree.Children(self)
		if err != nil {
			t.Fatalf("Children: %v", err)
		}
		for _, r := range refs {
			if r.Comm == "sleep" {
				// The orphan reparented to us, not to PID 1 — property P1.
				_ = proctree.Signal(r, syscall.SIGKILL)
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("the orphaned grandchild did not reparent to this subreaper")
}

// probe-pidfd: pidfd_open on a non-child works, poll() delivers exit, and
// pidfd_send_signal works. These give race-free waiting on adopted pids.
func TestProbePidfd(t *testing.T) {
	if !proctree.PidfdSupported() {
		t.Skip("pidfd_open is unavailable; the implementation falls back to " +
			"kill(2) with start-time validation")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no /bin/sh")
	}
	cmd := exec.Command(sh, "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	fd, err := proctree.OpenPidfd(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("pidfd_open: %v", err)
	}
	defer fd.Close()

	if fd.Wait(100 * time.Millisecond) {
		t.Error("poll() reported an exit for a process that is still running")
	}
	if err := fd.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("pidfd_send_signal: %v", err)
	}
	if !fd.Wait(3 * time.Second) {
		t.Error("poll() did not deliver the exit notification")
	}
}

// probe-cgroup: report whether /sys/fs/cgroup is writable. Under plain
// `docker run` it is not, which is why cgroups can only ever be an optional
// accelerator (D6). This probe never fails the build; it records the answer.
func TestProbeCgroup(t *testing.T) {
	ok, path, err := probeCgroup2()
	t.Logf("cgroup2 writable=%v path=%q err=%v", ok, path, err)
	if ok {
		t.Log("a writable cgroup hierarchy is available; the cgroup2 backend " +
			"will be selected in preference to subreaper tracking")
	}
}

// probe-pgid-insufficient: kill(-pgid) does NOT stop a setsid'd daemon. This is
// a regression guard against anyone "simplifying" the termination ladder into a
// single process-group kill.
func TestProbeProcessGroupKillIsInsufficient(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no /bin/sh")
	}
	// The child starts in its own process group and then setsid()s a
	// grandchild into a session of its own, exactly as daemonising does.
	cmd := exec.Command(sh, "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid

	escaped := exec.Command(sh, "-c", "sleep 30")
	escaped.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := escaped.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = escaped.Process.Kill()
		_ = escaped.Wait()
	}()

	// Signalling the first process's group must not reach the setsid'd one.
	_ = proctree.SignalGroup(pgid, syscall.SIGTERM)
	time.Sleep(300 * time.Millisecond)

	if !proctree.Alive(escaped.Process.Pid) {
		t.Error("kill(-pgid) reached a process in a different session; if this " +
			"ever becomes true the termination ladder's per-pid pass could be " +
			"simplified, but it is not true today")
	}
}

// probe-proc: /proc/<pid>/stat is readable and parseable for all processes.
func TestProbeProc(t *testing.T) {
	ok, err := probeProc()
	if !ok {
		t.Fatalf("/proc is not usable (%v); the manager falls back to the "+
			"degraded backend", err)
	}
	refs, err := proctree.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(refs) == 0 {
		t.Fatal("no processes visible in /proc")
	}
}

// Select must always return a usable backend, never nil.
func TestSelectAlwaysReturnsABackend(t *testing.T) {
	for _, forced := range []Name{Auto, Cgroup2, Subreaper, Degraded, "nonsense"} {
		be, probes := Select(forced)
		if be == nil {
			t.Fatalf("Select(%q) returned nil", forced)
		}
		if len(probes) == 0 {
			t.Errorf("Select(%q) reported no probe results", forced)
		}
	}
}

func TestSubreaperHandleMembership(t *testing.T) {
	be, _ := Select(Subreaper)
	if be.Name() != Subreaper {
		t.Skipf("the subreaper backend was not selected (got %s)", be.Name())
	}
	h, err := be.Attach("probe.service", os.Getpid())
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer h.Release()

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no /bin/sh")
	}
	cmd := exec.Command(sh, "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	members, err := h.Members()
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	found := false
	for _, m := range members {
		if m.PID == cmd.Process.Pid {
			found = true
		}
	}
	if !found {
		t.Error("a direct child is not reported as a member")
	}
	if h.IsEmpty() {
		t.Error("IsEmpty reported true while a child is running")
	}
}
