//go:build !linux

package journal

import (
	"io/fs"
	"time"
)

func statInode(fi fs.FileInfo) uint64 { return 0 }

// watcher degrades to a plain timeout where inotify is unavailable.
type watcher struct{}

func newWatcher(dir string) *watcher { return &watcher{} }

func (w *watcher) wait(timeout time.Duration) <-chan struct{} {
	out := make(chan struct{}, 1)
	go func() {
		time.Sleep(timeout)
		out <- struct{}{}
	}()
	return out
}

func (w *watcher) close() {}
