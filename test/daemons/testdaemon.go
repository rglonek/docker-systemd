// Command testdaemon is the L2 test helper of designs/docs/next/11-testing.md §3.
//
// Each mode reproduces one real-world way a service creates or escapes from
// processes, so the supervisor can be asserted against behaviour rather than
// against a mock.
package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: testdaemon <mode> [args...]")
		os.Exit(2)
	}
	mode := os.Args[1]
	args := os.Args[2:]

	switch mode {
	case "foreground":
		foreground()
	case "double-fork":
		doubleFork(args)
	case "setsid-escape":
		setsidEscape(args)
	case "pidfile":
		pidFile(args)
	case "notify":
		notify(args)
	case "ignore-sigterm":
		ignoreSigterm()
	case "fork-bomb-on-term":
		forkBombOnTerm()
	case "slow-stop":
		slowStop(args)
	case "closefrom":
		closeFrom()
	case "crash-loop":
		os.Exit(7)
	case "zombie-maker":
		zombieMaker()
	case "echo":
		fmt.Println(strings.Join(args, " "))
	case "exit":
		code := 0
		if len(args) > 0 {
			code, _ = strconv.Atoi(args[0])
		}
		os.Exit(code)
	case "spawn-child":
		spawnChild(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", mode)
		os.Exit(2)
	}
}

// blockForever parks until signalled, which is what a real foreground service
// does.
func blockForever() {
	select {}
}

func foreground() {
	fmt.Println("testdaemon: running in the foreground")
	blockForever()
}

// doubleFork is the classic daemonise: fork, parent exits, setsid, fork again.
// The surviving grandchild reparents to the nearest subreaper — which, under
// this design, is the unit's supervisor rather than PID 1.
func doubleFork(args []string) {
	if len(args) > 0 && args[0] == "--level2" {
		// Second generation: detach and run.
		if _, err := syscall.Setsid(); err != nil {
			fmt.Fprintf(os.Stderr, "setsid: %v\n", err)
		}
		cmd := exec.Command(os.Args[0], "double-fork", "--level3")
		if err := cmd.Start(); err != nil {
			os.Exit(1)
		}
		fmt.Printf("testdaemon: level2 %d exiting, grandchild %d\n", os.Getpid(), cmd.Process.Pid)
		os.Exit(0)
	}
	if len(args) > 0 && args[0] == "--level3" {
		fmt.Printf("testdaemon: daemon pid %d ppid %d\n", os.Getpid(), os.Getppid())
		blockForever()
	}
	cmd := exec.Command(os.Args[0], "double-fork", "--level2")
	if err := cmd.Start(); err != nil {
		os.Exit(1)
	}
	fmt.Printf("testdaemon: level1 %d exiting, child %d\n", os.Getpid(), cmd.Process.Pid)
	os.Exit(0)
}

// setsidEscape puts the child in a brand new session, which is precisely what
// makes kill(-pgid) insufficient.
func setsidEscape(args []string) {
	if len(args) > 0 && args[0] == "--child" {
		fmt.Printf("testdaemon: escaped child %d sid=new\n", os.Getpid())
		blockForever()
	}
	cmd := exec.Command(os.Args[0], "setsid-escape", "--child")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		os.Exit(1)
	}
	fmt.Printf("testdaemon: parent %d exiting, escaped child %d\n", os.Getpid(), cmd.Process.Pid)
	os.Exit(0)
}

// pidFile writes a pid file. The variant selects whose pid it writes, which is
// how the stale and hostile cases are exercised.
func pidFile(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: testdaemon pidfile <path> <correct|stale|hostile|malformed> [pid]")
		os.Exit(2)
	}
	path, variant := args[0], args[1]

	if variant == "correct" {
		// Daemonise, then write the surviving pid.
		if len(args) > 2 && args[2] == "--child" {
			if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
				os.Exit(1)
			}
			blockForever()
		}
		cmd := exec.Command(os.Args[0], "pidfile", path, "correct", "--child")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}

	content := ""
	switch variant {
	case "stale":
		content = "999999\n"
	case "hostile":
		if len(args) > 2 {
			content = args[2] + "\n"
		} else {
			content = "1\n"
		}
	case "malformed":
		content = "pid=/var/run/x\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		os.Exit(1)
	}
	// Fork a survivor so the tree is not empty, then exit like a forking unit.
	cmd := exec.Command(os.Args[0], "foreground")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// notify daemonises and then reports readiness from the surviving pid, which
// is exactly the case MAINPID= exists for.
func notify(args []string) {
	socket := os.Getenv("NOTIFY_SOCKET")
	delay := time.Duration(0)
	if len(args) > 0 {
		if d, err := time.ParseDuration(args[0]); err == nil {
			delay = d
		}
	}
	time.Sleep(delay)
	if socket != "" {
		send(socket, fmt.Sprintf("READY=1\nMAINPID=%d\nSTATUS=accepting connections", os.Getpid()))
	}
	blockForever()
}

func send(socket, payload string) {
	name := socket
	if strings.HasPrefix(name, "@") {
		name = "\x00" + name[1:]
	}
	conn, err := net.Dial("unixgram", name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdaemon: cannot reach %s: %v\n", socket, err)
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(payload))
}

// ignoreSigterm proves the ladder escalates to SIGKILL.
func ignoreSigterm() {
	ch := make(chan os.Signal, 8)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	fmt.Println("testdaemon: ignoring SIGTERM")
	for range ch {
		fmt.Println("testdaemon: caught a signal and ignoring it")
	}
}

// forkBombOnTerm proves the ladder converges: it re-enumerates each round, and
// SIGKILL cannot be blocked.
func forkBombOnTerm() {
	ch := make(chan os.Signal, 8)
	signal.Notify(ch, syscall.SIGTERM)
	fmt.Println("testdaemon: will fork on every SIGTERM")
	for range ch {
		cmd := exec.Command(os.Args[0], "foreground")
		_ = cmd.Start()
		fmt.Println("testdaemon: forked another child in response to SIGTERM")
	}
}

// slowStop takes N seconds to exit after SIGTERM, which is what the shutdown
// budget test needs.
func slowStop(args []string) {
	d := 3 * time.Second
	if len(args) > 0 {
		if parsed, err := time.ParseDuration(args[0]); err == nil {
			d = parsed
		}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	fmt.Printf("testdaemon: will take %s to stop\n", d)
	<-ch
	time.Sleep(d)
	os.Exit(0)
}

// closeFrom defeats inherited-fd liveness tricks by closing everything above
// stderr.
func closeFrom() {
	fmt.Println("testdaemon: closing all descriptors above 2")
	os.Stdout.Sync()
	for fd := 3; fd < 1024; fd++ {
		_ = syscall.Close(fd)
	}
	blockForever()
}

// zombieMaker forks children and never reaps them, so the supervisor's own
// reaper has to.
func zombieMaker() {
	for i := 0; i < 5; i++ {
		cmd := exec.Command(os.Args[0], "exit", "0")
		_ = cmd.Start()
	}
	fmt.Println("testdaemon: forked five children and will not reap them")
	blockForever()
}

// spawnChild forks N children that stay in the tree, for membership counting.
func spawnChild(args []string) {
	n := 2
	if len(args) > 0 {
		if parsed, err := strconv.Atoi(args[0]); err == nil {
			n = parsed
		}
	}
	for i := 0; i < n; i++ {
		cmd := exec.Command(os.Args[0], "foreground")
		if err := cmd.Start(); err != nil {
			os.Exit(1)
		}
	}
	fmt.Printf("testdaemon: spawned %d children\n", n)
	blockForever()
}
