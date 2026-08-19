//go:build linux

package manager

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// dirWatchMask is what a package manager actually does to a unit directory.
//
// dpkg unpacks to `foo.service.dpkg-new` and renames into place (IN_MOVED_TO),
// rpm writes `;deadbeef` temporaries and renames likewise, `install`/`cp`
// write in place (IN_CLOSE_WRITE), enable creates a symlink (IN_CREATE), and
// removal deletes one (IN_DELETE/IN_MOVED_FROM). IN_MODIFY is deliberately
// absent: it fires on every write() of a large file, and IN_CLOSE_WRITE
// already reports the finished result once.
const dirWatchMask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_TO |
	unix.IN_MOVED_FROM | unix.IN_CLOSE_WRITE

// dirWatcher reports changes to a set of directories over inotify. Events are
// coalesced into a single-slot channel: the consumer only needs to know that
// *something* changed, never what.
type dirWatcher struct {
	fd int
	ch chan struct{}
}

// newDirWatcher starts watching dirs. Directories that do not exist are
// skipped rather than fatal — `/run/systemd/system` and `/etc/systemd/system`
// are frequently absent in a stock image — but if none of them can be watched
// there is nothing to report and the caller is told so.
func newDirWatcher(dirs []string) (*dirWatcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return nil, err
	}
	w := &dirWatcher{fd: fd, ch: make(chan struct{}, 1)}
	if w.Rescan(dirs) == 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("none of the %d unit directories could be watched", len(dirs))
	}
	go w.loop()
	return w, nil
}

// Events returns the coalescing change channel. It is closed when the watcher
// stops.
func (w *dirWatcher) Events() <-chan struct{} { return w.ch }

// Rescan makes the watch set exactly dirs-that-exist and returns how many are
// watched.
//
// inotify_add_watch is idempotent — re-adding a path returns the existing
// watch descriptor and rewrites its mask — so this needs no bookkeeping and
// also re-establishes a watch on a directory that was deleted and recreated
// (which is what an `apt purge` followed by an `apt install` does to
// `multi-user.target.wants/`).
func (w *dirWatcher) Rescan(dirs []string) int {
	watched := 0
	for _, d := range dirs {
		if _, err := unix.InotifyAddWatch(w.fd, d, dirWatchMask); err == nil {
			watched++
		}
	}
	return watched
}

func (w *dirWatcher) loop() {
	defer close(w.ch)
	buf := make([]byte, 8192)
	for {
		n, err := unix.Read(w.fd, buf)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n <= 0 {
			return
		}
		// The event bodies are not parsed: any event on a watched directory
		// means the registry has to be rebuilt from scratch anyway.
		select {
		case w.ch <- struct{}{}:
		default:
		}
	}
}
