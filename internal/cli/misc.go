package cli

import (
	"fmt"
	"os"
	"strings"

	"docker-systemd/internal/proto"
	"docker-systemd/internal/supervisor"
	"docker-systemd/internal/unitfile"
)

// Service is the SysV-ish `service` shim.
//
// v0.5.x swapped argv[1] and argv[2] only when len(args) > 2, so
// `service --status-all` and `service foo` were mishandled (defect C15).
func Service(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: service < option > | --status-all | "+
			"[ service_name [ command | --full-restart ] ]")
		return 2
	}

	switch args[0] {
	case "--status-all", "-status-all":
		return statusAll()
	case "--help", "-h":
		fmt.Println("Usage: service <name> {start|stop|restart|reload|force-reload|status}")
		fmt.Println("       service --status-all")
		return 0
	}

	name := args[0]
	if len(args) == 1 {
		fmt.Fprintf(os.Stderr, "Usage: service %s {start|stop|restart|reload|force-reload|status}\n", name)
		return 2
	}
	verb := args[1]
	rest := args[2:]

	switch verb {
	case "--full-restart":
		verb = "restart"
	case "force-reload":
		verb = "reload-or-restart"
	}

	out := append([]string{verb, name}, rest...)
	return Systemctl(out)
}

// statusAll renders the `[ + ]` / `[ - ]` list.
func statusAll() int {
	return runRequest("service", proto.Request{
		Verb:    "list-units",
		Options: proto.RequestOptions{All: true, Types: []string{"service"}},
	}, func(resp *Response) int {
		var units []proto.UnitStatus
		if err := resp.DecodeData(&units); err != nil {
			return 0
		}
		for _, u := range units {
			mark := "-"
			if u.ActiveState == proto.StateActive || u.ActiveState == proto.StateReloading {
				mark = "+"
			}
			fmt.Printf(" [ %s ]  %s\n", mark, strings.TrimSuffix(u.Name, ".service"))
		}
		return 0
	})
}

// Power implements poweroff, halt, reboot, shutdown, telinit and runlevel.
func Power(name string, args []string) int {
	switch name {
	case "runlevel":
		// Nothing here has runlevels; report the multi-user equivalent, which
		// is what scripts testing for "3" or "5" expect to see.
		fmt.Println("N 5")
		return 0

	case "telinit":
		if len(args) == 0 {
			return usageError("telinit requires a runlevel")
		}
		switch args[0] {
		case "0":
			return Systemctl([]string{"poweroff"})
		case "6":
			return Systemctl([]string{"reboot"})
		case "q", "Q", "u", "U":
			return Systemctl([]string{"daemon-reload"})
		default:
			return Systemctl([]string{"default"})
		}

	case "shutdown":
		verb := "poweroff"
		for _, a := range args {
			switch a {
			case "-r", "--reboot":
				verb = "reboot"
			case "-H", "--halt":
				verb = "halt"
			case "-c":
				fmt.Fprintln(os.Stderr, "shutdown: no pending shutdown to cancel")
				return 0
			case "-k":
				fmt.Fprintln(os.Stderr, "shutdown: -k (warn only) is a no-op here")
				return 0
			}
		}
		return Systemctl([]string{verb})

	default:
		// poweroff, halt, reboot. `-f`/`--force` is accepted and ignored:
		// there is no distinction here between a forced and a clean stop
		// beyond the shutdown budget.
		return Systemctl([]string{name})
	}
}

// Notify implements the `systemd-notify` helper, which lets shell-implemented
// Type=notify units report readiness.
func Notify(args []string) int {
	var assignments []string
	socket := os.Getenv("NOTIFY_SOCKET")
	pid := 0

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--ready":
			assignments = append(assignments, "READY=1")
		case a == "--stopping":
			assignments = append(assignments, "STOPPING=1")
		case a == "--reloading":
			assignments = append(assignments, "RELOADING=1")
		case strings.HasPrefix(a, "--status="):
			assignments = append(assignments, "STATUS="+strings.TrimPrefix(a, "--status="))
		case strings.HasPrefix(a, "--pid="):
			v := strings.TrimPrefix(a, "--pid=")
			if v == "" || v == "auto" {
				pid = os.Getppid()
			} else {
				if _, err := fmt.Sscanf(v, "%d", &pid); err != nil {
					return usageError("--pid expects a number")
				}
			}
		case a == "--pid":
			pid = os.Getppid()
		case strings.HasPrefix(a, "--booted"):
			if _, err := os.Stat("/run/systemd/system"); err != nil {
				return 1
			}
			return 0
		case a == "--help" || a == "-h":
			fmt.Println("Usage: systemd-notify [--ready] [--status=TEXT] [--pid[=PID]] [VAR=VALUE...]")
			return 0
		case strings.HasPrefix(a, "-"):
			return usageError(fmt.Sprintf("Unknown option %s.", a))
		default:
			assignments = append(assignments, a)
		}
	}
	if pid > 0 {
		assignments = append(assignments, fmt.Sprintf("MAINPID=%d", pid))
	}
	if len(assignments) == 0 {
		return usageError("systemd-notify: nothing to send")
	}
	if err := supervisor.SendNotify(socket, assignments); err != nil {
		fmt.Fprintf(os.Stderr, "systemd-notify: %v\n", err)
		return 1
	}
	return 0
}

// DetectVirt implements `systemd-detect-virt`. Units with
// ConditionVirtualization=!container must be able to skip themselves, which
// needs this to answer truthfully.
func DetectVirt(args []string) int {
	quiet := false
	container := false
	vm := false
	for _, a := range args {
		switch a {
		case "-q", "--quiet":
			quiet = true
		case "-c", "--container":
			container = true
		case "-v", "--vm":
			vm = true
		case "-h", "--help":
			fmt.Println("Usage: systemd-detect-virt [-c|--container] [-v|--vm] [-q|--quiet]")
			return 0
		}
	}
	v := unitfile.DetectVirtualization()
	if vm && !container {
		// We are never a VM from inside a container.
		if !quiet {
			fmt.Println("none")
		}
		return 1
	}
	if !quiet {
		fmt.Println(v)
	}
	if v == "none" {
		return 1
	}
	return 0
}
