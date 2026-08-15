// Package backend abstracts "which processes belong to this unit" behind one
// interface, so the supervisor logic is written once (03 §5).
//
// cgroup v2 is an auto-detected accelerator, never a requirement: under plain
// `docker run` the hierarchy is mounted read-only, so the subreaper backend is
// the one that must always work.
package backend

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"docker-systemd/internal/proctree"
)

// Name identifies a backend.
type Name string

// Backend names, in probe order.
const (
	Cgroup2   Name = "cgroup2"
	Subreaper Name = "subreaper"
	Degraded  Name = "degraded"
	Auto      Name = "auto"
)

// Handle is a backend's per-unit membership handle.
type Handle interface {
	// Members returns the unit's live processes.
	Members() ([]proctree.ProcRef, error)
	// IsEmpty reports whether the unit has no live processes.
	IsEmpty() bool
	// WaitEmpty blocks until the unit's process set is empty or the deadline
	// passes, reporting whether it emptied.
	WaitEmpty(deadline time.Time) bool
	// KillAll signals every member.
	KillAll(sig syscall.Signal) error
	// Release frees any resources the handle holds.
	Release()
}

// Backend creates per-unit handles.
type Backend interface {
	Name() Name
	// Attach returns a handle rooted at the supervisor process supervisorPID
	// for the named unit.
	Attach(unit string, supervisorPID int) (Handle, error)
}

// Select probes for the best available backend, or forces one.
//
// Probe order and the expected outcome under plain `docker run`:
//
//  1. cgroup-v2   only if a subdirectory can actually be created — expected
//     to FAIL, because Docker mounts /sys/fs/cgroup read-only
//  2. subreaper   prctl(PR_SET_CHILD_SUBREAPER) — expected to succeed on
//     every Linux >= 3.4, i.e. always
//  3. degraded    direct children only, with a loud warning
func Select(force Name) (Backend, map[string]string) {
	probes := map[string]string{}

	cgOK, cgPath, cgErr := probeCgroup2()
	probes["cgroup2"] = describe(cgOK, cgErr)
	srOK, srErr := probeSubreaper()
	probes["subreaper"] = describe(srOK, srErr)
	probes["pidfd"] = describe(proctree.PidfdSupported(), nil)
	procOK, procErr := probeProc()
	probes["proc"] = describe(procOK, procErr)

	switch force {
	case Cgroup2:
		if cgOK {
			return &cgroupBackend{root: cgPath}, probes
		}
	case Subreaper:
		if srOK && procOK {
			return &subreaperBackend{}, probes
		}
	case Degraded:
		return &degradedBackend{}, probes
	}
	if force != Auto && force != "" {
		probes["forced"] = string(force) + " (unavailable, falling back)"
	}

	switch {
	case cgOK:
		return &cgroupBackend{root: cgPath}, probes
	case srOK && procOK:
		return &subreaperBackend{}, probes
	default:
		return &degradedBackend{}, probes
	}
}

func describe(ok bool, err error) string {
	if ok {
		return "available"
	}
	if err != nil {
		return "unavailable: " + err.Error()
	}
	return "unavailable"
}

func probeSubreaper() (bool, error) {
	if err := proctree.SetSubreaper(); err != nil {
		return false, err
	}
	return true, nil
}

func probeProc() (bool, error) {
	if _, err := os.Stat(proctree.ProcDir() + "/self/stat"); err != nil {
		return false, err
	}
	if _, err := proctree.All(); err != nil {
		return false, err
	}
	return true, nil
}

// probeCgroup2 reports whether we can actually create and populate a cgroup,
// which is a stronger test than "is cgroup2 mounted".
func probeCgroup2() (bool, string, error) {
	const mount = "/sys/fs/cgroup"
	if _, err := os.Stat(filepath.Join(mount, "cgroup.controllers")); err != nil {
		return false, "", err
	}
	own := ownCgroupPath()
	base := filepath.Join(mount, own)
	probe := filepath.Join(base, "docker-systemd-probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		return false, "", err
	}
	err := os.Remove(probe)
	if err != nil {
		return false, "", err
	}
	return true, base, nil
}

// ownCgroupPath reads the container's own cgroup v2 path from /proc/self/cgroup.
func ownCgroupPath() string {
	data, err := os.ReadFile(proctree.ProcDir() + "/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		// cgroup v2 lines look like "0::/some/path".
		if strings.HasPrefix(line, "0::") {
			return strings.TrimPrefix(line, "0::")
		}
	}
	return ""
}

// ---- subreaper -------------------------------------------------------------

type subreaperBackend struct{}

func (b *subreaperBackend) Name() Name { return Subreaper }

func (b *subreaperBackend) Attach(unit string, supervisorPID int) (Handle, error) {
	return &subreaperHandle{root: supervisorPID}, nil
}

type subreaperHandle struct{ root int }

func (h *subreaperHandle) Members() ([]proctree.ProcRef, error) {
	return proctree.Enumerate(h.root)
}

func (h *subreaperHandle) IsEmpty() bool {
	m, err := h.Members()
	return err == nil && len(m) == 0
}

func (h *subreaperHandle) WaitEmpty(deadline time.Time) bool {
	for {
		if h.IsEmpty() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// KillAll signals tree members individually, and additionally each distinct
// process group as a cheap supplement. Signalling process groups alone is
// measurably insufficient (03 §4.3): daemonising means setsid(), which puts
// the daemon in a group of its own.
func (h *subreaperHandle) KillAll(sig syscall.Signal) error {
	members, err := h.Members()
	if err != nil {
		return err
	}
	for _, m := range members {
		_ = proctree.Signal(m, sig)
		if sig != syscall.SIGKILL && sig != syscall.SIGCONT {
			_ = proctree.Signal(m, syscall.SIGCONT)
		}
	}
	for _, g := range proctree.DistinctPGIDs(members) {
		_ = proctree.SignalGroup(g, sig)
	}
	return nil
}

func (h *subreaperHandle) Release() {}

// ---- degraded --------------------------------------------------------------

type degradedBackend struct{}

func (b *degradedBackend) Name() Name { return Degraded }

func (b *degradedBackend) Attach(unit string, supervisorPID int) (Handle, error) {
	return &degradedHandle{root: supervisorPID}, nil
}

type degradedHandle struct{ root int }

func (h *degradedHandle) Members() ([]proctree.ProcRef, error) {
	return proctree.Children(h.root)
}

func (h *degradedHandle) IsEmpty() bool {
	m, err := h.Members()
	return err == nil && len(m) == 0
}

func (h *degradedHandle) WaitEmpty(deadline time.Time) bool {
	for {
		if h.IsEmpty() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *degradedHandle) KillAll(sig syscall.Signal) error {
	members, err := h.Members()
	if err != nil {
		return err
	}
	for _, m := range members {
		_ = proctree.Signal(m, sig)
	}
	return nil
}

func (h *degradedHandle) Release() {}

// ---- cgroup v2 -------------------------------------------------------------

type cgroupBackend struct{ root string }

func (b *cgroupBackend) Name() Name { return Cgroup2 }

func (b *cgroupBackend) Attach(unit string, supervisorPID int) (Handle, error) {
	dir := filepath.Join(b.root, "docker-systemd-"+unit)
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		return nil, err
	}
	h := &cgroupHandle{dir: dir, fallback: &subreaperHandle{root: supervisorPID}}
	// Move the supervisor into the cgroup; every process it spawns inherits
	// membership, and membership is then kernel-enforced.
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"),
		[]byte(strconv.Itoa(supervisorPID)), 0o644); err != nil {
		os.Remove(dir)
		return nil, err
	}
	return h, nil
}

type cgroupHandle struct {
	dir string
	// fallback answers membership questions if the cgroup files become
	// unreadable mid-flight; correctness never depends on the accelerator.
	fallback *subreaperHandle
}

func (h *cgroupHandle) Members() ([]proctree.ProcRef, error) {
	data, err := os.ReadFile(filepath.Join(h.dir, "cgroup.procs"))
	if err != nil {
		return h.fallback.Members()
	}
	var out []proctree.ProcRef
	for _, line := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(line)
		if err != nil || pid == h.fallback.root {
			continue
		}
		if ref, ok := proctree.Lookup(pid); ok {
			out = append(out, ref)
		}
	}
	return out, nil
}

func (h *cgroupHandle) IsEmpty() bool {
	m, err := h.Members()
	return err == nil && len(m) == 0
}

func (h *cgroupHandle) WaitEmpty(deadline time.Time) bool {
	for {
		if h.IsEmpty() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *cgroupHandle) KillAll(sig syscall.Signal) error {
	if sig == syscall.SIGKILL {
		if err := os.WriteFile(filepath.Join(h.dir, "cgroup.kill"), []byte("1"), 0o644); err == nil {
			return nil
		}
	}
	members, err := h.Members()
	if err != nil {
		return err
	}
	for _, m := range members {
		_ = proctree.Signal(m, sig)
	}
	return nil
}

func (h *cgroupHandle) Release() {
	// The supervisor must leave the cgroup before it can be removed; that
	// happens when the supervisor exits, so removal is best-effort here.
	_ = os.Remove(h.dir)
}
