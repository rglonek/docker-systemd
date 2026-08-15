package cli

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"docker-systemd/internal/proto"
	"docker-systemd/internal/unitfile"
)

// knownVerbs is the dispatch table. The `privileged` column is carried from day
// one so that per-verb authorisation policy (09 §3.2) is a small change later
// rather than a redesign.
type verbSpec struct {
	name       string
	minUnits   int
	privileged bool
	// takesArgs marks verbs whose operands are not unit names.
	takesArgs bool
}

var knownVerbs = map[string]verbSpec{
	"start":                 {name: "start", minUnits: 1, privileged: true},
	"stop":                  {name: "stop", minUnits: 1, privileged: true},
	"restart":               {name: "restart", minUnits: 1, privileged: true},
	"try-restart":           {name: "try-restart", minUnits: 1, privileged: true},
	"condrestart":           {name: "try-restart", minUnits: 1, privileged: true},
	"reload":                {name: "reload", minUnits: 1, privileged: true},
	"force-reload":          {name: "reload-or-restart", minUnits: 1, privileged: true},
	"reload-or-restart":     {name: "reload-or-restart", minUnits: 1, privileged: true},
	"try-reload-or-restart": {name: "try-reload-or-restart", minUnits: 1, privileged: true},
	"kill":                  {name: "kill", minUnits: 1, privileged: true},
	"isolate":               {name: "isolate", minUnits: 1, privileged: true},
	"status":                {name: "status"},
	"show":                  {name: "show"},
	"is-active":             {name: "is-active", minUnits: 1},
	"is-failed":             {name: "is-failed", minUnits: 1},
	"is-enabled":            {name: "is-enabled", minUnits: 1},
	"cat":                   {name: "cat", minUnits: 1},
	"list-units":            {name: "list-units"},
	"list":                  {name: "list-units"},
	"list-unit-files":       {name: "list-unit-files"},
	"list-dependencies":     {name: "list-dependencies"},
	"reset-failed":          {name: "reset-failed", privileged: true},
	"enable":                {name: "enable", minUnits: 1, privileged: true},
	"disable":               {name: "disable", minUnits: 1, privileged: true},
	"reenable":              {name: "reenable", minUnits: 1, privileged: true},
	"preset":                {name: "preset", minUnits: 1, privileged: true},
	"preset-all":            {name: "preset-all", privileged: true},
	"mask":                  {name: "mask", minUnits: 1, privileged: true},
	"unmask":                {name: "unmask", minUnits: 1, privileged: true},
	"link":                  {name: "link", minUnits: 1, privileged: true},
	"revert":                {name: "revert", minUnits: 1, privileged: true},
	"add-wants":             {name: "add-wants", minUnits: 1, privileged: true},
	"add-requires":          {name: "add-requires", minUnits: 1, privileged: true},
	"create-instance":       {name: "create-instance", minUnits: 1, privileged: true},
	"delete-instance":       {name: "delete-instance", minUnits: 1, privileged: true},
	"daemon-reload":         {name: "daemon-reload", privileged: true},
	"daemon-reexec":         {name: "daemon-reexec", privileged: true},
	"show-environment":      {name: "show-environment"},
	"set-environment":       {name: "set-environment", privileged: true, takesArgs: true},
	"unset-environment":     {name: "unset-environment", privileged: true, takesArgs: true},
	"import-environment":    {name: "import-environment", privileged: true, takesArgs: true},
	"poweroff":              {name: "poweroff", privileged: true},
	"halt":                  {name: "halt", privileged: true},
	"reboot":                {name: "reboot", privileged: true},
	"default":               {name: "default", privileged: true},
	"rescue":                {name: "rescue", privileged: true},
	"emergency":             {name: "emergency", privileged: true},
}

// options accepted and ignored, because maintainer scripts pass them and a
// parse failure would break unit registration.
var ignoredFlags = map[string]bool{
	"--system": true, "--no-reload": true, "--global": true, "--runtime": true,
	"--no-ask-password": true, "--no-pager": true, "--no-legend": true,
	"--plain": true, "--full": true, "-l": true, "--recursive": true,
	"--no-warn": true, "--legend": true,
}

type systemctlFlags struct {
	quiet      bool
	all        bool
	noBlock    bool
	now        bool
	force      bool
	value      bool
	noLegend   bool
	plain      bool
	types      []string
	states     []string
	properties []string
	signal     string
	killWho    string
	lines      int
	output     string
	root       string
	reverse    bool
}

// Systemctl is the `systemctl` entry point.
func Systemctl(args []string) int {
	var f systemctlFlags
	var positional []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, bool) {
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		switch {
		case a == "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)
		case a == "--version" || a == "-v":
			return runVersion()
		case a == "--help" || a == "-h":
			printSystemctlHelp()
			return 0
		case a == "--quiet" || a == "-q":
			f.quiet = true
		case a == "--all" || a == "-a":
			f.all = true
		case a == "--no-block":
			f.noBlock = true
		case a == "--now":
			f.now = true
		case a == "--force" || a == "-f":
			f.force = true
		case a == "--value":
			f.value = true
		case a == "--no-legend":
			f.noLegend = true
		case a == "--plain":
			f.plain = true
		case a == "--reverse":
			f.reverse = true
		case a == "--user":
			fmt.Fprintln(os.Stderr,
				"systemctl --user is not supported: this manager only runs a system instance.")
			return 1
		case strings.HasPrefix(a, "--type=") || a == "-t":
			v := strings.TrimPrefix(a, "--type=")
			if a == "-t" {
				var okv bool
				if v, okv = next(); !okv {
					return usageError("-t requires an argument")
				}
			}
			f.types = append(f.types, splitCommas(v)...)
		case strings.HasPrefix(a, "--state="):
			f.states = append(f.states, splitCommas(strings.TrimPrefix(a, "--state="))...)
		case strings.HasPrefix(a, "--property=") || a == "-p":
			v := strings.TrimPrefix(a, "--property=")
			if a == "-p" {
				var okv bool
				if v, okv = next(); !okv {
					return usageError("-p requires an argument")
				}
			}
			f.properties = append(f.properties, splitCommas(v)...)
		case strings.HasPrefix(a, "--signal=") || a == "-s":
			v := strings.TrimPrefix(a, "--signal=")
			if a == "-s" {
				var okv bool
				if v, okv = next(); !okv {
					return usageError("-s requires an argument")
				}
			}
			f.signal = v
		case strings.HasPrefix(a, "--kill-who=") || strings.HasPrefix(a, "--kill-whom="):
			f.killWho = a[strings.IndexByte(a, '=')+1:]
		case strings.HasPrefix(a, "--lines=") || a == "-n":
			v := strings.TrimPrefix(a, "--lines=")
			if a == "-n" {
				var okv bool
				if v, okv = next(); !okv {
					return usageError("-n requires an argument")
				}
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return usageError("--lines expects a number")
			}
			f.lines = n
		case strings.HasPrefix(a, "--output=") || a == "-o":
			v := strings.TrimPrefix(a, "--output=")
			if a == "-o" {
				var okv bool
				if v, okv = next(); !okv {
					return usageError("-o requires an argument")
				}
			}
			f.output = v
		case strings.HasPrefix(a, "--root="):
			f.root = strings.TrimPrefix(a, "--root=")
		case ignoredFlags[a]:
			// Accepted and ignored on purpose.
		case strings.HasPrefix(a, "-"):
			// Anything else must be a hard, visible failure: a silent no-op is
			// how a maintainer script's `systemctl start foo` appears to
			// succeed while doing nothing (defect D6).
			return usageError(fmt.Sprintf("Unknown option %s.", a))
		default:
			positional = append(positional, a)
		}
	}

	if len(positional) == 0 {
		positional = []string{"list-units"}
	}
	verbName := positional[0]
	operands := positional[1:]

	spec, known := knownVerbs[verbName]
	if !known {
		return usageError(fmt.Sprintf("Unknown operation %s.", verbName))
	}
	if len(operands) < spec.minUnits {
		return usageError(fmt.Sprintf("%s requires at least %d unit name(s).", verbName, spec.minUnits))
	}

	// `systemctl show --property=Capabilities` is the doctor command: it
	// prints the selected backend, the probe results, the runtime paths and
	// every warned-about directive, so a bug report can carry them in one
	// paste. It is a manager-level property, not a unit one.
	if spec.name == "show" && len(operands) == 0 && hasProperty(f.properties, "Capabilities") {
		return runRequest("systemctl", proto.Request{Verb: "capabilities"}, printCapabilities)
	}

	req := proto.Request{
		Verb: spec.name,
		Options: proto.RequestOptions{
			Now: f.now, NoBlock: f.noBlock, Quiet: f.quiet, All: f.all,
			Force: f.force, Types: f.types, States: f.states,
			Properties: f.properties, Value: f.value, Signal: f.signal,
			KillWho: f.killWho, Lines: f.lines, Root: f.root,
		},
	}
	if spec.takesArgs || spec.name == "link" {
		req.Args = operands
		if spec.name == "link" {
			req.Units = operands
		}
	} else if spec.name == "add-wants" || spec.name == "add-requires" {
		if len(operands) < 2 {
			return usageError(spec.name + " takes a target and at least one unit")
		}
		req.Units = []string{unitfile.CanonicalName(operands[0])}
		for _, u := range operands[1:] {
			req.Args = append(req.Args, unitfile.CanonicalName(u))
		}
	} else {
		for _, u := range operands {
			req.Units = append(req.Units, unitfile.CanonicalName(u))
		}
	}

	return runRequest("systemctl", req, func(resp *Response) int {
		return renderSystemctl(spec.name, req, resp, f)
	})
}

func hasProperty(list []string, name string) bool {
	for _, p := range list {
		if strings.EqualFold(p, name) {
			return true
		}
	}
	return false
}

// printCapabilities renders the doctor output.
func printCapabilities(resp *Response) int {
	var c proto.Capabilities
	if err := resp.DecodeData(&c); err != nil {
		fmt.Fprintf(os.Stderr, "systemctl: %v\n", err)
		return 1
	}
	fmt.Printf("Version:        %s\n", c.Version)
	fmt.Printf("Backend:        %s\n", c.Backend)
	fmt.Printf("RuntimeDir:     %s\n", c.RuntimeDir)
	fmt.Printf("ControlSocket:  %s\n", c.ControlSocket)
	fmt.Printf("LogDir:         %s\n", c.LogDir)
	probes := make([]string, 0, len(c.Probes))
	for k := range c.Probes {
		probes = append(probes, k)
	}
	sort.Strings(probes)
	fmt.Println("Probes:")
	for _, k := range probes {
		fmt.Printf("  %-12s %s\n", k+":", c.Probes[k])
	}
	if len(c.Warnings) > 0 {
		fmt.Println("Warnings:")
		for _, w := range c.Warnings {
			fmt.Printf("  %s\n", w)
		}
	}
	return 0
}

func splitCommas(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func usageError(msg string) int {
	fmt.Fprintln(os.Stderr, msg)
	fmt.Fprintln(os.Stderr, "Run 'systemctl --help' for a list of commands.")
	return 2
}

func runVersion() int {
	return runRequest("systemctl", proto.Request{Verb: "version"}, func(resp *Response) int {
		var v map[string]string
		if err := resp.DecodeData(&v); err == nil {
			fmt.Printf("docker-systemd %s\n", v["version"])
		}
		return 0
	})
}

// renderSystemctl formats the DATA payload for human or script consumption.
func renderSystemctl(verb string, req proto.Request, resp *Response, f systemctlFlags) int {
	switch verb {
	case "status":
		var units []proto.UnitStatus
		if err := resp.DecodeData(&units); err != nil {
			return 0
		}
		for i, u := range units {
			if i > 0 {
				fmt.Println()
			}
			printStatus(u)
		}
	case "show":
		var units []proto.UnitStatus
		if err := resp.DecodeData(&units); err != nil {
			return 0
		}
		for _, u := range units {
			printShow(u, f.properties, f.value)
		}
	case "is-active", "is-failed", "is-enabled":
		var values []string
		if err := resp.DecodeData(&values); err != nil {
			return 0
		}
		if !f.quiet {
			for _, v := range values {
				fmt.Println(v)
			}
		}
	case "cat":
		var d map[string]string
		if err := resp.DecodeData(&d); err == nil {
			fmt.Print(d["text"])
		}
	case "list-units":
		var units []proto.UnitStatus
		if err := resp.DecodeData(&units); err != nil {
			return 0
		}
		printUnitList(units, f.noLegend)
	case "list-unit-files":
		var files []proto.UnitFileInfo
		if err := resp.DecodeData(&files); err != nil {
			return 0
		}
		printUnitFileList(files, f.noLegend)
	case "list-dependencies":
		var deps map[string][]string
		if err := resp.DecodeData(&deps); err != nil {
			return 0
		}
		printDependencies(deps, req.Units)
	case "show-environment":
		var env []string
		if err := resp.DecodeData(&env); err == nil {
			for _, e := range env {
				fmt.Println(e)
			}
		}
	case "enable", "disable", "reenable", "preset", "preset-all", "mask", "unmask",
		"link", "revert", "add-wants", "add-requires", "create-instance", "delete-instance":
		// The manager already streamed each change as a PROGRESS frame, which
		// honours --quiet; printing the DATA copy too would double every line.
	}
	return 0
}

// printStatus renders the systemctl status block.
func printStatus(u proto.UnitStatus) {
	bullet := "●"
	switch u.ActiveState {
	case proto.StateFailed:
		bullet = "×"
	case proto.StateInactive:
		bullet = "○"
	}
	desc := u.Description
	if desc == "" {
		desc = u.Name
	}
	fmt.Printf("%s %s - %s\n", bullet, u.Name, desc)

	loaded := u.LoadState
	if u.FragmentPath != "" {
		loaded = fmt.Sprintf("%s (%s; %s)", u.LoadState, u.FragmentPath, u.UnitFileState)
	}
	fmt.Printf("     Loaded: %s\n", loaded)
	if u.LoadError != "" {
		fmt.Printf("             Error: %s\n", u.LoadError)
	}
	for _, d := range u.DropInPaths {
		fmt.Printf("    Drop-In: %s\n", d)
	}

	active := u.ActiveState
	if u.SubState != "" {
		active = fmt.Sprintf("%s (%s)", u.ActiveState, u.SubState)
	}
	if u.Since != "" {
		if t, err := time.Parse(time.RFC3339Nano, u.Since); err == nil {
			active += fmt.Sprintf(" since %s; %s ago",
				t.Format("Mon 2006-01-02 15:04:05 MST"),
				time.Since(t).Round(time.Second))
		}
	}
	fmt.Printf("     Active: %s\n", active)
	if u.StatusText != "" {
		fmt.Printf("     Status: %q\n", u.StatusText)
	}
	if u.MainPID > 0 {
		name := ""
		for _, p := range u.Processes {
			if p.PID == u.MainPID {
				name = " (" + firstWord(p.Cmdline) + ")"
			}
		}
		fmt.Printf("   Main PID: %d%s\n", u.MainPID, name)
	}
	if u.Tasks > 0 {
		fmt.Printf("      Tasks: %d\n", u.Tasks)
	}
	fmt.Printf("     CGroup: (unavailable: unprivileged container; using subreaper tracking)\n")
	for i, p := range u.Processes {
		prefix := "├─"
		if i == len(u.Processes)-1 {
			prefix = "└─"
		}
		fmt.Printf("             %s%d %s\n", prefix, p.PID, p.Cmdline)
	}
	for _, w := range u.Warnings {
		fmt.Printf("    Warning: %s\n", w)
	}
}

func firstWord(s string) string {
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}

// showProperties builds the property map for `systemctl show`.
func showProperties(u proto.UnitStatus) map[string]string {
	props := map[string]string{
		"Id":                     u.Name,
		"Names":                  strings.Join(u.Names, " "),
		"Description":            u.Description,
		"LoadState":              u.LoadState,
		"LoadError":              u.LoadError,
		"ActiveState":            u.ActiveState,
		"SubState":               u.SubState,
		"UnitFileState":          u.UnitFileState,
		"MainPID":                strconv.Itoa(u.MainPID),
		"ExecMainPID":            strconv.Itoa(u.ExecMainPID),
		"ExecMainStatus":         strconv.Itoa(u.ExecMainStatus),
		"ExecMainStartTimestamp": u.Since,
		"Result":                 u.Result,
		"FragmentPath":           u.FragmentPath,
		"DropInPaths":            strings.Join(u.DropInPaths, " "),
		"Type":                   u.Type,
		"Restart":                u.Restart,
		"NRestarts":              strconv.Itoa(u.NRestarts),
		"InvocationID":           u.InvocationID,
		"StatusText":             u.StatusText,
		"ConditionResult":        boolYes(u.ConditionOK),
		"TasksCurrent":           strconv.Itoa(u.Tasks),
	}
	for k, v := range u.Deps {
		props[k] = v
	}
	return props
}

func boolYes(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func printShow(u proto.UnitStatus, want []string, valueOnly bool) {
	props := showProperties(u)
	if len(want) == 0 {
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%s=%s\n", k, props[k])
		}
		return
	}
	for _, k := range want {
		v := props[k]
		if valueOnly {
			fmt.Println(v)
		} else {
			fmt.Printf("%s=%s\n", k, v)
		}
	}
}

func printUnitList(units []proto.UnitStatus, noLegend bool) {
	if !noLegend {
		fmt.Printf("%-40s %-10s %-10s %-10s %s\n", "UNIT", "LOAD", "ACTIVE", "SUB", "DESCRIPTION")
	}
	for _, u := range units {
		fmt.Printf("%-40s %-10s %-10s %-10s %s\n",
			u.Name, u.LoadState, u.ActiveState, u.SubState, u.Description)
	}
	if !noLegend {
		fmt.Printf("\n%d units listed.\n", len(units))
	}
}

func printUnitFileList(files []proto.UnitFileInfo, noLegend bool) {
	if !noLegend {
		fmt.Printf("%-40s %s\n", "UNIT FILE", "STATE")
	}
	for _, f := range files {
		fmt.Printf("%-40s %s\n", f.Name, f.State)
	}
	if !noLegend {
		fmt.Printf("\n%d unit files listed.\n", len(files))
	}
}

func printDependencies(deps map[string][]string, roots []string) {
	seen := map[string]bool{}
	var walk func(string, string)
	walk = func(name, indent string) {
		if seen[name] {
			return
		}
		seen[name] = true
		fmt.Printf("%s%s\n", indent, name)
		for _, d := range deps[name] {
			walk(d, indent+"  ")
		}
	}
	for _, r := range roots {
		walk(r, "")
	}
	if len(roots) == 0 {
		keys := make([]string, 0, len(deps))
		for k := range deps {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walk(k, "")
		}
	}
}

func printSystemctlHelp() {
	fmt.Print(`systemctl [OPTIONS...] COMMAND [UNIT...]

Query or send control commands to the docker-systemd manager.

Unit commands:
  start NAME...                 Start (activate) one or more units
  stop NAME...                  Stop (deactivate) one or more units
  restart NAME...               Start or restart one or more units
  try-restart NAME...           Restart one or more units if active
  reload NAME...                Reload one or more units
  reload-or-restart NAME...     Reload one or more units if possible, else restart
  kill NAME...                  Send a signal to a unit's processes
  isolate NAME                  Start a unit and stop all others
  status [NAME...]              Show runtime status of one or more units
  show [NAME...]                Show properties of one or more units
  cat NAME...                   Show the unit file and its drop-ins
  is-active NAME...             Check whether units are active
  is-failed NAME...             Check whether units are failed
  is-enabled NAME...            Check whether unit files are enabled
  list-units                    List loaded units
  list-unit-files               List installed unit files
  list-dependencies [NAME]      Show a unit's dependency tree
  reset-failed [NAME...]        Reset failed state

Unit file commands:
  enable NAME...                Enable unit files
  disable NAME...               Disable unit files
  reenable NAME...              Re-enable unit files
  preset NAME...                Apply the enablement preset
  preset-all                    Apply presets to every unit file
  mask NAME...                  Mask unit files
  unmask NAME...                Unmask unit files
  link PATH...                  Link a unit file into the search path
  revert NAME...                Revert to the vendor unit file
  add-wants TARGET NAME...      Add a Wants= dependency
  add-requires TARGET NAME...   Add a Requires= dependency

Manager commands:
  daemon-reload                 Reload unit files
  daemon-reexec                 Re-execute the manager
  show-environment              Show the manager environment block
  set-environment VAR=VALUE...  Set manager environment variables
  unset-environment VAR...      Unset manager environment variables
  import-environment VAR...     Import variables from the caller
  poweroff | halt | reboot      Stop the container
  default | rescue | emergency  Isolate the named target

Options:
  -q --quiet                    Suppress informational output
  -a --all                      Show all units, including inactive ones
  -t --type=TYPE                Filter by unit type
     --state=STATE              Filter by unit state
  -p --property=NAME            Show only the named properties
     --value                    Print only property values
     --now                      Also start/stop when enabling/disabling
  -s --signal=SIG               Signal for kill
     --no-block                 Do not wait for the operation to finish
     --no-legend                Omit table headers
  -v --version                  Print the version
`)
}
