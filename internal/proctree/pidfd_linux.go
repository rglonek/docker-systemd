//go:build linux

package proctree

import (
	"errors"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// pidfdSupported is probed once. pidfd_open(2) (Linux >= 5.3) and
// pidfd_send_signal(2) (>= 5.1) are permitted by Docker's default seccomp
// profile, which is what gives race-free waiting on and signalling of adopted
// pids — no (pid, starttime) re-validation dance, no risk of signalling a
// recycled PID.
var (
	pidfdOnce sync.Once
	pidfdOK   bool
)

// PidfdSupported reports whether pidfd_open works in this environment.
func PidfdSupported() bool {
	pidfdOnce.Do(func() {
		fd, err := unix.PidfdOpen(unix.Getpid(), 0)
		if err != nil {
			return
		}
		unix.Close(fd)
		pidfdOK = true
	})
	return pidfdOK
}

// Pidfd is an open handle on a process, valid across PID reuse.
type Pidfd struct {
	fd  int
	pid int
}

// OpenPidfd opens a pidfd for pid. It works on non-children, which is what
// lets a supervisor wait on a process it adopted rather than forked.
func OpenPidfd(pid int) (*Pidfd, error) {
	if !PidfdSupported() {
		return nil, errors.New("pidfd unsupported")
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, err
	}
	return &Pidfd{fd: fd, pid: pid}, nil
}

// Close releases the handle.
func (p *Pidfd) Close() error {
	if p == nil || p.fd < 0 {
		return nil
	}
	err := unix.Close(p.fd)
	p.fd = -1
	return err
}

// PID returns the pid the handle refers to.
func (p *Pidfd) PID() int { return p.pid }

// Signal sends sig without any PID-reuse hazard.
func (p *Pidfd) Signal(sig syscall.Signal) error {
	if p == nil || p.fd < 0 {
		return errors.New("pidfd closed")
	}
	return unix.PidfdSendSignal(p.fd, sig, nil, 0)
}

// Wait blocks until the process exits or the timeout expires. It reports
// whether the process exited.
func (p *Pidfd) Wait(timeout time.Duration) bool {
	if p == nil || p.fd < 0 {
		return true
	}
	ms := int(timeout / time.Millisecond)
	if ms < 0 {
		ms = -1
	}
	fds := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, ms)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false
		}
		return n > 0
	}
}

// Signal sends sig to a pid, preferring pidfd_send_signal and falling back to
// kill(2) guarded by a start-time re-read so a recycled PID is never targeted.
func Signal(ref ProcRef, sig syscall.Signal) error {
	if PidfdSupported() {
		fd, err := OpenPidfd(ref.PID)
		if err == nil {
			defer fd.Close()
			return fd.Signal(sig)
		}
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
	}
	if ref.StartTime != 0 {
		cur, ok := readStat(ref.PID)
		if !ok {
			return nil
		}
		if cur.StartTime != ref.StartTime {
			// The pid was recycled between enumeration and now; signalling it
			// would hit an unrelated process.
			return nil
		}
	}
	err := unix.Kill(ref.PID, sig)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

// SignalPID signals a bare pid with no start-time validation. Only for pids
// the caller forked itself.
func SignalPID(pid int, sig syscall.Signal) error {
	err := unix.Kill(pid, sig)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

// SignalGroup signals a whole process group. It is a cheap supplement to
// per-pid signalling, never a substitute: a daemonising process calls setsid()
// into its own group, so kill(-pgid) alone does not stop it (03 §4.3).
func SignalGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 1 {
		return nil
	}
	err := unix.Kill(-pgid, sig)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

// SetSubreaper marks the calling process as the reaper for its own
// descendants. Called by a supervisor as its first action, before spawning
// anything.
func SetSubreaper() error {
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}
