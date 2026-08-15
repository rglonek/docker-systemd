package unitfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

func filepathGlob(pattern string) ([]string, error) { return filepath.Glob(pattern) }

func numCPU() int { return runtime.NumCPU() }

func totalMemory() int64 {
	var si unix.Sysinfo_t
	if err := unix.Sysinfo(&si); err != nil {
		return 0
	}
	return int64(si.Totalram) * int64(si.Unit)
}

// isMountPoint reports whether path is a mount point, by comparing its device
// number with its parent's. /proc/self/mountinfo would be more precise but is
// not always readable under a restricted /proc, and this test needs no
// privileges.
func isMountPoint(path string) bool {
	var st, parent unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return false
	}
	if err := unix.Lstat(filepath.Dir(strings.TrimRight(path, "/")), &parent); err != nil {
		return false
	}
	return st.Dev != parent.Dev
}

// MachineID reads /etc/machine-id, synthesising and persisting one if absent,
// so that the %m specifier always expands to something stable.
func MachineID() string {
	data, err := os.ReadFile(MachineIDPath)
	if err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id
		}
	}
	id := randomHex(16)
	// Persisting is best-effort: a read-only /etc must not stop the boot.
	_ = os.WriteFile(MachineIDPath, []byte(id+"\n"), 0o444)
	return id
}
