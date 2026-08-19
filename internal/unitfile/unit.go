package unitfile

import (
	"strings"
	"time"
)

// Load states, matching systemd's LoadState property.
const (
	LoadLoaded     = "loaded"
	LoadNotFound   = "not-found"
	LoadBadSetting = "bad-setting"
	LoadMasked     = "masked"
)

// Service types.
const (
	TypeSimple       = "simple"
	TypeExec         = "exec"
	TypeForking      = "forking"
	TypeOneshot      = "oneshot"
	TypeIdle         = "idle"
	TypeNotify       = "notify"
	TypeNotifyReload = "notify-reload"
	TypeDBus         = "dbus"
)

// Kill modes.
const (
	KillControlGroup = "control-group"
	KillMixed        = "mixed"
	KillProcess      = "process"
	KillNone         = "none"
)

// Restart policies.
const (
	RestartNo         = "no"
	RestartAlways     = "always"
	RestartOnSuccess  = "on-success"
	RestartOnFailure  = "on-failure"
	RestartOnAbnormal = "on-abnormal"
	RestartOnAbort    = "on-abort"
	RestartOnWatchdog = "on-watchdog"
)

// Unit is a fully resolved unit: fragment plus drop-ins, specifiers expanded,
// values parsed. It is immutable once built and is what a supervisor receives
// over its config fd (invariant I3).
type Unit struct {
	Name         string    `json:"name"`
	Names        []string  `json:"names,omitempty"` // aliases, including Name
	InvocationID string    `json:"invocation_id,omitempty"`
	FragmentPath string    `json:"fragment_path,omitempty"`
	DropInPaths  []string  `json:"drop_in_paths,omitempty"`
	LoadState    string    `json:"load_state"`
	LoadError    string    `json:"load_error,omitempty"`
	Warnings     []Warning `json:"warnings,omitempty"`
	Synthetic    bool      `json:"synthetic,omitempty"`

	Prefix   string `json:"prefix,omitempty"`
	Instance string `json:"instance,omitempty"`

	Unit    UnitSection     `json:"unit"`
	Service *ServiceSection `json:"service,omitempty"`
	Install InstallSection  `json:"install"`
}

// Kind returns the unit suffix without the dot, e.g. "service" or "target".
func (u *Unit) Kind() string { return strings.TrimPrefix(UnitSuffix(u.Name), ".") }

// IsTarget reports whether the unit is a synchronisation point with no
// processes.
func (u *Unit) IsTarget() bool { return u.Kind() == "target" }

// Condition is one Condition*= or Assert*= directive.
type Condition struct {
	Kind   string `json:"kind"`   // e.g. "PathExists"
	Value  string `json:"value"`  // the argument, after `!` removal
	Negate bool   `json:"negate"` // the leading `!`
	Assert bool   `json:"assert"` // Assert*= rather than Condition*=
}

// UnitSection is the parsed [Unit] section.
type UnitSection struct {
	Description   string   `json:"description,omitempty"`
	Documentation []string `json:"documentation,omitempty"`

	Wants     []string `json:"wants,omitempty"`
	Requires  []string `json:"requires,omitempty"`
	Requisite []string `json:"requisite,omitempty"`
	BindsTo   []string `json:"binds_to,omitempty"`
	PartOf    []string `json:"part_of,omitempty"`
	Upholds   []string `json:"upholds,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`

	WantedBy   []string `json:"wanted_by,omitempty"`
	RequiredBy []string `json:"required_by,omitempty"`
	UpheldBy   []string `json:"upheld_by,omitempty"`

	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`

	OnFailure        []string `json:"on_failure,omitempty"`
	OnSuccess        []string `json:"on_success,omitempty"`
	OnFailureJobMode string   `json:"on_failure_job_mode,omitempty"`

	Conditions []Condition `json:"conditions,omitempty"`

	StopWhenUnneeded  bool `json:"stop_when_unneeded,omitempty"`
	RefuseManualStart bool `json:"refuse_manual_start,omitempty"`
	RefuseManualStop  bool `json:"refuse_manual_stop,omitempty"`
	AllowIsolate      bool `json:"allow_isolate,omitempty"`
	IgnoreOnIsolate   bool `json:"ignore_on_isolate,omitempty"`

	JobTimeoutSec        time.Duration `json:"job_timeout,omitempty"`
	JobRunningTimeoutSec time.Duration `json:"job_running_timeout,omitempty"`

	StartLimitInterval time.Duration `json:"start_limit_interval"`
	StartLimitBurst    int           `json:"start_limit_burst"`
	StartLimitAction   string        `json:"start_limit_action,omitempty"`

	FailureAction string `json:"failure_action,omitempty"`
	SuccessAction string `json:"success_action,omitempty"`

	DefaultDependencies bool   `json:"default_dependencies"`
	SourcePath          string `json:"source_path,omitempty"`
}

// RLimit is one Limit*= directive value.
type RLimit struct {
	Soft uint64 `json:"soft"`
	Hard uint64 `json:"hard"`
}

// ServiceSection is the parsed [Service] section.
type ServiceSection struct {
	Type            string `json:"type"`
	ExitType        string `json:"exit_type,omitempty"`
	RemainAfterExit bool   `json:"remain_after_exit,omitempty"`
	GuessMainPID    bool   `json:"guess_main_pid"`
	PIDFile         string `json:"pid_file,omitempty"`
	BusName         string `json:"bus_name,omitempty"`

	ExecStart     []Command `json:"exec_start,omitempty"`
	ExecStartPre  []Command `json:"exec_start_pre,omitempty"`
	ExecStartPost []Command `json:"exec_start_post,omitempty"`
	ExecCondition []Command `json:"exec_condition,omitempty"`
	ExecReload    []Command `json:"exec_reload,omitempty"`
	ExecStop      []Command `json:"exec_stop,omitempty"`
	ExecStopPost  []Command `json:"exec_stop_post,omitempty"`

	Restart                  string        `json:"restart"`
	RestartSec               time.Duration `json:"restart_sec"`
	RestartPreventExitStatus ExitStatusSet `json:"restart_prevent_exit_status,omitempty"`
	RestartForceExitStatus   ExitStatusSet `json:"restart_force_exit_status,omitempty"`
	SuccessExitStatus        ExitStatusSet `json:"success_exit_status,omitempty"`

	TimeoutStartSec time.Duration `json:"timeout_start"`
	TimeoutStopSec  time.Duration `json:"timeout_stop"`
	TimeoutAbortSec time.Duration `json:"timeout_abort,omitempty"`
	RuntimeMaxSec   time.Duration `json:"runtime_max,omitempty"`
	WatchdogSec     time.Duration `json:"watchdog,omitempty"`

	KillMode          string `json:"kill_mode"`
	KillSignal        string `json:"kill_signal"`
	RestartKillSignal string `json:"restart_kill_signal,omitempty"`
	FinalKillSignal   string `json:"final_kill_signal"`
	SendSIGHUP        bool   `json:"send_sighup,omitempty"`
	SendSIGKILL       bool   `json:"send_sigkill"`

	User                string   `json:"user,omitempty"`
	Group               string   `json:"group,omitempty"`
	SupplementaryGroups []string `json:"supplementary_groups,omitempty"`

	Environment      []string  `json:"environment,omitempty"`
	EnvironmentFiles []EnvFile `json:"environment_files,omitempty"`
	PassEnvironment  []string  `json:"pass_environment,omitempty"`
	UnsetEnvironment []string  `json:"unset_environment,omitempty"`

	WorkingDirectory   string            `json:"working_directory,omitempty"`
	WorkingDirOptional bool              `json:"working_directory_optional,omitempty"`
	UMask              *uint32           `json:"umask,omitempty"`
	Nice               *int              `json:"nice,omitempty"`
	OOMScoreAdjust     *int              `json:"oom_score_adjust,omitempty"`
	NoNewPrivileges    bool              `json:"no_new_privileges,omitempty"`
	Limits             map[string]RLimit `json:"limits,omitempty"`

	RuntimeDirectory         []string `json:"runtime_directory,omitempty"`
	StateDirectory           []string `json:"state_directory,omitempty"`
	CacheDirectory           []string `json:"cache_directory,omitempty"`
	LogsDirectory            []string `json:"logs_directory,omitempty"`
	ConfigurationDirectory   []string `json:"configuration_directory,omitempty"`
	RuntimeDirectoryMode     uint32   `json:"runtime_directory_mode"`
	StateDirectoryMode       uint32   `json:"state_directory_mode"`
	CacheDirectoryMode       uint32   `json:"cache_directory_mode"`
	LogsDirectoryMode        uint32   `json:"logs_directory_mode"`
	ConfigurationDirMode     uint32   `json:"configuration_directory_mode"`
	RuntimeDirectoryPreserve string   `json:"runtime_directory_preserve,omitempty"`

	StandardInput     string `json:"standard_input"`
	StandardOutput    string `json:"standard_output"`
	StandardError     string `json:"standard_error"`
	SyslogIdentifier  string `json:"syslog_identifier,omitempty"`
	SyslogLevel       int    `json:"syslog_level"`
	SyslogLevelPrefix bool   `json:"syslog_level_prefix"`
}

// InstallSection is the parsed [Install] section.
type InstallSection struct {
	WantedBy        []string `json:"wanted_by,omitempty"`
	RequiredBy      []string `json:"required_by,omitempty"`
	UpheldBy        []string `json:"upheld_by,omitempty"`
	Alias           []string `json:"alias,omitempty"`
	Also            []string `json:"also,omitempty"`
	DefaultInstance string   `json:"default_instance,omitempty"`
}

// Empty reports whether the unit has no installation configuration, which
// `systemctl enable` must report as an error (06 §4).
func (i InstallSection) Empty() bool {
	return len(i.WantedBy) == 0 && len(i.RequiredBy) == 0 && len(i.UpheldBy) == 0 &&
		len(i.Alias) == 0 && len(i.Also) == 0
}

// defaultUnitSection returns a [Unit] section with systemd's defaults.
func defaultUnitSection() UnitSection {
	return UnitSection{
		StartLimitInterval:  10 * time.Second,
		StartLimitBurst:     5,
		StartLimitAction:    "none",
		FailureAction:       "none",
		SuccessAction:       "none",
		DefaultDependencies: true,
		OnFailureJobMode:    "replace",
	}
}

// defaultServiceSection returns a [Service] section with systemd's defaults.
//
// Two of these differ from v0.5.x and matter: TimeoutStopSec is 90 s rather
// than 5 s, and RestartSec is 100 ms rather than a 1 s floor (defects C11,
// C12).
func defaultServiceSection() ServiceSection {
	return ServiceSection{
		Type:                     TypeSimple,
		ExitType:                 "main",
		GuessMainPID:             true,
		Restart:                  RestartNo,
		RestartSec:               100 * time.Millisecond,
		TimeoutStartSec:          90 * time.Second,
		TimeoutStopSec:           90 * time.Second,
		KillMode:                 KillControlGroup,
		KillSignal:               "SIGTERM",
		FinalKillSignal:          "SIGKILL",
		SendSIGKILL:              true,
		StandardInput:            "null",
		StandardOutput:           "journal",
		StandardError:            "journal",
		SyslogLevel:              6,
		SyslogLevelPrefix:        true,
		RuntimeDirectoryMode:     0o755,
		StateDirectoryMode:       0o755,
		CacheDirectoryMode:       0o755,
		LogsDirectoryMode:        0o755,
		ConfigurationDirMode:     0o755,
		RuntimeDirectoryPreserve: "no",
		Limits:                   map[string]RLimit{},
	}
}

// NewUnit returns a unit with all sections at their defaults.
func NewUnit(name string) *Unit {
	u := &Unit{
		Name:      name,
		LoadState: LoadNotFound,
		Unit:      defaultUnitSection(),
		Install:   InstallSection{},
	}
	u.Prefix, u.Instance = SplitTemplate(name)
	if u.Kind() == "service" {
		svc := defaultServiceSection()
		u.Service = &svc
	}
	return u
}

// EffectiveTimeoutStop returns the stop timeout to apply, treating Infinity as
// a very long but finite budget so the ladder always terminates.
func (s *ServiceSection) EffectiveTimeoutStop() time.Duration {
	if s == nil {
		return 90 * time.Second
	}
	if s.TimeoutStopSec == Infinity {
		return 24 * time.Hour
	}
	return s.TimeoutStopSec
}

// EffectiveTimeoutStart is the start-side equivalent.
func (s *ServiceSection) EffectiveTimeoutStart() time.Duration {
	if s == nil {
		return 90 * time.Second
	}
	if s.TimeoutStartSec == Infinity {
		return 24 * time.Hour
	}
	return s.TimeoutStartSec
}
