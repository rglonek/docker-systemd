package supervisor

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ExitStatus is the outcome of one waited-for process.
type ExitStatus struct {
	PID    int
	Code   int
	Signal int
}

// Failed reports whether the process did not exit cleanly.
func (e ExitStatus) Failed() bool { return e.Code != 0 || e.Signal != 0 }

// String renders the status the way systemd's logs do.
func (e ExitStatus) String() string {
	if e.Signal != 0 {
		return "signal=" + signalString(e.Signal)
	}
	return "code=" + itoa(e.Code)
}

// Reaper owns the SIGCHLD loop.
//
// A subreaper learns of exits it never spawned, because an orphaned descendant
// reparents to it (property P2 in 03 §6.2). Every exit therefore goes through
// this one place: registered waiters are woken, and anything else is an
// adopted process whose exit is published on Adopted.
//
// It also replaces v0.5.x's procwait, whose global registry grew without bound
// and used an RWMutex as a condition variable (defects B16, F3).
type Reaper struct {
	mu      sync.Mutex
	waiters map[int]chan ExitStatus
	adopted chan ExitStatus
	stop    chan struct{}
	once    sync.Once
}

// NewReaper starts the reaping loop.
func NewReaper() *Reaper {
	r := &Reaper{
		waiters: map[int]chan ExitStatus{},
		adopted: make(chan ExitStatus, 256),
		stop:    make(chan struct{}),
	}
	go r.loop()
	return r
}

// Adopted publishes exits of processes nobody registered a waiter for.
func (r *Reaper) Adopted() <-chan ExitStatus { return r.adopted }

// Register returns a channel that receives the exit status of pid. It must be
// called before the process can exit — that is, by the spawning code path
// immediately after fork — so no exit can be missed.
func (r *Reaper) Register(pid int) <-chan ExitStatus {
	ch := make(chan ExitStatus, 1)
	r.mu.Lock()
	r.waiters[pid] = ch
	r.mu.Unlock()
	return ch
}

// Forget drops a waiter, for the case where the spawn itself failed.
func (r *Reaper) Forget(pid int) {
	r.mu.Lock()
	delete(r.waiters, pid)
	r.mu.Unlock()
}

// Stop ends the loop.
func (r *Reaper) Stop() {
	r.once.Do(func() { close(r.stop) })
}

func (r *Reaper) loop() {
	sigch := make(chan os.Signal, 16)
	signal.Notify(sigch, syscall.SIGCHLD)
	defer signal.Stop(sigch)

	// Signals coalesce, so a periodic sweep is not optional: two children
	// exiting while one SIGCHLD is pending would otherwise leave a zombie.
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()

	for {
		r.drain()
		select {
		case <-r.stop:
			return
		case <-sigch:
		case <-tick.C:
		}
	}
}

// drain reaps everything currently waitable.
func (r *Reaper) drain() {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil || pid <= 0 {
			return
		}
		st := ExitStatus{PID: pid}
		switch {
		case ws.Signaled():
			st.Signal = int(ws.Signal())
		default:
			st.Code = ws.ExitStatus()
		}
		r.deliver(st)
	}
}

func (r *Reaper) deliver(st ExitStatus) {
	r.mu.Lock()
	ch, ok := r.waiters[st.PID]
	if ok {
		delete(r.waiters, st.PID)
	}
	r.mu.Unlock()
	if ok {
		ch <- st
		close(ch)
		return
	}
	select {
	case r.adopted <- st:
	default:
		// The adopted channel is a diagnostic stream; dropping an entry when
		// it backs up is preferable to blocking the reaper and accumulating
		// zombies.
	}
}

func signalString(n int) string {
	return syscall.Signal(n).String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
