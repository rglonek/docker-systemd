package daemons

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

func loadUnitFile(d *daemon, r io.Reader) error {
	const (
		sectionNone    = 1
		sectionUnit    = 2
		sectionService = 3
		sectionInstall = 4
		sectionUnknown = 5
	)
	if d == nil || r == nil {
		return errors.New("nil value provided")
	}
	d.Lock()
	defer d.Unlock()
	if d.def == nil {
		d.def = &daemondef{
			Wants:        make(map[string]*daemon),
			WantedBy:     make(map[string]*daemon),
			Requires:     make(map[string]*daemon),
			RequiredBy:   make(map[string]*daemon),
			Requisite:    make(map[string]*daemon),
			RequisiteOf:  make(map[string]*daemon),
			BindsTo:      make(map[string]*daemon),
			BoundBy:      make(map[string]*daemon),
			PartOf:       make(map[string]*daemon),
			ConsistsOf:   make(map[string]*daemon),
			Upholds:      make(map[string]*daemon),
			UpheldBy:     make(map[string]*daemon),
			Conflicts:    make(map[string]*daemon),
			ConflictedBy: make(map[string]*daemon),
			Before:       make(map[string]*daemon),
			After:        make(map[string]*daemon),
			OnFailure:    make(map[string]*daemon),
			OnSuccess:    make(map[string]*daemon),
		}
	}
	section := sectionNone
	concat := ""
	s := bufio.NewScanner(r)
	for s.Scan() {
		line := s.Text()
		trimmedLine := strings.Trim(line, "\r\n\t ")
		if strings.HasPrefix(trimmedLine, ";") || strings.HasPrefix(trimmedLine, "#") {
			continue
		}
		if strings.HasSuffix(trimmedLine, "\\") {
			concat += strings.TrimSuffix(strings.TrimRight(line, " \r\n\t"), "\\") + " "
			continue
		}
		if concat != "" {
			line = concat + strings.TrimSuffix(strings.TrimRight(line, " \r\n\t"), "\\")
			concat = ""
			trimmedLine = strings.Trim(line, "\r\n\t ")
		}
		nextLine := true
		switch strings.ToUpper(trimmedLine) {
		case "[UNIT]":
			section = sectionUnit
		case "[SERVICE]":
			section = sectionService
		case "[INSTALL]":
			section = sectionInstall
		default:
			if strings.HasPrefix(trimmedLine, "[") && strings.HasSuffix(trimmedLine, "]") {
				section = sectionUnknown
			} else {
				nextLine = false
			}
		}
		if nextLine {
			continue
		}
		name, val := parseUnitLine(line)
		switch section {
		case sectionUnit:
			switch name {
			case "DESCRIPTION":
				d.def.Description = val
			case "WANTS": // attempt to start, don't fail if dependency fails
				item := d.def.Wants
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "WANTEDBY": // opposite of wants
				item := d.def.WantedBy
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "REQUIRES": // attempt to start, fail if dependency fails
				item := d.def.Requires
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "REQUIREDBY": // opposite of requires
				item := d.def.RequiredBy
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "REQUISITE": // if dependency not running, fail, do not attempt to start
				item := d.def.Requisite
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "REQUISITEOF":
				item := d.def.RequisiteOf
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "BINDSTO": // like requires, but if dependency stops, stop this too
				item := d.def.BindsTo
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "BOUNDBY":
				item := d.def.BoundBy
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "PARTOF": // not start, but if dependencies are stopped or restarted, this one will too
				item := d.def.PartOf
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "CONSISTSOF":
				item := d.def.ConsistsOf
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "UPHOLDS": // like wants, but also if the depdendencies are stopped, starts them again (monitoring-like)
				item := d.def.Upholds
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "UPHELDBY":
				item := d.def.UpheldBy
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "CONFLICTS": // if configured, will stop another service if this one is started
				item := d.def.Conflicts
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "CONFLICTEDBY":
				item := d.def.ConflictedBy
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "BEFORE": // start before another unit, stop after
				item := d.def.Before
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "AFTER": // start after another unit, stop before
				item := d.def.After
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "ONFAILURE": // what to start when the service fails (ret!=0)
				item := d.def.OnFailure
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "ONSUCCESS": // what to start when the service exits with ret==0
				item := d.def.OnSuccess
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "STOPWHENUNNEEDED": // if started not manually but as a dependency, if not longer needed, stop it
				if strings.ToLower(val) == "true" {
					d.def.StopWhenUnneeded = true
				} else {
					d.def.StopWhenUnneeded = false
				}
			case "FAILUREACTION": //none, reboot, reboot-force, reboot-immediate, poweroff, poweroff-force, poweroff-immediate, exit, exit-force, soft-reboot, soft-reboot-force, kexec, kexec-force, halt, halt-force and halt-immediate
				d.def.FailureAction = val
			case "SUCCESSACTION": //none, reboot, reboot-force, reboot-immediate, poweroff, poweroff-force, poweroff-immediate, exit, exit-force, soft-reboot, soft-reboot-force, kexec, kexec-force, halt, halt-force and halt-immediate
				d.def.SuccessAction = val

			}
		case sectionInstall:
			switch name {
			case "REQUIREDBY": // opposite of requires
				item := d.def.RequiredBy
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "WANTEDBY": // opposite of wants
				item := d.def.WantedBy
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			case "UPHELDBY": // opposite of upholds
				item := d.def.UpheldBy
				if val == "" {
					for k := range item {
						delete(item, k)
					}
				} else {
					for _, v := range strings.Split(val, " ") {
						item[v] = nil
					}
				}
			}
		case sectionService:
			switch name {
			case "TYPE": // simple=foreground,exec=simple,forking=background(needs PID as PIDFile=),oneshot=runs+quits but considered still running,dbus/notify/notify-reload=background,idle=simple
				d.def.ServiceType = val
			case "REMAINAFTEREXIT": // considered up if service exits
				if strings.ToLower(val) == "true" {
					d.def.RemainAfterExit = true
				} else {
					d.def.RemainAfterExit = false
				}
			case "PIDFILE": // pidfile for background jobs
				d.def.PidFile = val
			case "EXECSTART": // run this to start (oneshot=multiple lines permitted, otherwise 1 line only)
				if strings.Contains(d.name, "@") {
					valInst := strings.Split(d.name, "@")[1]
					val = strings.ReplaceAll(strings.ReplaceAll(val, "%i", valInst), "%I", valInst)
				}
				d.def.ExecStart = append(d.def.ExecStart, val)
			case "EXECSTOP":
				if strings.Contains(d.name, "@") {
					valInst := strings.Split(d.name, "@")[1]
					val = strings.ReplaceAll(strings.ReplaceAll(val, "%i", valInst), "%I", valInst)
				}
				d.def.ExecStop = append(d.def.ExecStop, val)
			case "EXECSTARTPRE":
				if strings.Contains(d.name, "@") {
					valInst := strings.Split(d.name, "@")[1]
					val = strings.ReplaceAll(strings.ReplaceAll(val, "%i", valInst), "%I", valInst)
				}
				d.def.ExecStartPre = append(d.def.ExecStartPre, val)
			case "EXECSTARTPOST":
				if strings.Contains(d.name, "@") {
					valInst := strings.Split(d.name, "@")[1]
					val = strings.ReplaceAll(strings.ReplaceAll(val, "%i", valInst), "%I", valInst)
				}
				d.def.ExecStartPost = append(d.def.ExecStartPost, val)
			case "EXECSTOPPRE":
				if strings.Contains(d.name, "@") {
					valInst := strings.Split(d.name, "@")[1]
					val = strings.ReplaceAll(strings.ReplaceAll(val, "%i", valInst), "%I", valInst)
				}
				d.def.ExecStopPre = append(d.def.ExecStopPre, val)
			case "EXECSTOPPOST":
				if strings.Contains(d.name, "@") {
					valInst := strings.Split(d.name, "@")[1]
					val = strings.ReplaceAll(strings.ReplaceAll(val, "%i", valInst), "%I", valInst)
				}
				d.def.ExecStopPost = append(d.def.ExecStopPost, val)
			case "EXECCONDITION": // before pre, if ret!=0, just don't start (success), otherwise continue
				if strings.Contains(d.name, "@") {
					valInst := strings.Split(d.name, "@")[1]
					val = strings.ReplaceAll(strings.ReplaceAll(val, "%i", valInst), "%I", valInst)
				}
				d.def.ExecCondition = append(d.def.ExecCondition, val)
			case "EXECRELOAD": // call on daemon-reload
				if strings.Contains(d.name, "@") {
					valInst := strings.Split(d.name, "@")[1]
					val = strings.ReplaceAll(strings.ReplaceAll(val, "%i", valInst), "%I", valInst)
				}
				d.def.ExecReload = val
			case "RESTARTSEC": // sleep between restarts Takes a unit-less value in seconds, or a time span value such as "5min 20s". Defaults to 100ms.
				var err error
				d.def.RestartSleep, err = parseSystemdDuration(val)
				if err != nil {
					return err
				}
			case "TIMEOUTSEC":
				fallthrough
			case "TIMEOUTSTOPSEC": // SIGTERM->SIGKILL timeout, or 'infinity'
				var err error
				d.def.StopTimeout, err = parseSystemdDuration(val)
				if err != nil {
					return err
				}
			case "RESTART": // no, on-success, (on-failure, on-abnormal, on-watchdog, on-abort), or always
				d.def.Restart = val
			case "WORKINGDIRECTORY":
				d.def.WorkingDirectory = val
			case "USER":
				d.def.User = val
			case "GROUP":
				d.def.Group = val
			case "LIMITCPU": // ulimit -t
				d.def.LimitCpu = val
			case "LIMITFSIZE": // ulimit -f
				d.def.LimitFsize = val
			case "LIMITDATA": // ulimit -d
				d.def.LimitData = val
			case "LIMITSTACK": // ulimit -s
				d.def.LimitStack = val
			case "LIMITCORE": // ulimit -c
				d.def.LimitCore = val
			case "LIMITRSS": // ulimit -m
				d.def.LimitRss = val
			case "LIMITNOFILE": // ulimit -n
				d.def.LimitNoFile = val
			case "LIMITAS": // ulimit -v
				d.def.LimitAs = val
			case "LIMITNPROC": // ulimit -u
				d.def.LimitNProc = val
			case "LIMITMEMLOCK": // ulimit -l
				d.def.LimitMemLock = val
			case "LIMITLOCKS": // ulimit -x
				d.def.LimitLocks = val
			case "LIMITSIGPENDING": // ulimit -i
				d.def.LimitSigPending = val
			case "LIMITMSGQUEUE": // ulimit -q
				d.def.LimitMsgQueue = val
			case "LIMITNICE": // ulimit -e
				d.def.LimitNice = val
			case "LIMITRTPRIO": // ulimit -r
				d.def.LimitRtPrio = val
			case "LIMITRTTIME": // ulimit -R
				d.def.LimitRtTime = val
			case "ENVIRONMENT":
				d.def.Env = append(d.def.Env, val)
			case "ENVIRONMENTFILE":
				d.def.EnvFile = append(d.def.EnvFile, val)
			}
		}
	}
	return nil
}

func parseUnitLine(line string) (name string, val string) {
	split := strings.Split(line, "=")
	name = strings.ToUpper(strings.Trim(split[0], "\r\n\t "))
	if len(split) == 1 {
		return
	}
	right := strings.Join(split[1:], "=")
	tmp := strings.Trim(right, " ")
	if strings.HasPrefix(tmp, "\"") && strings.HasSuffix(tmp, "\"") {
		val = strings.Trim(tmp, "\"")
		return
	}
	if strings.HasPrefix(tmp, "'") && strings.HasSuffix(tmp, "'") {
		val = strings.Trim(tmp, "'")
		return
	}
	val = strings.TrimLeft(right, " ")
	return
}

func parseSystemdDuration(s string) (time.Duration, error) {
	notdigits := false
	for _, r := range s {
		if r < '0' || r > '9' {
			notdigits = true
			break
		}
	}
	if !notdigits {
		no, _ := strconv.Atoi(s)
		return time.Second * time.Duration(no), nil
	}
	type replace struct {
		what string
		with []string
	}
	replaces := []replace{
		{
			what: "us",
			with: []string{"μs", "usec"},
		},
		{
			what: "ms",
			with: []string{"msec"},
		},
		{
			what: "s",
			with: []string{"seconds", "second", "sec"},
		},
		{
			what: "m",
			with: []string{"minutes", "minute", "min"},
		},
		{
			what: "h",
			with: []string{"hours", "hour", "hr"},
		},
		{
			what: "d",
			with: []string{"days", "day"},
		},
		{
			what: "w",
			with: []string{"weeks", "week"},
		},
		{
			what: "M",
			with: []string{"months", "month"},
		},
		{
			what: "y",
			with: []string{"years", "year"},
		},
	}
	s = strings.ReplaceAll(s, " ", "")
	for _, r := range replaces {
		for _, with := range r.with {
			s = strings.ReplaceAll(s, with, r.what)
		}
	}
	// [-+]?([0-9]*(\.[0-9]*)?[a-z]+)+
	orig := s
	var d uint64
	neg := false

	// Consume [-+]?
	if s != "" {
		c := s[0]
		if c == '-' || c == '+' {
			neg = c == '-'
			s = s[1:]
		}
	}
	// Special case: if all that is left is "0", this is zero.
	if s == "0" {
		return 0, nil
	}
	if s == "" {
		return 0, errors.New("time: invalid duration " + quote(orig))
	}
	for s != "" {
		var (
			v, f  uint64      // integers before, after decimal point
			scale float64 = 1 // value = v + f/scale
		)

		var err error

		// The next character must be [0-9.]
		if !(s[0] == '.' || '0' <= s[0] && s[0] <= '9') {
			return 0, errors.New("time: invalid duration " + quote(orig))
		}
		// Consume [0-9]*
		pl := len(s)
		v, s, err = leadingInt(s)
		if err != nil {
			return 0, errors.New("time: invalid duration " + quote(orig))
		}
		pre := pl != len(s) // whether we consumed anything before a period

		// Consume (\.[0-9]*)?
		post := false
		if s != "" && s[0] == '.' {
			s = s[1:]
			pl := len(s)
			f, scale, s = leadingFraction(s)
			post = pl != len(s)
		}
		if !pre && !post {
			// no digits (e.g. ".s" or "-.s")
			return 0, errors.New("time: invalid duration " + quote(orig))
		}

		// Consume unit.
		i := 0
		for ; i < len(s); i++ {
			c := s[i]
			if c == '.' || '0' <= c && c <= '9' {
				break
			}
		}
		if i == 0 {
			return 0, errors.New("time: missing unit in duration " + quote(orig))
		}
		u := s[:i]
		s = s[i:]
		unit, ok := unitMap[u]
		if !ok {
			return 0, errors.New("time: unknown unit " + quote(u) + " in duration " + quote(orig))
		}
		if v > 1<<63/unit {
			// overflow
			return 0, errors.New("time: invalid duration " + quote(orig))
		}
		v *= unit
		if f > 0 {
			// float64 is needed to be nanosecond accurate for fractions of hours.
			// v >= 0 && (f*unit/scale) <= 3.6e+12 (ns/h, h is the largest unit)
			v += uint64(float64(f) * (float64(unit) / scale))
			if v > 1<<63 {
				// overflow
				return 0, errors.New("time: invalid duration " + quote(orig))
			}
		}
		d += v
		if d > 1<<63 {
			return 0, errors.New("time: invalid duration " + quote(orig))
		}
	}
	if neg {
		return -time.Duration(d), nil
	}
	if d > 1<<63-1 {
		return 0, errors.New("time: invalid duration " + quote(orig))
	}
	return time.Duration(d), nil
}

func quote(s string) string {
	buf := make([]byte, 1, len(s)+2)
	buf[0] = '"'
	for i, c := range s {
		if c >= runeSelf || c < ' ' {
			var width int
			if c == runeError {
				width = 1
				if i+2 < len(s) && s[i:i+3] == string(runeError) {
					width = 3
				}
			} else {
				width = len(string(c))
			}
			for j := 0; j < width; j++ {
				buf = append(buf, `\x`...)
				buf = append(buf, lowerhex[s[i+j]>>4])
				buf = append(buf, lowerhex[s[i+j]&0xF])
			}
		} else {
			if c == '"' || c == '\\' {
				buf = append(buf, '\\')
			}
			buf = append(buf, string(c)...)
		}
	}
	buf = append(buf, '"')
	return string(buf)
}

var errLeadingInt = errors.New("time: bad [0-9]*")

const (
	lowerhex  = "0123456789abcdef"
	runeSelf  = 0x80
	runeError = '\uFFFD'
)

func leadingInt[bytes []byte | string](s bytes) (x uint64, rem bytes, err error) {
	i := 0
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			break
		}
		if x > 1<<63/10 {
			// overflow
			return 0, rem, errLeadingInt
		}
		x = x*10 + uint64(c) - '0'
		if x > 1<<63 {
			// overflow
			return 0, rem, errLeadingInt
		}
	}
	return x, s[i:], nil
}

func leadingFraction(s string) (x uint64, scale float64, rem string) {
	i := 0
	scale = 1
	overflow := false
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			break
		}
		if overflow {
			continue
		}
		if x > (1<<63-1)/10 {
			// It's possible for overflow to give a positive number, so take care.
			overflow = true
			continue
		}
		y := x*10 + uint64(c) - '0'
		if y > 1<<63 {
			overflow = true
			continue
		}
		x = y
		scale *= 10
	}
	return x, scale, s[i:]
}

var unitMap = map[string]uint64{
	"ns": uint64(time.Nanosecond),
	"us": uint64(time.Microsecond),
	"µs": uint64(time.Microsecond), // U+00B5 = micro symbol
	"μs": uint64(time.Microsecond), // U+03BC = Greek letter mu
	"ms": uint64(time.Millisecond),
	"s":  uint64(time.Second),
	"m":  uint64(time.Minute),
	"h":  uint64(time.Hour),
	"d":  uint64(time.Hour * 24),
	"w":  uint64(time.Hour * 24 * 7),
	"M":  uint64(float64(time.Hour*24) * 30.44),
	"y":  uint64(float64(time.Hour*24) * 365.25),
}
