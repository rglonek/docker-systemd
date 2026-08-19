package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"

	"docker-systemd/internal/unitfile"
	"golang.org/x/sys/unix"
)

// Trampoline fd numbers, inherited across the exec that starts the helper.
const (
	// SpecFD carries the JSON ExecSpec.
	SpecFD = 3
	// StatusFD carries a failure report. It is marked close-on-exec just
	// before execve, so the supervisor sees EOF on a successful exec and a
	// message on a failed one — the mechanism Type=exec needs to know whether
	// the binary actually started (03 §7).
	StatusFD = 4
)

// RunExecHelper is the `--exec-helper` mode. It applies the parts of the
// execution context that SysProcAttr cannot express and then replaces itself
// with the unit's real binary. It never returns on success.
func RunExecHelper() int {
	specFile := os.NewFile(SpecFD, "exec-spec")
	if specFile == nil {
		fmt.Fprintln(os.Stderr, "exec-helper: no spec on fd 3")
		return ExitExec
	}
	var spec ExecSpec
	if err := json.NewDecoder(specFile).Decode(&spec); err != nil {
		reportExecFailure(ExitExec, fmt.Sprintf("cannot read exec spec: %v", err))
		return ExitExec
	}
	specFile.Close()

	if code, msg := applyExecSpec(&spec); code != 0 {
		reportExecFailure(code, msg)
		return code
	}

	// Mark the status pipe close-on-exec: its EOF is the success signal.
	if _, _, errno := unix.Syscall(unix.SYS_FCNTL, StatusFD, unix.F_SETFD, unix.FD_CLOEXEC); errno != 0 {
		// Not fatal: the supervisor falls back to waiting for the process.
		_ = errno
	}

	if err := unix.Exec(spec.Path, spec.Argv, spec.Env); err != nil {
		reportExecFailure(ExitExec, fmt.Sprintf("%s: %v", spec.Path, err))
		return ExitExec
	}
	return 0 // unreachable
}

// applyExecSpec performs the pre-exec setup, returning a systemd-conventional
// exit code and a message on failure.
func applyExecSpec(spec *ExecSpec) (int, string) {
	// Resource limits first: lowering them after dropping privileges would
	// still work, but raising a soft limit up to the inherited hard limit
	// would not.
	for name, l := range spec.Limits {
		res, ok := unitfile.RLimitResource(name)
		if !ok {
			continue
		}
		if err := applyLimit(res, l); err != nil {
			return ExitLimits, fmt.Sprintf("Limit%s: %v", name, err)
		}
	}

	if spec.UMask != nil {
		unix.Umask(int(*spec.UMask))
	}
	if spec.Nice != nil {
		if err := unix.Setpriority(unix.PRIO_PROCESS, 0, *spec.Nice); err != nil {
			// Lowering the nice value needs CAP_SYS_NICE; warn rather than
			// refuse to start a unit over a scheduling hint.
			fmt.Fprintf(os.Stderr, "exec-helper: Nice=%d ignored: %v\n", *spec.Nice, err)
		}
	}
	if spec.OOMScoreAdjust != nil {
		if err := os.WriteFile("/proc/self/oom_score_adj",
			[]byte(fmt.Sprintf("%d", *spec.OOMScoreAdjust)), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "exec-helper: OOMScoreAdjust=%d ignored: %v\n",
				*spec.OOMScoreAdjust, err)
		}
	}

	if spec.Dir != "" {
		if err := os.Chdir(spec.Dir); err != nil {
			if !spec.DirOptional {
				return ExitChdir, fmt.Sprintf("WorkingDirectory=%s: %v", spec.Dir, err)
			}
			_ = os.Chdir("/")
		}
	} else {
		_ = os.Chdir("/")
	}

	// Credentials last, and in this order. A failure to drop privileges is a
	// failure to start the unit, never a silent run-as-root: v0.5.x skipped
	// the credential change entirely whenever the resolved uid was 0, so
	// `User=root Group=adm` ran as root:root (defect C6).
	if len(spec.Groups) > 0 {
		if err := syscall.Setgroups(spec.Groups); err != nil {
			return ExitGroup, fmt.Sprintf("setgroups: %v", err)
		}
	}
	if spec.GID != nil {
		if err := syscall.Setgid(*spec.GID); err != nil {
			return ExitGroup, fmt.Sprintf("setgid(%d): %v", *spec.GID, err)
		}
	}
	if spec.NoNewPrivileges {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return ExitNoNewPrivs, fmt.Sprintf("PR_SET_NO_NEW_PRIVS: %v", err)
		}
	}
	if spec.UID != nil {
		if err := syscall.Setuid(*spec.UID); err != nil {
			return ExitUser, fmt.Sprintf("setuid(%d): %v", *spec.UID, err)
		}
	}
	return 0, ""
}

// applyLimit clamps a requested limit against the container's inherited hard
// limit (05 §7.4). setrlimit may always lower a limit, and may raise a soft
// limit up to the hard limit, without any capability; only raising the hard
// limit needs CAP_SYS_RESOURCE.
func applyLimit(res int, want Limit) error {
	var cur unix.Rlimit
	if err := unix.Getrlimit(res, &cur); err != nil {
		return err
	}
	newHard := want.Hard
	if cur.Max != unix.RLIM_INFINITY && (newHard == unix.RLIM_INFINITY || newHard > cur.Max) {
		newHard = cur.Max
	}
	newSoft := want.Soft
	if newHard != unix.RLIM_INFINITY && (newSoft == unix.RLIM_INFINITY || newSoft > newHard) {
		newSoft = newHard
	}
	lim := unix.Rlimit{Cur: newSoft, Max: newHard}
	if err := unix.Setrlimit(res, &lim); err != nil {
		// Retry raising only the soft limit: a hard-limit raise we are not
		// allowed to make must not lose the soft-limit change as well.
		lim = unix.Rlimit{Cur: newSoft, Max: cur.Max}
		if newSoft > cur.Max && cur.Max != unix.RLIM_INFINITY {
			lim.Cur = cur.Max
		}
		return unix.Setrlimit(res, &lim)
	}
	return nil
}

// reportExecFailure writes a diagnostic to the status pipe so the supervisor
// can attribute the failure precisely rather than reporting a bare exit code.
func reportExecFailure(code int, msg string) {
	f := os.NewFile(StatusFD, "exec-status")
	if f == nil {
		fmt.Fprintf(os.Stderr, "exec-helper: %s\n", msg)
		return
	}
	fmt.Fprintf(f, "%d %s", code, msg)
	f.Close()
}

// ClampedLimits reports which of a unit's Limit*= directives cannot be honoured
// at their requested value, so the manager can warn once at load with the
// docker flag that would fix it.
func ClampedLimits(limits map[string]unitfile.RLimit) map[string]uint64 {
	out := map[string]uint64{}
	for name, l := range limits {
		res, ok := unitfile.RLimitResource(name)
		if !ok {
			continue
		}
		var cur unix.Rlimit
		if err := unix.Getrlimit(res, &cur); err != nil {
			continue
		}
		if cur.Max == unix.RLIM_INFINITY {
			continue
		}
		if l.Hard == unix.RLIM_INFINITY || l.Hard > cur.Max {
			out[name] = cur.Max
		}
	}
	return out
}
