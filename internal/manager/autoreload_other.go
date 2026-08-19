//go:build !linux

package manager

import "errors"

// dirWatcher has no implementation off Linux; the manager falls back to
// requiring an explicit `systemctl daemon-reload`.
type dirWatcher struct{ ch chan struct{} }

func newDirWatcher(dirs []string) (*dirWatcher, error) {
	return nil, errors.New("inotify is not available on this platform")
}

func (w *dirWatcher) Events() <-chan struct{} { return w.ch }

func (w *dirWatcher) Rescan(dirs []string) int { return 0 }
