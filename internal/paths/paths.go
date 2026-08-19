// Package paths centralises the on-disk layout from
// designs/docs/next/04-architecture.md §4 and the unit-file search path from
// designs/docs/next/05-unit-semantics.md §1.
package paths

import (
	"os"
	"path/filepath"
	"sort"
)

// Runtime state. Everything lives under /run rather than /tmp: /run is
// root-owned and not world-writable in every supported base image, which
// removes the escalation vector E1 outright.
const (
	RuntimeDir    = "/run/docker-systemd"
	ControlSocket = RuntimeDir + "/control.sock"
	NotifyDir     = RuntimeDir + "/notify"
	UnitStateDir  = RuntimeDir + "/units"
	BootIDFile    = RuntimeDir + "/boot-id"
	InstalledFile = RuntimeDir + "/installed"

	// SystemdMarkerDir must exist as a directory or distro maintainer scripts
	// conclude systemd is not running and skip unit registration (B19).
	SystemdMarkerDir = "/run/systemd/system"

	// LegacySocket is the v0.5.x control socket, recreated as a symlink only
	// under --compat-tmp-socket.
	LegacySocket = "/tmp/docker-systemd.sock"

	// LegacyBootFile is v0.5.x's boot marker; removed at boot so that
	// committed images are not polluted.
	LegacyBootFile = "/etc/boot-time"

	// LegacyPreloadLib is the shared object the removed LD_PRELOAD subsystem
	// installed. A stale /etc/ld.so.preload entry naming it would break every
	// dynamically linked binary in an image committed from an old container,
	// so the manager removes its own entry at boot.
	LegacyPreloadLib = "/usr/local/lib/fork.so"
	LdSoPreload      = "/etc/ld.so.preload"

	// LogDir holds per-unit log files.
	LogDir = "/var/log/services"

	// MachineIDFile is read (and synthesised if absent) for the %m specifier.
	MachineIDFile = "/etc/machine-id"
)

// Modes for the runtime layout.
//
// The runtime and notify directories are 0711 — searchable but not listable —
// rather than 0700. A unit that dropped to User= has to reach its own
// $NOTIFY_SOCKET, and the kernel enforces the search bit on every component of
// the path regardless of whether the process was told where the socket is. With
// 0700 a Type=notify unit running as a non-root user could never deliver
// READY=1, so it hung in "activating" until TimeoutStartSec — forever for
// mysql.service, which ships TimeoutSec=infinity.
//
// Nothing is given away by the search bit: neither directory can be listed, the
// control socket is 0600, per-unit state files are 0600, and every notification
// that changes unit state is attributed to a tree member through
// SO_PASSCRED before it is acted on.
const (
	ModeRuntimeDir = 0o711
	ModeStateDir   = 0o700
	ModeControlSck = 0o600
	ModeNotifyDir  = 0o711
	ModeNotifySck  = 0o666
	ModeLogDir     = 0o750
	ModeLogFile    = 0o640
	ModeMarkerDir  = 0o755
)

// unitSearchPath is the highest-precedence-first list of unit directories.
var unitSearchPath = []string{
	"/etc/systemd/system",
	"/run/systemd/system",
	"/usr/lib/systemd/system",
	"/lib/systemd/system",
}

// UnitSearchPath returns the unit-file directories to scan, highest precedence
// first, with directories that resolve to the same real path collapsed to the
// first (highest precedence) spelling.
//
// On a usrmerge system /lib/systemd/system and /usr/lib/systemd/system are the
// same directory; scanning both would load every vendor unit twice. Resolving
// with filepath.EvalSymlinks and de-duplicating is correct on merged, unmerged
// and partially-merged layouts alike, and replaces the six-branch symlink
// heuristic of common.GetSystemdPaths (F13).
func UnitSearchPath(root string) []string {
	seen := make(map[string]bool, len(unitSearchPath))
	out := make([]string, 0, len(unitSearchPath))
	for _, dir := range unitSearchPath {
		p := filepath.Join(root, dir)
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			// A directory that does not exist yet is still worth keeping in
			// the list: enable/mask may create it. De-duplicate on the
			// cleaned path in that case.
			real = filepath.Clean(p)
		}
		if seen[real] {
			continue
		}
		seen[real] = true
		out = append(out, p)
	}
	return out
}

// WritableUnitDir is where enable/mask/edit write, i.e. the administrator
// directory.
func WritableUnitDir(root string) string { return filepath.Join(root, "/etc/systemd/system") }

// UnitLogFile returns the active log file for a unit.
func UnitLogFile(unit string) string { return filepath.Join(LogDir, unit+".log") }

// RotatedLogFiles returns the rotated generations of a unit's log, oldest
// first, so a reader can concatenate them ahead of the active file.
func RotatedLogFiles(unit string, count int) []string {
	out := make([]string, 0, count)
	for i := count; i >= 1; i-- {
		out = append(out, UnitLogFile(unit)+"."+itoa(i))
	}
	return out
}

// AllLogFiles lists every log file present in LogDir, sorted by name.
func AllLogFiles() ([]string, error) {
	entries, err := os.ReadDir(LogDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, filepath.Join(LogDir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
