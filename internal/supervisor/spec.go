package supervisor

import (
	"docker-systemd/internal/backend"
	"docker-systemd/internal/unitfile"
)

// Config is what the manager sends a supervisor over its config fd at spawn
// time (invariant I3: a supervisor never reads unit files).
type Config struct {
	Unit *unitfile.Unit `json:"unit"`
	// Environment is the manager-side environment block the unit inherits,
	// already filtered by --pass-environment.
	Environment []string `json:"environment,omitempty"`
	// Backend names the membership backend selected at boot.
	Backend backend.Name `json:"backend"`
	// NotifySocket is the path the supervisor binds for Type=notify.
	NotifySocket string `json:"notify_socket,omitempty"`
	// HeartbeatMS is the tree re-scan interval while the unit has members
	// that are not direct children.
	HeartbeatMS int `json:"heartbeat_ms"`
	// LogToStderr mirrors the unit's records to the supervisor's stderr in
	// addition to forwarding them to the broker.
	LogToStderr bool `json:"log_to_stderr,omitempty"`
	// RecoveredPIDs are processes found by orphan recovery that the
	// replacement supervisor should adopt (03 §6.8). It cannot become their
	// parent, but it can pidfd_open each one to wait on and signal it.
	RecoveredPIDs []int `json:"recovered_pids,omitempty"`
	// Mode selects the initial action: "start" or "adopt".
	Mode string `json:"mode,omitempty"`
}

// ExecSpec is the trampoline's input: everything that must be applied between
// the supervisor's fork and the unit process's execve but that Go's
// SysProcAttr does not cover (10 §3).
//
// setrlimit, umask, nice, OOMScoreAdjust and PR_SET_NO_NEW_PRIVS are all
// inherited across execve, so applying them in a freshly exec'd trampoline is
// semantically identical to applying them in the forked child — and it is safe,
// because the trampoline is an ordinary single-threaded program at that point
// rather than the async-signal-safe-only window after fork(2).
type ExecSpec struct {
	Path        string   `json:"path"`
	Argv        []string `json:"argv"`
	Env         []string `json:"env"`
	Dir         string   `json:"dir,omitempty"`
	DirOptional bool     `json:"dir_optional,omitempty"`

	UID    *int  `json:"uid,omitempty"`
	GID    *int  `json:"gid,omitempty"`
	Groups []int `json:"groups,omitempty"`

	UMask           *uint32          `json:"umask,omitempty"`
	Nice            *int             `json:"nice,omitempty"`
	OOMScoreAdjust  *int             `json:"oom_score_adjust,omitempty"`
	NoNewPrivileges bool             `json:"no_new_privileges,omitempty"`
	Limits          map[string]Limit `json:"limits,omitempty"`
}

// Limit is one resource limit request.
type Limit struct {
	Soft uint64 `json:"soft"`
	Hard uint64 `json:"hard"`
}

// systemd's conventional exit codes for a failed pre-exec step. Reporting
// these rather than a generic failure is what makes `status=203/EXEC` and
// `status=217/USER` in the logs mean what an operator expects.
const (
	ExitChdir      = 200
	ExitExec       = 203
	ExitStdio      = 208
	ExitGroup      = 216
	ExitUser       = 217
	ExitLimits     = 219
	ExitNoNewPrivs = 225
)
