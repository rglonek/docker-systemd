// Command docker-systemd is a single static binary that acts as PID 1 inside a
// container and behaves enough like systemd that a stock distro image works
// like a VM.
//
// The binary multiplexes on argv[0] (06 §1), with getpid()==1 forcing manager
// mode regardless of the name it was invoked under.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"docker-systemd/internal/backend"
	"docker-systemd/internal/cli"
	"docker-systemd/internal/journal"
	"docker-systemd/internal/logging"
	"docker-systemd/internal/manager"
	"docker-systemd/internal/supervisor"
	"docker-systemd/internal/unitfile"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() { os.Exit(run()) }

func run() int {
	args := os.Args[1:]

	// The internal re-exec modes are selected by an explicit flag rather than
	// by argv[0], so they cannot be reached by naming a symlink badly.
	for _, a := range args {
		switch a {
		case "--supervise":
			return supervisor.Run()
		case "--exec-helper":
			return supervisor.RunExecHelper()
		}
	}

	name := filepath.Base(os.Args[0])
	// Anything, when pid == 1, is the manager.
	if os.Getpid() == 1 {
		return runManager(args)
	}

	switch name {
	case "systemctl":
		return cli.Systemctl(args)
	case "journalctl":
		return cli.Journalctl(args)
	case "service":
		return cli.Service(args)
	case "poweroff", "halt", "reboot", "shutdown", "telinit", "runlevel":
		return cli.Power(name, args)
	case "systemd-notify":
		return cli.Notify(args)
	case "systemd-detect-virt":
		return cli.DetectVirt(args)
	case "init", "systemd", "init-docker-systemd", "docker-systemd":
		return runManager(args)
	}

	// An unrecognised name still allows the subcommand form, which is what the
	// protocol-mismatch message tells users to fall back to.
	if len(args) > 0 {
		switch args[0] {
		case "systemctl":
			return cli.Systemctl(args[1:])
		case "journalctl":
			return cli.Journalctl(args[1:])
		case "service":
			return cli.Service(args[1:])
		case "systemd-notify":
			return cli.Notify(args[1:])
		case "systemd-detect-virt":
			return cli.DetectVirt(args[1:])
		case "poweroff", "halt", "reboot", "shutdown", "telinit", "runlevel":
			return cli.Power(args[0], args[1:])
		}
	}
	return runManager(args)
}

// runManager parses the manager flags of 04 §11 and boots.
func runManager(args []string) int {
	opts := manager.DefaultOptions()
	opts.Version = version

	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func(prefix string) (string, bool) {
			if strings.HasPrefix(a, prefix+"=") {
				return strings.TrimPrefix(a, prefix+"="), true
			}
			return "", false
		}
		switch {
		case a == "--help" || a == "-h":
			printManagerHelp()
			return 0
		case a == "--version" || a == "-v":
			fmt.Printf("docker-systemd %s\n", version)
			return 0
		case a == "--log-to-stderr":
			opts.LogToStderr = true
		case strings.HasPrefix(a, "--log-to-stderr="):
			opts.LogToStderr = true
			opts.LogToStderrUnits = splitCommaUnits(strings.TrimPrefix(a, "--log-to-stderr="))
		case a == "--no-logfile":
			opts.NoLogfile = true
		case a == "--no-install":
			opts.NoInstall = true
		case a == "--no-auto-reload":
			opts.NoAutoReload = true
		case a == "--compat-tmp-socket":
			opts.CompatTmpSocket = true
		case a == "--compat-shell-exec":
			opts.CompatShellExec = true
		case a == "--debug-reaper":
			// Folded into --log-level=trace; accepted for one release.
			opts.LogLevel = logging.LevelTrace
		case a == "--no-pidtrack":
			// The LD_PRELOAD subsystem is gone (ADR-1); the flag is accepted
			// and ignored with a deprecation warning for one release.
			fmt.Fprintln(os.Stderr,
				"docker-systemd: --no-pidtrack is obsolete and ignored: "+
					"process tracking now uses subreaper adoption, which needs no injection.")
		default:
			if v, ok := val("--log-level"); ok {
				lv, err := logging.ParseLevel(v)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return 2
				}
				opts.LogLevel = lv
				continue
			}
			if v, ok := val("--default-target"); ok {
				opts.DefaultTarget = unitfile.CanonicalName(v)
				continue
			}
			if v, ok := val("--shutdown-timeout"); ok {
				d, err := unitfile.ParseDuration(v)
				if err != nil {
					fmt.Fprintf(os.Stderr, "--shutdown-timeout: %v\n", err)
					return 2
				}
				opts.ShutdownTimeout = d
				continue
			}
			if v, ok := val("--backend"); ok {
				switch backend.Name(v) {
				case backend.Auto, backend.Cgroup2, backend.Subreaper, backend.Degraded:
					opts.Backend = backend.Name(v)
				default:
					fmt.Fprintf(os.Stderr,
						"--backend: expected auto, cgroup2, subreaper or degraded, got %q\n", v)
					return 2
				}
				continue
			}
			if v, ok := val("--supervisor-heartbeat"); ok {
				d, err := unitfile.ParseDuration(v)
				if err != nil {
					fmt.Fprintf(os.Stderr, "--supervisor-heartbeat: %v\n", err)
					return 2
				}
				opts.HeartbeatMS = int(d / time.Millisecond)
				continue
			}
			if v, ok := val("--control-allow-uid"); ok {
				ids, err := parseIDs(v)
				if err != nil {
					fmt.Fprintf(os.Stderr, "--control-allow-uid: %v\n", err)
					return 2
				}
				opts.ControlAllowUID = ids
				continue
			}
			if v, ok := val("--control-allow-gid"); ok {
				ids, err := parseIDs(v)
				if err != nil {
					fmt.Fprintf(os.Stderr, "--control-allow-gid: %v\n", err)
					return 2
				}
				opts.ControlAllowGID = ids
				continue
			}
			if v, ok := val("--pass-environment"); ok {
				opts.PassEnvironment = v
				continue
			}
			if v, ok := val("--log-size-max"); ok {
				n, err := unitfile.ParseSize(v)
				if err != nil {
					fmt.Fprintf(os.Stderr, "--log-size-max: %v\n", err)
					return 2
				}
				opts.Rotate.SizeMax = n
				continue
			}
			if v, ok := val("--log-total-max"); ok {
				n, err := unitfile.ParseSize(v)
				if err != nil {
					fmt.Fprintf(os.Stderr, "--log-total-max: %v\n", err)
					return 2
				}
				opts.Rotate.TotalMax = n
				continue
			}
			if v, ok := val("--log-file-count"); ok {
				n, err := strconv.Atoi(v)
				if err != nil || n < 0 {
					fmt.Fprintf(os.Stderr, "--log-file-count: expected a non-negative number\n")
					return 2
				}
				opts.Rotate.FileCount = n
				continue
			}
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(os.Stderr, "docker-systemd: unknown option %s\n", a)
				return 2
			}
		}
	}

	m := manager.New(opts)
	return m.Boot()
}

func splitCommaUnits(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, unitfile.CanonicalName(p))
		}
	}
	return out
}

// parseIDs rejects non-integer values rather than silently widening the
// control policy (09 §4).
func parseIDs(s string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%q is not a numeric id", p)
		}
		out = append(out, n)
	}
	return out, nil
}

func printManagerHelp() {
	fmt.Printf(`docker-systemd %s — a systemd-compatible init for containers

Usage: as PID 1, or invoked as init / systemd / init-docker-systemd.
       Client commands are reached via the systemctl, journalctl, service,
       poweroff, halt, reboot, shutdown, systemd-notify and
       systemd-detect-virt symlinks it installs.

Options:
  --log-to-stderr[=UNIT,...]  Mirror unit logs to stderr (docker logs)
  --no-logfile                Do not write %s/*.log
  --log-level=LEVEL           error, warn, info, debug or trace (default info)
  --default-target=TARGET     Boot target (default multi-user.target)
  --no-install                Do not symlink over the distro's init/systemctl/...
  --no-auto-reload            Do not reload unit files when they change on disk
  --shutdown-timeout=TIME     Global shutdown budget (default 90s)
  --backend=NAME              auto, cgroup2, subreaper or degraded
  --supervisor-heartbeat=TIME Tree re-scan interval (default 1s)
  --control-allow-uid=UID,... Additional uids allowed on the control socket
  --control-allow-gid=GID,... Additional gids allowed on the control socket
  --pass-environment=LIST     all, none, or a comma-separated variable list
  --compat-tmp-socket         Also create the legacy %s symlink (insecure)
  --log-size-max=SIZE         Rotate a unit log above this size (default 16M)
  --log-file-count=N          Rotated generations to keep (default 3)
  --log-total-max=SIZE        Cap the whole log directory (default 128M)
  --version                   Print the version

Behaviour differences from systemd are documented in the README.
`, version, journalLogDir(), "/tmp/docker-systemd.sock")
}

func journalLogDir() string {
	_ = journal.TimeFormat
	return "/var/log/services"
}
