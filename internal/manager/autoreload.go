package manager

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"docker-systemd/internal/paths"
)

// Debounce parameters for the automatic reload.
//
// A package installation writes a unit file, then its `.wants` symlink, then
// often a drop-in, and dpkg/rpm do that once per package in a transaction.
// Reloading per event would rebuild the registry dozens of times for one
// `apt install`, so events are coalesced until the unit directories have been
// quiet for autoReloadQuiet — bounded by autoReloadMax so a long-running
// transaction still becomes visible while it is in progress.
const (
	autoReloadQuiet = 400 * time.Millisecond
	autoReloadMax   = 5 * time.Second
)

// unitDropinSuffixes are the subdirectories of a unit directory that also
// contribute to the registry: `foo.target.wants/`, `foo.target.requires/` and
// `foo.service.d/`. They are watched as well, because creating a symlink
// inside one of them produces no event on the parent directory.
var unitDropinSuffixes = []string{".wants", ".requires", ".d"}

// watchDirs returns every directory whose contents define the registry: the
// unit search path plus its `.wants`/`.requires`/`.d` subdirectories.
//
// It is called again after each reload, so subdirectories that a package
// creates (the usual `multi-user.target.wants/` on a first install) start
// being watched as soon as the reload they triggered is done.
func (m *Manager) watchDirs() []string {
	var out []string
	for _, dir := range paths.UnitSearchPath(m.opts.Root) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			// The directory does not exist yet. Watch its parent instead, so
			// that a package creating /etc/systemd/system (or a distro without
			// /usr/lib/systemd/system gaining one) is still noticed; the
			// rescan after that reload picks up the real directory. Only one
			// level up, because the level above that is /etc and /usr, where
			// every package writes and nothing we care about lives.
			if parent := filepath.Dir(dir); dirExists(parent) {
				out = append(out, parent)
			}
			continue
		}
		out = append(out, dir)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			for _, suffix := range unitDropinSuffixes {
				if strings.HasSuffix(e.Name(), suffix) {
					out = append(out, filepath.Join(dir, e.Name()))
					break
				}
			}
		}
	}
	return out
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// startAutoReload watches the unit directories and reloads the registry when
// they change.
//
// Real systemd requires an explicit `systemctl daemon-reload`, and distro
// maintainer scripts do call it — but only from a postinst that decided
// systemd is running, and never for a unit file that arrived any other way (a
// package whose packaging predates `dh_installsystemd`, `docker cp`, a
// Dockerfile `COPY`, `apk add`, an `rpm -i --noscripts`). In a container the
// cost of being wrong is a service the operator cannot see at all, so the
// manager watches instead of relying on the hook being called.
//
// The reload is exactly the one `systemctl daemon-reload` performs: units that
// are already running keep the configuration they were started with, and
// nothing is started or stopped as a side effect.
func (m *Manager) startAutoReload() {
	if m.opts.NoAutoReload {
		return
	}
	dirs := m.watchDirs()
	w, err := newDirWatcher(dirs)
	if err != nil {
		m.log.Warnf("cannot watch the unit directories (%v); "+
			"run `systemctl daemon-reload` by hand after installing a package", err)
		return
	}
	m.log.Debugf("watching %d unit directories for changes", len(dirs))
	go m.autoReloadLoop(w)
}

// autoReloadLoop coalesces change events and reloads once per quiet batch.
func (m *Manager) autoReloadLoop(w *dirWatcher) {
	events := w.Events()
	for {
		if _, ok := <-events; !ok {
			return
		}
		if !m.coalesce(events) {
			return
		}
		if m.shuttingDown.Load() {
			return
		}
		m.log.Infof("unit files changed on disk; reloading")
		m.DaemonReload()
		// A first-time install creates the `.wants` directory the enable
		// symlink lands in, so the watch set is refreshed after every reload.
		w.Rescan(m.watchDirs())
	}
}

// coalesce waits for the change events to stop arriving, or for the hard
// deadline, whichever comes first. It reports false if the watcher died.
func (m *Manager) coalesce(events <-chan struct{}) bool {
	hard := time.After(autoReloadMax)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return false
			}
		case <-time.After(autoReloadQuiet):
			return true
		case <-hard:
			return true
		}
	}
}
