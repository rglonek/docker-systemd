package unitfile

import (
	"strconv"
	"strings"
	"syscall"
)

// signalNames maps systemd's signal spellings onto numbers. Only the signals a
// unit file can plausibly name are listed; anything else is a parse error, so
// KillSignal=SIGTREM fails loudly instead of silently defaulting.
var signalNames = map[string]int{
	"SIGABRT":   int(syscall.SIGABRT),
	"SIGALRM":   int(syscall.SIGALRM),
	"SIGBUS":    int(syscall.SIGBUS),
	"SIGCHLD":   int(syscall.SIGCHLD),
	"SIGCONT":   int(syscall.SIGCONT),
	"SIGFPE":    int(syscall.SIGFPE),
	"SIGHUP":    int(syscall.SIGHUP),
	"SIGILL":    int(syscall.SIGILL),
	"SIGINT":    int(syscall.SIGINT),
	"SIGIO":     int(syscall.SIGIO),
	"SIGKILL":   int(syscall.SIGKILL),
	"SIGPIPE":   int(syscall.SIGPIPE),
	"SIGPROF":   int(syscall.SIGPROF),
	"SIGPWR":    int(syscall.SIGPWR),
	"SIGQUIT":   int(syscall.SIGQUIT),
	"SIGSEGV":   int(syscall.SIGSEGV),
	"SIGSTOP":   int(syscall.SIGSTOP),
	"SIGSYS":    int(syscall.SIGSYS),
	"SIGTERM":   int(syscall.SIGTERM),
	"SIGTRAP":   int(syscall.SIGTRAP),
	"SIGTSTP":   int(syscall.SIGTSTP),
	"SIGTTIN":   int(syscall.SIGTTIN),
	"SIGTTOU":   int(syscall.SIGTTOU),
	"SIGURG":    int(syscall.SIGURG),
	"SIGUSR1":   int(syscall.SIGUSR1),
	"SIGUSR2":   int(syscall.SIGUSR2),
	"SIGVTALRM": int(syscall.SIGVTALRM),
	"SIGWINCH":  int(syscall.SIGWINCH),
	"SIGXCPU":   int(syscall.SIGXCPU),
	"SIGXFSZ":   int(syscall.SIGXFSZ),
}

// SignalNumber resolves a signal name (with or without the SIG prefix, in any
// case) or a decimal number. It returns 0 for an unknown signal.
func SignalNumber(s string) int {
	name := strings.ToUpper(strings.TrimSpace(s))
	if name == "" {
		return 0
	}
	if n, err := strconv.Atoi(name); err == nil {
		if n > 0 && n < 64 {
			return n
		}
		return 0
	}
	if !strings.HasPrefix(name, "SIG") {
		name = "SIG" + name
	}
	return signalNames[name]
}

// SignalName renders a signal number as its canonical name, or "SIG<n>" when
// the number is not one we know.
func SignalName(n int) string {
	for name, v := range signalNames {
		if v == n {
			return name
		}
	}
	return "SIG" + strconv.Itoa(n)
}
