package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"docker-systemd/internal/journal"
	"docker-systemd/internal/paths"
	"docker-systemd/internal/unitfile"
)

// Journalctl is the `journalctl` entry point. It reads the log files directly
// rather than going through the manager, so it works even when the manager is
// not running.
func Journalctl(args []string) int {
	q := journal.NewQuery()
	output := "short"
	follow := false
	noHostname := false
	quiet := false
	linesSet := false
	var vacuumSize string
	var vacuumTime string
	diskUsage := false
	bootFilter := false

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
		case a == "--help" || a == "-h":
			printJournalctlHelp()
			return 0
		case a == "--version":
			fmt.Println("docker-systemd journalctl")
			return 0
		case a == "-u" || a == "--unit" || strings.HasPrefix(a, "--unit="):
			v := strings.TrimPrefix(a, "--unit=")
			if a == "-u" || a == "--unit" {
				var okv bool
				if v, okv = next(); !okv {
					return usageError("-u requires a unit name")
				}
			}
			q.Units = append(q.Units, unitLogName(v))
		case a == "-n" || a == "--lines" || strings.HasPrefix(a, "--lines="):
			v := strings.TrimPrefix(a, "--lines=")
			if a == "-n" || a == "--lines" {
				var okv bool
				if v, okv = next(); !okv {
					v = "10"
					i--
				}
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return usageError("--lines expects a number")
			}
			q.Lines = n
			linesSet = true
		case a == "-f" || a == "--follow":
			follow = true
		case a == "-e" || a == "--pager-end":
			if !linesSet {
				q.Lines = 1000
			}
		case a == "-r" || a == "--reverse":
			q.Reverse = true
		case a == "-S" || a == "--since" || strings.HasPrefix(a, "--since="):
			v := strings.TrimPrefix(a, "--since=")
			if a == "-S" || a == "--since" {
				var okv bool
				if v, okv = next(); !okv {
					return usageError("--since requires a time")
				}
			}
			t, err := parseTimeSpec(v, true)
			if err != nil {
				return usageError(err.Error())
			}
			q.Since = t
		case a == "-U" || a == "--until" || strings.HasPrefix(a, "--until="):
			v := strings.TrimPrefix(a, "--until=")
			if a == "-U" || a == "--until" {
				var okv bool
				if v, okv = next(); !okv {
					return usageError("--until requires a time")
				}
			}
			t, err := parseTimeSpec(v, false)
			if err != nil {
				return usageError(err.Error())
			}
			q.Until = t
		case a == "-b" || a == "--boot" || strings.HasPrefix(a, "--boot="):
			v := strings.TrimPrefix(a, "--boot=")
			if v != "" && v != "0" && v != a {
				fmt.Fprintln(os.Stderr,
					"journalctl: only the current boot is available; nothing survives a container\n"+
						"restart unless /var/log is a volume.")
				return 1
			}
			bootFilter = true
		case a == "-p" || a == "--priority" || strings.HasPrefix(a, "--priority="):
			v := strings.TrimPrefix(a, "--priority=")
			if a == "-p" || a == "--priority" {
				var okv bool
				if v, okv = next(); !okv {
					return usageError("--priority requires a value")
				}
			}
			// A range like "0..4" keeps the upper bound, which is what the
			// filter needs.
			if idx := strings.Index(v, ".."); idx >= 0 {
				v = v[idx+2:]
			}
			p, err := journal.ParsePriority(v)
			if err != nil {
				return usageError(err.Error())
			}
			q.Priority = p
		case a == "-o" || a == "--output" || strings.HasPrefix(a, "--output="):
			v := strings.TrimPrefix(a, "--output=")
			if a == "-o" || a == "--output" {
				var okv bool
				if v, okv = next(); !okv {
					return usageError("--output requires a format")
				}
			}
			output = v
		case a == "-g" || a == "--grep" || strings.HasPrefix(a, "--grep="):
			v := strings.TrimPrefix(a, "--grep=")
			if a == "-g" || a == "--grep" {
				var okv bool
				if v, okv = next(); !okv {
					return usageError("--grep requires a pattern")
				}
			}
			q.Grep = v
		case a == "--disk-usage":
			diskUsage = true
		case strings.HasPrefix(a, "--vacuum-size="):
			vacuumSize = strings.TrimPrefix(a, "--vacuum-size=")
		case strings.HasPrefix(a, "--vacuum-time="):
			vacuumTime = strings.TrimPrefix(a, "--vacuum-time=")
		case a == "-k" || a == "--dmesg":
			// There is no kernel log in a container; empty output, exit 0.
			return 0
		case a == "-q":
			quiet = true
		case a == "--no-hostname":
			noHostname = true
		case a == "-x" || a == "--catalog" || a == "--no-pager" || a == "--no-full" ||
			a == "-l" || a == "--full" || a == "--all" || a == "-a" || a == "--system":
			// Accepted and ignored.
		case strings.HasPrefix(a, "-"):
			return usageError(fmt.Sprintf("Unknown option %s.", a))
		default:
			q.Units = append(q.Units, unitLogName(a))
		}
	}
	_ = quiet

	rd := journal.NewReader(paths.LogDir)

	if diskUsage {
		fmt.Printf("Archived and active journals take up %s in the file system.\n",
			humanSize(rd.DiskUsage()))
		return 0
	}
	if vacuumSize != "" {
		n, err := unitfile.ParseSize(vacuumSize)
		if err != nil {
			return usageError(err.Error())
		}
		removed, freed := rd.VacuumSize(n)
		fmt.Printf("Vacuuming done, freed %s of archived journals (%d files).\n",
			humanSize(freed), removed)
		return 0
	}
	if vacuumTime != "" {
		d, err := unitfile.ParseDuration(vacuumTime)
		if err != nil {
			return usageError(err.Error())
		}
		removed, freed := rd.VacuumTime(time.Now().Add(-d))
		fmt.Printf("Vacuuming done, freed %s of archived journals (%d files).\n",
			humanSize(freed), removed)
		return 0
	}

	if bootFilter {
		if t, err := readBootTime(); err == nil && q.Since.IsZero() {
			q.Since = t
		}
	}
	if follow && !linesSet {
		q.Lines = 10
	}

	recs, err := rd.Read(q)
	if err != nil {
		fmt.Fprintf(os.Stderr, "journalctl: %v\n", err)
		return 1
	}
	for _, r := range recs {
		fmt.Println(formatRecord(r, output, noHostname))
	}

	if !follow {
		return 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel()
	}()
	followQuery := q
	followQuery.Lines = 0
	_ = rd.Follow(ctx, followQuery, func(r journal.Record) {
		fmt.Println(formatRecord(r, output, noHostname))
	})
	return 0
}

// unitLogName maps a unit name onto the name used in log filenames.
func unitLogName(s string) string {
	s = strings.TrimSuffix(s, ".log")
	return unitfile.CanonicalName(s)
}

// formatRecord renders one record in the requested output format.
func formatRecord(r journal.Record, format string, noHostname bool) string {
	host := ""
	if !noHostname {
		if h, err := os.Hostname(); err == nil {
			host = h + " "
		}
	}
	ident := r.Ident()
	pid := ""
	if r.PID > 0 {
		pid = fmt.Sprintf("[%d]", r.PID)
	}
	switch format {
	case "cat":
		return r.Message
	case "short-iso":
		return fmt.Sprintf("%s %s%s%s: %s",
			r.Time.Format(time.RFC3339), host, ident, pid, r.Message)
	case "short-precise":
		return fmt.Sprintf("%s %s%s%s: %s",
			r.Time.Format("Jan 02 15:04:05.000000"), host, ident, pid, r.Message)
	case "json", "json-pretty":
		obj := map[string]any{
			"__REALTIME_TIMESTAMP": strconv.FormatInt(r.Time.UnixMicro(), 10),
			"PRIORITY":             strconv.Itoa(r.Priority),
			"_PID":                 strconv.Itoa(r.PID),
			"_SYSTEMD_UNIT":        r.Unit,
			"SYSLOG_IDENTIFIER":    ident,
			"MESSAGE":              r.Message,
		}
		var b []byte
		if format == "json-pretty" {
			b, _ = json.MarshalIndent(obj, "", "    ")
		} else {
			b, _ = json.Marshal(obj)
		}
		return string(b)
	default: // short
		return fmt.Sprintf("%s %s%s%s: %s",
			r.Time.Format("Jan 02 15:04:05"), host, ident, pid, r.Message)
	}
}

// parseTimeSpec accepts the expressions people actually type:
// absolute stamps, `yesterday`/`today`/`now`, and relative offsets like `-1h`.
func parseTimeSpec(s string, isSince bool) (time.Time, error) {
	s = strings.TrimSpace(s)
	now := time.Now()
	switch strings.ToLower(s) {
	case "now":
		return now, nil
	case "today":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()), nil
	case "yesterday":
		y := now.AddDate(0, 0, -1)
		return time.Date(y.Year(), y.Month(), y.Day(), 0, 0, 0, 0, now.Location()), nil
	case "tomorrow":
		t := now.AddDate(0, 0, 1)
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, now.Location()), nil
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		sign := time.Duration(1)
		if s[0] == '-' {
			sign = -1
		}
		d, err := unitfile.ParseDuration(s[1:])
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid time specification %q", s)
		}
		return now.Add(sign * d), nil
	}
	for _, layout := range []string{
		time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02",
		"15:04:05", "15:04",
	} {
		if t, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			if layout == "15:04:05" || layout == "15:04" {
				return time.Date(now.Year(), now.Month(), now.Day(),
					t.Hour(), t.Minute(), t.Second(), 0, now.Location()), nil
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time specification %q", s)
}

// readBootTime reads the timestamp recorded at boot.
func readBootTime() (time.Time, error) {
	data, err := os.ReadFile(paths.BootIDFile)
	if err != nil {
		return time.Time{}, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return time.Time{}, fmt.Errorf("empty boot id file")
	}
	return time.Parse(journal.TimeFormat, fields[0])
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTPE"[exp])
}

func printJournalctlHelp() {
	fmt.Print(`journalctl [OPTIONS...] [UNIT...]

Query the unit logs written by the docker-systemd manager.

  -u --unit=UNIT        Show logs for the named unit (optional; default: all)
  -n --lines=N          Show the last N lines
  -f --follow           Follow new entries
  -r --reverse          Show the newest entries first
  -S --since=TIME       Show entries at or after TIME (accepts -1h, today, ...)
  -U --until=TIME       Show entries at or before TIME
  -b --boot             Show entries from the current boot only
  -p --priority=PRIO    Show entries at or above the given priority
  -o --output=FORMAT    short, short-iso, short-precise, cat, json, json-pretty
  -g --grep=PATTERN     Show entries whose message contains PATTERN
     --disk-usage       Show the size of the log directory
     --vacuum-size=SIZE Remove archived logs until the directory fits
     --vacuum-time=TIME Remove archived logs older than TIME
     --no-hostname      Omit the hostname field
`)
}
