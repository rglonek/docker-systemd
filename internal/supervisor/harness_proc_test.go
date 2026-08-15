//go:build proc && linux

// Package-level harness for the L2 process tests
// (designs/docs/next/11-testing.md §3). These need a real Linux kernel — the
// whole point is to assert behaviour against real daemonising processes rather
// than against a mock — so they are behind the `proc` build tag.
package supervisor

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"docker-systemd/internal/backend"
	"docker-systemd/internal/paths"
	"docker-systemd/internal/proctree"
	"docker-systemd/internal/proto"
	"docker-systemd/internal/unitfile"
)

var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
	// haveCC records whether the C helper could be built; the daemon(3),
	// posix_spawn and vfork cases are skipped when it could not.
	haveCC bool
)

// binaries builds the manager binary, the Go test daemon, and (when a C
// toolchain is present) the libc helper.
func binaries(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "docker-systemd-l2-")
		if err != nil {
			buildErr = err
			return
		}
		binDir = dir
		root, err := repoRoot()
		if err != nil {
			buildErr = err
			return
		}
		build := exec.Command("go", "build", "-o", filepath.Join(dir, "docker-systemd"),
			"./cmd/docker-systemd")
		build.Dir = root
		if out, err := build.CombinedOutput(); err != nil {
			buildErr = errors.New("building docker-systemd: " + string(out))
			return
		}
		build = exec.Command("go", "build", "-o", filepath.Join(dir, "testdaemon"),
			"./test/daemons/testdaemon.go")
		build.Dir = root
		if out, err := build.CombinedOutput(); err != nil {
			buildErr = errors.New("building testdaemon: " + string(out))
			return
		}
		// The C helper lives under an underscore-prefixed directory so that
		// `go build ./...` does not try to treat it as a package.
		cc := exec.Command("cc", "-O1", "-o", filepath.Join(dir, "libcdaemon"),
			filepath.Join(root, "test/daemons/_libc/libcdaemon.c"))
		if err := cc.Run(); err == nil {
			haveCC = true
		}
	})
	if buildErr != nil {
		t.Fatalf("harness build failed: %v", buildErr)
	}
	return binDir
}

func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := wd; dir != "/"; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
	}
	return "", errors.New("go.mod not found above the working directory")
}

// harness drives one supervisor process the way the manager does.
type harness struct {
	t    *testing.T
	conn net.Conn
	proc *os.Process
	unit *unitfile.Unit

	mu     sync.Mutex
	states []proto.StateReport
	logs   []proto.LogRecord
	done   chan struct{}
	exit   int
}

// start spawns `<bin> --supervise` with a socketpair on fd 3 and sends CONFIG.
func start(t *testing.T, u *unitfile.Unit) *harness {
	t.Helper()
	dir := binaries(t)

	if err := os.MkdirAll(paths.NotifyDir, paths.ModeNotifyDir); err != nil {
		t.Skipf("cannot create %s (need root): %v", paths.NotifyDir, err)
	}

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parent := os.NewFile(uintptr(fds[0]), "ctl")
	child := os.NewFile(uintptr(fds[1]), "ctl-child")
	conn, err := net.FileConn(parent)
	parent.Close()
	if err != nil {
		t.Fatalf("FileConn: %v", err)
	}

	proc, err := os.StartProcess(filepath.Join(dir, "docker-systemd"),
		[]string{"supervisor", "--supervise", u.Name},
		&os.ProcAttr{
			Files: []*os.File{nil, nil, os.Stderr, child},
			Env:   os.Environ(),
		})
	child.Close()
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	u.InvocationID = unitfile.NewInvocationID()
	h := &harness{t: t, conn: conn, proc: proc, unit: u, done: make(chan struct{})}

	cfg := Config{
		Unit:        u,
		Environment: os.Environ(),
		Backend:     backend.Subreaper,
		HeartbeatMS: 200,
		Mode:        "start",
	}
	if u.Service.Type == unitfile.TypeNotify || u.Service.Type == unitfile.TypeNotifyReload {
		cfg.NotifySocket = filepath.Join(paths.NotifyDir, u.Name+".sock")
	}
	if err := proto.WriteJSON(conn, proto.TypeConfig, cfg); err != nil {
		t.Fatalf("sending CONFIG: %v", err)
	}

	go h.readLoop()
	go func() {
		st, _ := proc.Wait()
		if st != nil {
			h.exit = st.ExitCode()
		}
		close(h.done)
	}()
	t.Cleanup(h.cleanup)
	return h
}

func (h *harness) readLoop() {
	for {
		f, err := proto.Read(h.conn)
		if err != nil {
			return
		}
		switch f.Type {
		case proto.TypeState:
			var r proto.StateReport
			if json.Unmarshal(f.Payload, &r) == nil {
				h.mu.Lock()
				h.states = append(h.states, r)
				h.mu.Unlock()
			}
		case proto.TypeLogRecord:
			var r proto.LogRecord
			if json.Unmarshal(f.Payload, &r) == nil {
				h.mu.Lock()
				h.logs = append(h.logs, r)
				h.mu.Unlock()
			}
		}
	}
}

// last returns the most recent state report.
func (h *harness) last() proto.StateReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.states) == 0 {
		return proto.StateReport{}
	}
	return h.states[len(h.states)-1]
}

// waitState blocks until the unit reaches one of the given states.
func (h *harness) waitState(timeout time.Duration, want ...string) proto.StateReport {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st := h.last()
		for _, w := range want {
			if st.State == w {
				return st
			}
		}
		select {
		case <-h.done:
			// The supervisor exited; one more look at what it reported.
			st = h.last()
			for _, w := range want {
				if st.State == w {
					return st
				}
			}
			h.t.Fatalf("supervisor exited (code %d) in state %q while waiting for %v",
				h.exit, st.State, want)
		case <-time.After(20 * time.Millisecond):
		}
	}
	h.t.Fatalf("timed out after %s waiting for %v; last state was %q/%q",
		timeout, want, h.last().State, h.last().SubState)
	return proto.StateReport{}
}

// tree returns the live members of the unit as seen from outside.
func (h *harness) tree() []proctree.ProcRef {
	refs, err := proctree.Enumerate(h.proc.Pid)
	if err != nil {
		h.t.Fatalf("Enumerate: %v", err)
	}
	return refs
}

// stop sends STOP and waits for the supervisor to exit, which is the
// authoritative "unit fully stopped" event (invariant I4).
func (h *harness) stop(timeout time.Duration) {
	h.t.Helper()
	_ = proto.WriteJSON(h.conn, proto.TypeStop,
		proto.StopRequest{Mode: proto.StopNormal, TimeoutMS: timeout.Milliseconds()})
	select {
	case <-h.done:
	case <-time.After(timeout + 10*time.Second):
		h.t.Fatalf("the supervisor did not exit within %s of the stop request", timeout)
	}
}

// assertNoSurvivors checks that the ladder left nothing behind.
func (h *harness) assertNoSurvivors() {
	h.t.Helper()
	// Give the kernel a moment to reap after the supervisor exited.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.tree()) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	var left []int
	for _, r := range h.tree() {
		left = append(left, r.PID)
	}
	h.t.Errorf("processes survived the stop: %v", left)
}

func (h *harness) cleanup() {
	select {
	case <-h.done:
	default:
		_ = h.proc.Signal(syscall.SIGKILL)
	}
	// Sweep anything the test left behind so one failure cannot leak into the
	// next.
	for _, r := range h.tree() {
		_ = proctree.Signal(r, syscall.SIGKILL)
	}
	h.conn.Close()
}

// unitFor builds a minimal .service unit around one ExecStart command.
func unitFor(t *testing.T, name string, mutate func(*unitfile.Unit)) *unitfile.Unit {
	t.Helper()
	u := unitfile.NewUnit(name)
	u.LoadState = unitfile.LoadLoaded
	u.Service.TimeoutStartSec = 10 * time.Second
	u.Service.TimeoutStopSec = 5 * time.Second
	if mutate != nil {
		mutate(u)
	}
	return u
}

// exec sets ExecStart from a raw command line.
func setExecStart(t *testing.T, u *unitfile.Unit, line string) {
	t.Helper()
	cmd, err := unitfile.ParseCommand(line)
	if err != nil {
		t.Fatalf("ParseCommand(%q): %v", line, err)
	}
	u.Service.ExecStart = []unitfile.Command{cmd}
}

// daemonPath returns the path of the Go test daemon.
func daemonPath(t *testing.T) string { return filepath.Join(binaries(t), "testdaemon") }

// libcPath returns the path of the C helper, skipping the test if it could not
// be built.
func libcPath(t *testing.T) string {
	t.Helper()
	dir := binaries(t)
	if !haveCC {
		t.Skip("no C toolchain: cannot exercise daemon(3), posix_spawn or vfork")
	}
	return filepath.Join(dir, "libcdaemon")
}
