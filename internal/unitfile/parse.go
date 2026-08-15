package unitfile

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Parser applies fragments and drop-ins to a Unit in order.
type Parser struct {
	unit *Unit
	ctx  SpecifierContext
	// warn collects diagnostics; every rejected or clamped directive produces
	// exactly one, with file and line (05 §9).
	warn []Warning
	// seenReject remembers which unsupported directives have already been
	// warned about, so a unit with ten sandboxing directives produces ten
	// warnings but a drop-in that repeats one does not double up.
	seenReject map[string]bool
	// execStartLines counts ExecStart= assignments for the multiplicity check.
	execStartLines int
}

// NewParser starts a parse for the named unit.
func NewParser(name string, ctx SpecifierContext) *Parser {
	u := NewUnit(name)
	ctx.UnitName = name
	ctx.Prefix, ctx.Instance = SplitTemplate(name)
	return &Parser{unit: u, ctx: ctx, seenReject: map[string]bool{}}
}

// Apply folds one fragment (a unit file or a drop-in) into the unit.
func (p *Parser) Apply(f *Fragment) {
	p.warn = append(p.warn, f.Warnings...)
	for _, a := range f.Assignments {
		p.assign(a)
	}
}

// Finish validates cross-directive constraints and returns the unit.
func (p *Parser) Finish() *Unit {
	u := p.unit
	if svc := u.Service; svc != nil {
		// Exactly one ExecStart= is allowed except for oneshot, where the
		// lines run sequentially. v0.5.x started several lines in parallel
		// (defect C10).
		if svc.Type != TypeOneshot && p.execStartLines > 1 {
			p.warnf(Warning{
				Directive: "ExecStart",
				Reason:    fmt.Sprintf("ExecStart= may only be specified once for Type=%s", svc.Type),
				Action:    "unit failed to load",
			})
			u.LoadState = LoadBadSetting
			if u.LoadError == "" {
				u.LoadError = fmt.Sprintf("ExecStart= may only be specified once for Type=%s", svc.Type)
			}
		}
		if svc.Type == TypeDBus {
			p.rejectOnce("Type", "dbus", "no D-Bus in a container; treated as Type=simple")
		}
		if svc.BusName != "" {
			p.rejectOnce("BusName", svc.BusName, "no D-Bus in a container")
		}
		if svc.RestartKillSignal == "" {
			svc.RestartKillSignal = svc.KillSignal
		}
		if len(svc.ExecStart) == 0 && svc.Type != TypeOneshot && u.LoadState == LoadLoaded {
			p.warnf(Warning{
				Directive: "ExecStart",
				Reason:    "service has no ExecStart= and is not Type=oneshot",
				Action:    "unit failed to load",
			})
			u.LoadState = LoadBadSetting
			if u.LoadError == "" {
				u.LoadError = "service has no ExecStart= setting"
			}
		}
	}
	u.Warnings = append(u.Warnings, p.warn...)
	for i := range u.Warnings {
		u.Warnings[i].Unit = u.Name
	}
	return u
}

func (p *Parser) warnf(w Warning) {
	w.Unit = p.unit.Name
	p.warn = append(p.warn, w)
}

func (p *Parser) rejectOnce(directive, value, reason string) {
	if p.seenReject[directive] {
		return
	}
	p.seenReject[directive] = true
	p.warnf(Warning{Directive: directive, Value: value, Reason: reason, Action: "ignored"})
}

// assign dispatches one assignment.
func (p *Parser) assign(a Assignment) {
	// Expand specifiers in every value systemd expands them in — that is,
	// everything except a handful of literal-only directives (defect C7).
	if !literalDirectives[a.Key] {
		a.Value = p.ctx.ExpandSpecifiers(a.Value)
	}
	switch a.Section {
	case "Unit":
		p.assignUnit(a)
	case "Service":
		if p.unit.Service == nil {
			// A [Service] section in a .target file: skip, and say so once.
			p.rejectOnce("[Service]", "", fmt.Sprintf("a %s unit has no [Service] section", p.unit.Kind()))
			return
		}
		p.assignService(a)
	case "Install":
		p.assignInstall(a)
	default:
		// Unknown sections are skipped entirely along with their contents.
	}
}

// literalDirectives are the values in which a `%` is not a specifier.
var literalDirectives = map[string]bool{
	"Documentation": true,
}

func (p *Parser) bad(a Assignment, err error) {
	p.warnf(Warning{
		File: a.File, Line: a.Line, Directive: a.Key, Value: a.Value,
		Reason: err.Error(), Action: "ignored",
	})
}

// setBool parses a boolean, warning on a bad value rather than silently
// treating it as false.
func (p *Parser) setBool(a Assignment, dst *bool, def bool) {
	if a.Value == "" {
		*dst = def
		return
	}
	v, err := ParseBool(a.Value)
	if err != nil {
		p.bad(a, err)
		return
	}
	*dst = v
}

func (p *Parser) setDuration(a Assignment, dst *time.Duration, def time.Duration) {
	if a.Value == "" {
		*dst = def
		return
	}
	v, err := ParseDuration(a.Value)
	if err != nil {
		p.bad(a, err)
		return
	}
	*dst = v
}

// appendList implements systemd's list semantics: an empty assignment resets
// the accumulated list, which is how a drop-in subtracts (05 §2).
func appendList(dst *[]string, value string) {
	if strings.TrimSpace(value) == "" {
		*dst = nil
		return
	}
	*dst = append(*dst, strings.Fields(value)...)
}

// appendUnitList is appendList with unit-name canonicalisation applied.
func appendUnitList(dst *[]string, value string) {
	if strings.TrimSpace(value) == "" {
		*dst = nil
		return
	}
	for _, f := range strings.Fields(value) {
		*dst = append(*dst, CanonicalName(f))
	}
}

// CanonicalName appends the implicit `.service` suffix to a bare unit name.
// Internally every name carries its suffix; the CLI adds a missing one on
// input and may hide it on output.
func CanonicalName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		switch name[i:] {
		case ".service", ".target", ".socket", ".timer", ".path", ".mount",
			".automount", ".swap", ".slice", ".scope", ".device":
			return name
		}
	}
	return name + ".service"
}

func (p *Parser) assignUnit(a Assignment) {
	u := &p.unit.Unit
	switch a.Key {
	case "Description":
		u.Description = a.Value
	case "Documentation":
		appendList(&u.Documentation, a.Value)
	case "Wants":
		appendUnitList(&u.Wants, a.Value)
	case "Requires":
		appendUnitList(&u.Requires, a.Value)
	case "Requisite", "RequisiteOverridable":
		appendUnitList(&u.Requisite, a.Value)
	case "BindsTo":
		appendUnitList(&u.BindsTo, a.Value)
	case "PartOf":
		appendUnitList(&u.PartOf, a.Value)
	case "Upholds":
		appendUnitList(&u.Upholds, a.Value)
	case "Conflicts":
		appendUnitList(&u.Conflicts, a.Value)
	case "WantedBy":
		appendUnitList(&u.WantedBy, a.Value)
	case "RequiredBy":
		appendUnitList(&u.RequiredBy, a.Value)
	case "UpheldBy":
		appendUnitList(&u.UpheldBy, a.Value)
	case "Before":
		appendUnitList(&u.Before, a.Value)
	case "After":
		appendUnitList(&u.After, a.Value)
	case "OnFailure":
		appendUnitList(&u.OnFailure, a.Value)
	case "OnSuccess":
		appendUnitList(&u.OnSuccess, a.Value)
	case "OnFailureJobMode":
		u.OnFailureJobMode = a.Value
	case "StopWhenUnneeded":
		p.setBool(a, &u.StopWhenUnneeded, false)
	case "RefuseManualStart":
		p.setBool(a, &u.RefuseManualStart, false)
	case "RefuseManualStop":
		p.setBool(a, &u.RefuseManualStop, false)
	case "AllowIsolate":
		p.setBool(a, &u.AllowIsolate, false)
	case "IgnoreOnIsolate":
		p.setBool(a, &u.IgnoreOnIsolate, false)
	case "JobTimeoutSec":
		p.setDuration(a, &u.JobTimeoutSec, 0)
	case "JobRunningTimeoutSec":
		p.setDuration(a, &u.JobRunningTimeoutSec, 0)
	case "StartLimitIntervalSec", "StartLimitInterval":
		p.setDuration(a, &u.StartLimitInterval, 10*time.Second)
	case "StartLimitBurst":
		n, err := ParseIntRange(a.Value, 0, 1<<20)
		if err != nil {
			p.bad(a, err)
			return
		}
		u.StartLimitBurst = n
	case "StartLimitAction":
		u.StartLimitAction = a.Value
	case "FailureAction":
		u.FailureAction = p.checkAction(a)
	case "SuccessAction":
		u.SuccessAction = p.checkAction(a)
	case "DefaultDependencies":
		p.setBool(a, &u.DefaultDependencies, true)
	case "SourcePath":
		u.SourcePath = a.Value
	case "AssertPathExists", "AssertPathIsDirectory", "AssertFileNotEmpty",
		"AssertDirectoryNotEmpty", "AssertPathIsSymbolicLink", "AssertPathIsMountPoint",
		"AssertFileIsExecutable", "AssertKernelVersion", "AssertArchitecture",
		"AssertHost", "AssertEnvironment", "AssertUser", "AssertGroup",
		"AssertVirtualization", "AssertCapability", "AssertSecurity",
		"AssertACPower", "AssertMemory", "AssertCPUs", "AssertFirstBoot",
		"AssertNeedsUpdate", "AssertControlGroupController", "AssertKernelCommandLine":
		p.addCondition(a, strings.TrimPrefix(a.Key, "Assert"), true)
	default:
		if strings.HasPrefix(a.Key, "Condition") {
			p.addCondition(a, strings.TrimPrefix(a.Key, "Condition"), false)
			return
		}
		p.unknownKey(a)
	}
}

func (p *Parser) checkAction(a Assignment) string {
	switch a.Value {
	case "", "none", "poweroff", "poweroff-force", "poweroff-immediate",
		"exit", "exit-force", "reboot", "reboot-force", "reboot-immediate",
		"halt", "halt-force", "halt-immediate":
		if a.Value == "" {
			return "none"
		}
		return a.Value
	default:
		p.warnf(Warning{
			File: a.File, Line: a.Line, Directive: a.Key, Value: a.Value,
			Reason: "unsupported action", Action: "treated as none",
		})
		return "none"
	}
}

func (p *Parser) addCondition(a Assignment, kind string, assert bool) {
	if a.Value == "" {
		// An empty assignment resets the accumulated condition list.
		var kept []Condition
		for _, c := range p.unit.Unit.Conditions {
			if c.Kind != kind || c.Assert != assert {
				kept = append(kept, c)
			}
		}
		p.unit.Unit.Conditions = kept
		return
	}
	v := a.Value
	neg := false
	if strings.HasPrefix(v, "!") {
		neg = true
		v = strings.TrimSpace(v[1:])
	}
	p.unit.Unit.Conditions = append(p.unit.Unit.Conditions, Condition{
		Kind: kind, Value: v, Negate: neg, Assert: assert,
	})
}

func (p *Parser) unknownKey(a Assignment) {
	if reason, ok := unsupportedDirectives[a.Key]; ok {
		// A directive we know about but cannot honour. Silence here would be
		// worse than a warning: operators assume hardening directives took
		// effect (05 §8).
		if !p.seenReject[a.Key] {
			p.seenReject[a.Key] = true
			p.warnf(Warning{
				File: a.File, Line: a.Line, Directive: a.Key, Value: a.Value,
				Reason: reason, Action: "ignored",
			})
		}
		return
	}
	if ignoredDirectives[a.Key] {
		return
	}
	p.warnf(Warning{
		File: a.File, Line: a.Line, Directive: a.Key, Value: a.Value,
		Reason: "unknown directive", Action: "ignored",
	})
}

func (p *Parser) assignService(a Assignment) {
	s := p.unit.Service
	switch a.Key {
	case "Type":
		switch a.Value {
		case "", TypeSimple, TypeExec, TypeForking, TypeOneshot, TypeIdle,
			TypeNotify, TypeNotifyReload, TypeDBus:
			if a.Value == "" {
				s.Type = TypeSimple
			} else {
				s.Type = a.Value
			}
		default:
			p.bad(a, fmt.Errorf("unknown service type %q", a.Value))
		}
	case "ExitType":
		s.ExitType = a.Value
	case "RemainAfterExit":
		p.setBool(a, &s.RemainAfterExit, false)
	case "GuessMainPID":
		p.setBool(a, &s.GuessMainPID, true)
	case "PIDFile":
		s.PIDFile = a.Value
	case "BusName":
		s.BusName = a.Value
	case "ExecStart":
		if a.Value == "" {
			s.ExecStart = nil
			p.execStartLines = 0
			return
		}
		p.execStartLines++
		p.addCommand(a, &s.ExecStart)
	case "ExecStartPre":
		p.addCommand(a, &s.ExecStartPre)
	case "ExecStartPost":
		p.addCommand(a, &s.ExecStartPost)
	case "ExecCondition":
		p.addCommand(a, &s.ExecCondition)
	case "ExecReload":
		p.addCommand(a, &s.ExecReload)
	case "ExecStop":
		p.addCommand(a, &s.ExecStop)
	case "ExecStopPost":
		p.addCommand(a, &s.ExecStopPost)
	case "ExecStopPre":
		// Not a systemd directive; it was invented by v0.5.x (defect C9).
		// Accepted with a deprecation warning for one release, mapped onto the
		// nearest real thing.
		p.warnf(Warning{
			File: a.File, Line: a.Line, Directive: a.Key, Value: a.Value,
			Reason: "ExecStopPre= is not a systemd directive and is deprecated; use ExecStop=",
			Action: "treated as ExecStop=",
		})
		p.addCommand(a, &s.ExecStop)
	case "Restart":
		switch a.Value {
		case "", RestartNo, "none", RestartAlways, RestartOnSuccess, RestartOnFailure,
			RestartOnAbnormal, RestartOnAbort, RestartOnWatchdog:
			if a.Value == "" || a.Value == "none" {
				s.Restart = RestartNo
			} else {
				s.Restart = a.Value
			}
		default:
			p.bad(a, fmt.Errorf("unknown Restart= policy %q", a.Value))
		}
	case "RestartSec":
		p.setDuration(a, &s.RestartSec, 100*time.Millisecond)
	case "RestartPreventExitStatus":
		p.addExitStatus(a, &s.RestartPreventExitStatus)
	case "RestartForceExitStatus":
		p.addExitStatus(a, &s.RestartForceExitStatus)
	case "SuccessExitStatus":
		p.addExitStatus(a, &s.SuccessExitStatus)
	case "TimeoutStartSec":
		p.setDuration(a, &s.TimeoutStartSec, 90*time.Second)
	case "TimeoutStopSec":
		p.setDuration(a, &s.TimeoutStopSec, 90*time.Second)
	case "TimeoutAbortSec":
		p.setDuration(a, &s.TimeoutAbortSec, 0)
	case "TimeoutSec":
		var d time.Duration
		p.setDuration(a, &d, 90*time.Second)
		s.TimeoutStartSec, s.TimeoutStopSec = d, d
	case "RuntimeMaxSec":
		p.setDuration(a, &s.RuntimeMaxSec, 0)
	case "WatchdogSec", "RuntimeWatchdogSec":
		p.setDuration(a, &s.WatchdogSec, 0)
	case "KillMode":
		switch a.Value {
		case "", KillControlGroup, KillMixed, KillProcess, KillNone:
			if a.Value == "" {
				s.KillMode = KillControlGroup
			} else {
				s.KillMode = a.Value
			}
		default:
			p.bad(a, fmt.Errorf("unknown KillMode=%q", a.Value))
		}
	case "KillSignal":
		p.setSignal(a, &s.KillSignal, "SIGTERM")
	case "RestartKillSignal":
		p.setSignal(a, &s.RestartKillSignal, "")
	case "FinalKillSignal":
		p.setSignal(a, &s.FinalKillSignal, "SIGKILL")
	case "SendSIGHUP":
		p.setBool(a, &s.SendSIGHUP, false)
	case "SendSIGKILL":
		p.setBool(a, &s.SendSIGKILL, true)
	case "User":
		s.User = a.Value
	case "Group":
		s.Group = a.Value
	case "SupplementaryGroups":
		appendList(&s.SupplementaryGroups, a.Value)
	case "Environment":
		if a.Value == "" {
			s.Environment = nil
			return
		}
		assigns, err := ParseEnvironmentAssignments(a.Value)
		if err != nil {
			p.bad(a, err)
			return
		}
		s.Environment = append(s.Environment, assigns...)
	case "EnvironmentFile":
		if a.Value == "" {
			s.EnvironmentFiles = nil
			return
		}
		s.EnvironmentFiles = append(s.EnvironmentFiles, ParseEnvFileRef(a.Value))
	case "PassEnvironment":
		appendList(&s.PassEnvironment, a.Value)
	case "UnsetEnvironment":
		appendList(&s.UnsetEnvironment, a.Value)
	case "WorkingDirectory":
		v := a.Value
		s.WorkingDirOptional = strings.HasPrefix(v, "-")
		s.WorkingDirectory = strings.TrimPrefix(v, "-")
	case "UMask":
		m, err := ParseMode(a.Value)
		if err != nil {
			p.bad(a, err)
			return
		}
		s.UMask = &m
	case "Nice":
		n, err := ParseIntRange(a.Value, -20, 19)
		if err != nil {
			p.bad(a, err)
			return
		}
		s.Nice = &n
	case "OOMScoreAdjust":
		n, err := ParseIntRange(a.Value, -1000, 1000)
		if err != nil {
			p.bad(a, err)
			return
		}
		s.OOMScoreAdjust = &n
	case "NoNewPrivileges":
		p.setBool(a, &s.NoNewPrivileges, false)
	case "RuntimeDirectory":
		appendList(&s.RuntimeDirectory, a.Value)
	case "StateDirectory":
		appendList(&s.StateDirectory, a.Value)
	case "CacheDirectory":
		appendList(&s.CacheDirectory, a.Value)
	case "LogsDirectory":
		appendList(&s.LogsDirectory, a.Value)
	case "ConfigurationDirectory":
		appendList(&s.ConfigurationDirectory, a.Value)
	case "RuntimeDirectoryMode":
		p.setMode(a, &s.RuntimeDirectoryMode)
	case "StateDirectoryMode":
		p.setMode(a, &s.StateDirectoryMode)
	case "CacheDirectoryMode":
		p.setMode(a, &s.CacheDirectoryMode)
	case "LogsDirectoryMode":
		p.setMode(a, &s.LogsDirectoryMode)
	case "ConfigurationDirectoryMode":
		p.setMode(a, &s.ConfigurationDirMode)
	case "RuntimeDirectoryPreserve":
		s.RuntimeDirectoryPreserve = a.Value
	case "StandardInput":
		s.StandardInput = a.Value
	case "StandardOutput":
		s.StandardOutput = p.checkStdio(a)
	case "StandardError":
		s.StandardError = p.checkStdio(a)
	case "SyslogIdentifier":
		s.SyslogIdentifier = a.Value
	case "SyslogLevel":
		if n, ok := syslogLevels[strings.ToLower(a.Value)]; ok {
			s.SyslogLevel = n
			return
		}
		n, err := ParseIntRange(a.Value, 0, 7)
		if err != nil {
			p.bad(a, err)
			return
		}
		s.SyslogLevel = n
	case "SyslogLevelPrefix":
		p.setBool(a, &s.SyslogLevelPrefix, true)
	default:
		if strings.HasPrefix(a.Key, "Limit") {
			p.setLimit(a)
			return
		}
		p.unknownKey(a)
	}
}

var syslogLevels = map[string]int{
	"emerg": 0, "alert": 1, "crit": 2, "err": 3,
	"warning": 4, "notice": 5, "info": 6, "debug": 7,
}

func (p *Parser) checkStdio(a Assignment) string {
	v := a.Value
	switch {
	case v == "", v == "journal", v == "journal+console", v == "inherit",
		v == "null", v == "tty", v == "syslog", v == "kmsg":
		if v == "" {
			return "journal"
		}
		if v == "syslog" || v == "kmsg" {
			return "journal"
		}
		return v
	case strings.HasPrefix(v, "file:"), strings.HasPrefix(v, "append:"):
		return v
	default:
		p.warnf(Warning{
			File: a.File, Line: a.Line, Directive: a.Key, Value: v,
			Reason: "unsupported stream target", Action: "treated as journal",
		})
		return "journal"
	}
}

func (p *Parser) setMode(a Assignment, dst *uint32) {
	m, err := ParseMode(a.Value)
	if err != nil {
		p.bad(a, err)
		return
	}
	*dst = m
}

func (p *Parser) setSignal(a Assignment, dst *string, def string) {
	if a.Value == "" {
		*dst = def
		return
	}
	if SignalNumber(a.Value) == 0 {
		p.bad(a, fmt.Errorf("unknown signal %q", a.Value))
		return
	}
	*dst = a.Value
}

func (p *Parser) addExitStatus(a Assignment, dst *ExitStatusSet) {
	if a.Value == "" {
		*dst = ExitStatusSet{}
		return
	}
	if err := ParseExitStatusInto(dst, a.Value); err != nil {
		p.bad(a, err)
	}
}

func (p *Parser) addCommand(a Assignment, dst *[]Command) {
	if a.Value == "" {
		*dst = nil
		return
	}
	cmd, err := ParseCommand(a.Value)
	if err != nil {
		p.bad(a, err)
		p.unit.LoadState = LoadBadSetting
		if p.unit.LoadError == "" {
			p.unit.LoadError = fmt.Sprintf("%s:%d: %s=: %v", a.File, a.Line, a.Key, err)
		}
		return
	}
	cmd.File, cmd.Line = a.File, a.Line
	*dst = append(*dst, cmd)
}

// rlimitNames maps the Limit*= suffix onto its RLIMIT_ constant.
var rlimitNames = map[string]int{
	"CPU": unix.RLIMIT_CPU, "FSIZE": unix.RLIMIT_FSIZE, "DATA": unix.RLIMIT_DATA,
	"STACK": unix.RLIMIT_STACK, "CORE": unix.RLIMIT_CORE, "RSS": unix.RLIMIT_RSS,
	"NOFILE": unix.RLIMIT_NOFILE, "AS": unix.RLIMIT_AS, "NPROC": unix.RLIMIT_NPROC,
	"MEMLOCK": unix.RLIMIT_MEMLOCK, "LOCKS": 10, "SIGPENDING": 11,
	"MSGQUEUE": 12, "NICE": 13, "RTPRIO": 14, "RTTIME": 15,
}

// RLimitResource resolves a Limit*= suffix to its resource number.
func RLimitResource(name string) (int, bool) {
	r, ok := rlimitNames[strings.ToUpper(name)]
	return r, ok
}

// RLimitNames returns the supported Limit*= suffixes, sorted.
func RLimitNames() []string {
	out := make([]string, 0, len(rlimitNames))
	for k := range rlimitNames {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// setLimit parses a Limit*= directive. Contrary to v0.5.x, which warned and
// ignored all sixteen on the stated grounds that docker forbids them, only
// *raising a hard limit* needs CAP_SYS_RESOURCE; lowering, and raising a soft
// limit up to the inherited hard limit, always work. The clamping happens at
// spawn time (05 §7.4); here we only parse.
func (p *Parser) setLimit(a Assignment) {
	name := strings.TrimPrefix(a.Key, "Limit")
	if _, ok := rlimitNames[strings.ToUpper(name)]; !ok {
		p.unknownKey(a)
		return
	}
	if a.Value == "" {
		delete(p.unit.Service.Limits, strings.ToUpper(name))
		return
	}
	soft, hard, err := parseLimitValue(strings.ToUpper(name), a.Value)
	if err != nil {
		p.bad(a, err)
		return
	}
	if p.unit.Service.Limits == nil {
		p.unit.Service.Limits = map[string]RLimit{}
	}
	p.unit.Service.Limits[strings.ToUpper(name)] = RLimit{Soft: soft, Hard: hard}
}

// parseLimitValue accepts "N", "N:M" and "infinity", with time-valued and
// size-valued resources parsed in their own grammars.
func parseLimitValue(name, v string) (soft, hard uint64, err error) {
	parts := strings.SplitN(v, ":", 2)
	soft, err = parseOneLimit(name, parts[0])
	if err != nil {
		return 0, 0, err
	}
	hard = soft
	if len(parts) == 2 {
		hard, err = parseOneLimit(name, parts[1])
		if err != nil {
			return 0, 0, err
		}
	}
	return soft, hard, nil
}

func parseOneLimit(name, v string) (uint64, error) {
	v = strings.TrimSpace(v)
	if strings.EqualFold(v, "infinity") {
		return unix.RLIM_INFINITY, nil
	}
	switch name {
	case "CPU", "RTTIME":
		d, err := ParseDuration(v)
		if err != nil {
			return 0, err
		}
		if name == "CPU" {
			return uint64(d / time.Second), nil
		}
		return uint64(d / time.Microsecond), nil
	case "FSIZE", "DATA", "STACK", "CORE", "RSS", "AS", "MEMLOCK", "MSGQUEUE":
		n, err := ParseSize(v)
		if err != nil {
			return 0, err
		}
		return uint64(n), nil
	default:
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid limit %q", v)
		}
		return n, nil
	}
}

func (p *Parser) assignInstall(a Assignment) {
	i := &p.unit.Install
	switch a.Key {
	case "WantedBy":
		appendUnitList(&i.WantedBy, a.Value)
	case "RequiredBy":
		appendUnitList(&i.RequiredBy, a.Value)
	case "UpheldBy":
		appendUnitList(&i.UpheldBy, a.Value)
	case "Alias":
		appendList(&i.Alias, a.Value)
	case "Also":
		appendUnitList(&i.Also, a.Value)
	case "DefaultInstance":
		i.DefaultInstance = a.Value
	default:
		p.unknownKey(a)
	}
}

// unsupportedDirectives are directives we recognise but cannot honour in an
// unprivileged container. Each produces one warning per unit naming the
// directive and the reason.
var unsupportedDirectives = map[string]string{
	"PrivateTmp":               "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"PrivateDevices":           "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"PrivateNetwork":           "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"PrivateUsers":             "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"PrivateIPC":               "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"ProtectSystem":            "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"ProtectHome":              "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"ProtectKernelTunables":    "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"ProtectKernelModules":     "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"ProtectKernelLogs":        "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"ProtectControlGroups":     "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"ProtectClock":             "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"ProtectHostname":          "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"ProtectProc":              "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"ReadOnlyPaths":            "requires mount namespaces, not available in an unprivileged container",
	"ReadOnlyDirectories":      "requires mount namespaces, not available in an unprivileged container",
	"ReadWriteDirectories":     "requires mount namespaces, not available in an unprivileged container",
	"InaccessibleDirectories":  "requires mount namespaces, not available in an unprivileged container",
	"ReadWritePaths":           "requires mount namespaces, not available in an unprivileged container",
	"InaccessiblePaths":        "requires mount namespaces, not available in an unprivileged container",
	"BindPaths":                "requires mount namespaces, not available in an unprivileged container",
	"BindReadOnlyPaths":        "requires mount namespaces, not available in an unprivileged container",
	"TemporaryFileSystem":      "requires mount namespaces, not available in an unprivileged container",
	"RootDirectory":            "requires CAP_SYS_CHROOT, not available in an unprivileged container",
	"RootImage":                "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"RestrictAddressFamilies":  "requires a seccomp profile we cannot install",
	"RestrictNamespaces":       "requires a seccomp profile we cannot install",
	"RestrictRealtime":         "requires a seccomp profile we cannot install",
	"RestrictSUIDSGID":         "requires a seccomp profile we cannot install",
	"SystemCallFilter":         "requires a seccomp profile we cannot install",
	"SystemCallArchitectures":  "requires a seccomp profile we cannot install",
	"SystemCallErrorNumber":    "requires a seccomp profile we cannot install",
	"MemoryDenyWriteExecute":   "requires a seccomp profile we cannot install",
	"LockPersonality":          "requires a seccomp profile we cannot install",
	"CapabilityBoundingSet":    "requires CAP_SETPCAP, not available in an unprivileged container",
	"AmbientCapabilities":      "requires CAP_SETPCAP, not available in an unprivileged container",
	"MemoryMax":                "requires a writable cgroup hierarchy; use `docker run --memory`",
	"MemoryHigh":               "requires a writable cgroup hierarchy; use `docker run --memory`",
	"MemoryLow":                "requires a writable cgroup hierarchy; use `docker run --memory`",
	"MemoryMin":                "requires a writable cgroup hierarchy; use `docker run --memory`",
	"MemoryLimit":              "requires a writable cgroup hierarchy; use `docker run --memory`",
	"MemorySwapMax":            "requires a writable cgroup hierarchy; use `docker run --memory-swap`",
	"CPUQuota":                 "requires a writable cgroup hierarchy; use `docker run --cpus`",
	"CPUWeight":                "requires a writable cgroup hierarchy; use `docker run --cpu-shares`",
	"CPUShares":                "requires a writable cgroup hierarchy; use `docker run --cpu-shares`",
	"CPUAccounting":            "requires a writable cgroup hierarchy",
	"MemoryAccounting":         "requires a writable cgroup hierarchy",
	"IOAccounting":             "requires a writable cgroup hierarchy",
	"TasksAccounting":          "requires a writable cgroup hierarchy",
	"TasksMax":                 "requires a writable cgroup hierarchy; use `docker run --pids-limit`",
	"IOWeight":                 "requires a writable cgroup hierarchy",
	"IOReadBandwidthMax":       "requires a writable cgroup hierarchy; use `docker run --device-read-bps`",
	"IOWriteBandwidthMax":      "requires a writable cgroup hierarchy; use `docker run --device-write-bps`",
	"Slice":                    "requires a writable cgroup hierarchy",
	"Delegate":                 "requires a writable cgroup hierarchy",
	"OOMPolicy":                "requires a writable cgroup hierarchy",
	"DeviceAllow":              "requires a writable cgroup hierarchy",
	"DevicePolicy":             "requires a writable cgroup hierarchy",
	"CPUAffinity":              "requires CAP_SYS_NICE for other users' processes",
	"CPUSchedulingPolicy":      "requires CAP_SYS_NICE, not available in an unprivileged container",
	"CPUSchedulingPriority":    "requires CAP_SYS_NICE, not available in an unprivileged container",
	"CPUSchedulingResetOnFork": "requires CAP_SYS_NICE, not available in an unprivileged container",
	"IOSchedulingClass":        "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"IOSchedulingPriority":     "requires CAP_SYS_ADMIN, not available in an unprivileged container",
	"IPAddressAllow":           "requires BPF and CAP_BPF",
	"IPAddressDeny":            "requires BPF and CAP_BPF",
	"NUMAPolicy":               "not supported in a container",
	"KeyringMode":              "not supported",
	"UtmpIdentifier":           "no utmp in a container",
	"UtmpMode":                 "no utmp in a container",
	"PAMName":                  "no PAM integration",
	"DynamicUser":              "not supported; declare a real User=",
	"PermissionsStartOnly":     "deprecated in systemd; use the + prefix on Exec* lines",
}

// ignoredDirectives are recognised, silently accepted, and have no effect
// worth warning about.
var ignoredDirectives = map[string]bool{
	"Documentation":          true,
	"DefaultInstance":        true,
	"IgnoreSIGPIPE":          true,
	"NonBlocking":            true,
	"NotifyAccess":           true,
	"FileDescriptorStoreMax": true,
	"Sockets":                true,
	"TTYPath":                true,
	"TTYReset":               true,
	"TTYVHangup":             true,
	"TTYVTDisallocate":       true,
	"TTYColumns":             true,
	"TTYRows":                true,
	"StartLimitAction":       true,
	"RebootArgument":         true,
	"CollectMode":            true,
}
