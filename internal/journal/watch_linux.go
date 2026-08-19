//go:build linux

package journal

import (
	"io/fs"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func statInode(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// watcher wakes a follower when the log directory changes. inotify avoids
// polling in the common idle case; if it is unavailable the caller's timeout
// still bounds the wait, so correctness never depends on it.
type watcher struct {
	fd   int
	ch   chan struct{}
	done chan struct{}
}

func newWatcher(dir string) *watcher {
	w := &watcher{fd: -1, ch: make(chan struct{}, 1), done: make(chan struct{})}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return w
	}
	if _, err := unix.InotifyAddWatch(fd, dir,
		unix.IN_MODIFY|unix.IN_CREATE|unix.IN_MOVED_TO|unix.IN_MOVED_FROM); err != nil {
		unix.Close(fd)
		return w
	}
	w.fd = fd
	go w.loop()
	return w
}

func (w *watcher) loop() {
	buf := make([]byte, 4096)
	for {
		n, err := unix.Read(w.fd, buf)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n <= 0 {
			return
		}
		select {
		case w.ch <- struct{}{}:
		default:
		}
		select {
		case <-w.done:
			return
		default:
		}
	}
}

// wait returns a channel that fires on a directory change or after timeout.
func (w *watcher) wait(timeout time.Duration) <-chan struct{} {
	out := make(chan struct{}, 1)
	go func() {
		select {
		case <-w.ch:
		case <-time.After(timeout):
		}
		out <- struct{}{}
	}()
	return out
}

func (w *watcher) close() {
	close(w.done)
	if w.fd >= 0 {
		unix.Close(w.fd)
		w.fd = -1
	}
}
