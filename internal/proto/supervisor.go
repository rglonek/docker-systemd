package proto

// Messages exchanged between the manager and a per-unit supervisor over the
// inherited SOCK_SEQPACKET socketpair on fd 3.

// StopMode selects the flavour of a STOP request.
type StopMode string

const (
	StopNormal   StopMode = "normal"
	StopRestart  StopMode = "restart"
	StopShutdown StopMode = "shutdown"
)

// StopRequest is the payload of a STOP frame.
type StopRequest struct {
	Mode      StopMode `json:"mode"`
	TimeoutMS int64    `json:"timeout_ms"`
}

// KillRequest is the payload of a KILL frame.
type KillRequest struct {
	Signal string `json:"signal"`
	Who    string `json:"who"` // main|all|control
}

// StateReport is the supervisor's replication of its unit's state.
type StateReport struct {
	State          string        `json:"state"`
	SubState       string        `json:"sub_state"`
	MainPID        int           `json:"main_pid"`
	ExecMainStatus int           `json:"exec_main_status"`
	Tasks          int           `json:"tasks"`
	Result         string        `json:"result"`
	StatusText     string        `json:"status_text,omitempty"`
	Since          string        `json:"since,omitempty"`
	NRestarts      int           `json:"n_restarts"`
	ConditionOK    bool          `json:"condition_result"`
	Processes      []ProcessInfo `json:"processes,omitempty"`
	Message        string        `json:"message,omitempty"`
}

// LogRecord is one assembled log line forwarded to the manager's broker.
type LogRecord struct {
	Unit       string `json:"unit"`
	Identifier string `json:"identifier,omitempty"`
	PID        int    `json:"pid"`
	Priority   int    `json:"priority"`
	TimeUnixNS int64  `json:"ts"`
	Message    string `json:"msg"`
}

// NotifyReport forwards a single sd_notify assignment.
type NotifyReport struct {
	SenderPID int    `json:"pid"`
	Key       string `json:"key"`
	Value     string `json:"value"`
}

// ExitReport announces the exit of a process inside the unit's tree.
type ExitReport struct {
	PID    int  `json:"pid"`
	Code   int  `json:"code"`
	Signal int  `json:"signal"`
	IsMain bool `json:"is_main"`
}

// Unit states, matching systemd's ActiveState vocabulary.
const (
	StateInactive     = "inactive"
	StateActivating   = "activating"
	StateActive       = "active"
	StateDeactivating = "deactivating"
	StateFailed       = "failed"
	StateReloading    = "reloading"
	StateAutoRestart  = "auto-restart"
	StateUnknown      = "unknown"
)

// Unit results, matching systemd's Result property.
const (
	ResultSuccess       = "success"
	ResultExitCode      = "exit-code"
	ResultSignal        = "signal"
	ResultTimeout       = "timeout"
	ResultResources     = "resources"
	ResultStartLimitHit = "start-limit-hit"
	ResultProtocol      = "protocol"
	ResultCondition     = "condition"
)
