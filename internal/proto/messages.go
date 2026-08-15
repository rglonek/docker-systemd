package proto

import (
	"encoding/json"
	"io"
)

// Hello is the first frame a client sends.
type Hello struct {
	Version uint16 `json:"version"`
	Client  string `json:"client"`
}

// HelloAck is the manager's reply to Hello.
type HelloAck struct {
	Version      uint16   `json:"version"`
	Server       string   `json:"server"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Request is a client command.
type Request struct {
	Verb    string          `json:"verb"`
	Units   []string        `json:"units,omitempty"`
	Options RequestOptions  `json:"options"`
	Args    []string        `json:"args,omitempty"`
	Extra   json.RawMessage `json:"extra,omitempty"`
}

// RequestOptions carries the global systemctl/journalctl switches that the
// manager needs to see. Options that only affect client-side rendering stay in
// the client.
type RequestOptions struct {
	Now        bool     `json:"now,omitempty"`
	NoBlock    bool     `json:"no_block,omitempty"`
	Quiet      bool     `json:"quiet,omitempty"`
	All        bool     `json:"all,omitempty"`
	Force      bool     `json:"force,omitempty"`
	Runtime    bool     `json:"runtime,omitempty"`
	Types      []string `json:"types,omitempty"`
	States     []string `json:"states,omitempty"`
	Properties []string `json:"properties,omitempty"`
	Value      bool     `json:"value,omitempty"`
	Signal     string   `json:"signal,omitempty"`
	KillWho    string   `json:"kill_who,omitempty"`
	Lines      int      `json:"lines,omitempty"`
	Root       string   `json:"root,omitempty"`
}

// Result terminates every request. ExitCode is the client's process exit code.
type Result struct {
	ExitCode  int    `json:"exit_code"`
	ErrorKind string `json:"error_kind,omitempty"`
	Message   string `json:"message,omitempty"`
}

// Error kinds carried in Result.ErrorKind.
const (
	ErrKindNone        = ""
	ErrKindNoSuchUnit  = "no-such-unit"
	ErrKindMasked      = "masked"
	ErrKindNoInstall   = "no-install"
	ErrKindInvalidArgs = "invalid-args"
	ErrKindFailed      = "failed"
	ErrKindDenied      = "access-denied"
	ErrKindShutdown    = "shutting-down"
)

// UnitStatus is the machine-readable view of one unit, returned in DATA frames.
type UnitStatus struct {
	Name           string            `json:"name"`
	Names          []string          `json:"names,omitempty"`
	Description    string            `json:"description"`
	LoadState      string            `json:"load_state"`
	LoadError      string            `json:"load_error,omitempty"`
	ActiveState    string            `json:"active_state"`
	SubState       string            `json:"sub_state"`
	UnitFileState  string            `json:"unit_file_state"`
	MainPID        int               `json:"main_pid"`
	ExecMainPID    int               `json:"exec_main_pid"`
	ExecMainStatus int               `json:"exec_main_status"`
	Result         string            `json:"result"`
	StatusText     string            `json:"status_text,omitempty"`
	FragmentPath   string            `json:"fragment_path,omitempty"`
	DropInPaths    []string          `json:"drop_in_paths,omitempty"`
	Type           string            `json:"type,omitempty"`
	Restart        string            `json:"restart,omitempty"`
	NRestarts      int               `json:"n_restarts"`
	InvocationID   string            `json:"invocation_id,omitempty"`
	ConditionOK    bool              `json:"condition_result"`
	Tasks          int               `json:"tasks"`
	Processes      []ProcessInfo     `json:"processes,omitempty"`
	Since          string            `json:"since,omitempty"`
	Warnings       []string          `json:"warnings,omitempty"`
	Deps           map[string]string `json:"deps,omitempty"`
}

// ProcessInfo is one member of a unit's process tree.
type ProcessInfo struct {
	PID     int    `json:"pid"`
	Cmdline string `json:"cmdline"`
}

// UnitFileInfo is one row of `systemctl list-unit-files`.
type UnitFileInfo struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Path  string `json:"path,omitempty"`
}

// Capabilities reports the runtime probe results; surfaced by
// `systemctl show --property=Capabilities` so a bug report can carry them.
type Capabilities struct {
	Backend       string            `json:"backend"`
	Probes        map[string]string `json:"probes"`
	RuntimeDir    string            `json:"runtime_dir"`
	ControlSocket string            `json:"control_socket"`
	LogDir        string            `json:"log_dir"`
	Version       string            `json:"version"`
	Warnings      []string          `json:"warnings,omitempty"`
}

// WriteJSON marshals v and writes it as a frame of the given type.
func WriteJSON(w io.Writer, typ uint8, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return Write(w, Frame{Type: typ, Payload: b})
}

// WriteText writes a UTF-8 text frame (PROGRESS).
func WriteText(w io.Writer, typ uint8, s string) error {
	return Write(w, Frame{Type: typ, Payload: []byte(s)})
}

// ReadJSON reads f.Payload into v.
func ReadJSON(f Frame, v any) error {
	return json.Unmarshal(f.Payload, v)
}
