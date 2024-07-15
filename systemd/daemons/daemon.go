package daemons

import (
	"bytes"
	"docker-systemd/procwait"
	"docker-systemd/systemd/pidtracker"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bestmethod/inslice"
	"github.com/mitchellh/go-ps"
	"gopkg.in/yaml.v3"
)

type daemon struct {
	sync.RWMutex
	state      DaemonState
	stateError error
	name       string
	def        *daemondef
	olddef     *daemondef
	paths      []string
	isMasked   bool
	isManual   bool // is started as dependency or as wanted
	cmds       []*exec.Cmd
	pids       []int
}

type daemondef struct {
	Description string
	// dependencies
	Wants        map[string]*daemon
	WantedBy     map[string]*daemon
	Requires     map[string]*daemon
	RequiredBy   map[string]*daemon
	Requisite    map[string]*daemon
	RequisiteOf  map[string]*daemon
	BindsTo      map[string]*daemon
	BoundBy      map[string]*daemon
	PartOf       map[string]*daemon
	ConsistsOf   map[string]*daemon
	Upholds      map[string]*daemon
	UpheldBy     map[string]*daemon
	Conflicts    map[string]*daemon
	ConflictedBy map[string]*daemon
	Before       map[string]*daemon
	After        map[string]*daemon
	OnFailure    map[string]*daemon
	OnSuccess    map[string]*daemon
	// behaviour
	StopWhenUnneeded bool
	FailureAction    string
	SuccessAction    string
	// service section
	ServiceType      string
	RemainAfterExit  bool
	PidFile          string
	ExecStart        []string
	ExecStop         []string
	ExecStartPre     []string
	ExecStartPost    []string
	ExecStopPre      []string
	ExecStopPost     []string
	ExecCondition    []string
	ExecReload       string
	RestartSleep     time.Duration
	StopTimeout      time.Duration
	Restart          string
	WorkingDirectory string
	User             string
	Group            string
	Env              []string
	EnvFile          []string
	// rlimit
	LimitCpu        string
	LimitFsize      string
	LimitData       string
	LimitStack      string
	LimitCore       string
	LimitRss        string
	LimitNoFile     string
	LimitAs         string
	LimitNProc      string
	LimitMemLock    string
	LimitLocks      string
	LimitSigPending string
	LimitMsgQueue   string
	LimitNice       string
	LimitRtPrio     string
	LimitRtTime     string
}

func (d *daemon) Name() string {
	return d.name
}

func (d *daemon) handleStopDeps() {
	for depName, dep := range d.def.BoundBy {
		err := dep.Stop()
		if err != nil {
			log.Printf("Failed to stop dependency %s: %s", depName, err)
		}
	}
	for depName, dep := range d.def.ConsistsOf {
		err := dep.Stop()
		if err != nil {
			log.Printf("Failed to stop dependency %s: %s", depName, err)
		}
	}
}

func (d *daemon) monitorNeeded() {
	for {
		time.Sleep(time.Second)
		if d.State() == StateStopped {
			break
		}
		if !d.def.StopWhenUnneeded {
			continue
		}
		for _, dep := range d.def.WantedBy {
			if dep.State() == StateRunning {
				continue
			}
		}
		for _, dep := range d.def.RequiredBy {
			if dep.State() == StateRunning {
				continue
			}
		}
		for _, dep := range d.def.RequisiteOf {
			if dep.State() == StateRunning {
				continue
			}
		}
		for _, dep := range d.def.BoundBy {
			if dep.State() == StateRunning {
				continue
			}
		}
		for _, dep := range d.def.ConsistsOf {
			if dep.State() == StateRunning {
				continue
			}
		}
		for _, dep := range d.def.UpheldBy {
			if dep.State() == StateRunning {
				continue
			}
		}
		err := d.Stop()
		if err != nil {
			log.Printf("Failed to stop service as unneeded: %s: %s", d.Name(), err)
		}
		break
	}
}

func (d *daemon) monitorCmds(l *Logger) {
	go d.monitorNeeded()
	d.pids = []int{}
	cmdpids := []int{}
	var ans []*syscall.WaitStatus
	for _, cmd := range d.cmds {
		if cmd.Process != nil {
			ans = append(ans, procwait.Wait(cmd.Process.Pid))
		}
		cmdpids = append(cmdpids, cmd.Process.Pid)
	}
	d.cmds = []*exec.Cmd{}
	if inslice.HasString([]string{"forking", "dbus", "notify", "notify-reload"}, d.def.ServiceType) {
		if d.def.PidFile != "" {
			npid, err := os.ReadFile(d.def.PidFile)
			if err == nil {
				pids := ""
				for _, r := range npid {
					if r >= '0' && r <= '9' {
						pids += string(r)
					}
				}
				if pids != "" {
					p, err := strconv.Atoi(pids)
					if err == nil && p > 1 {
						d.pids = []int{p}
						ans = append(ans, procwait.Wait(p))
						d.pids = []int{}
					}
				}
			}
		} else {
			for {
				processList, err := ps.Processes()
				if err != nil {
					break
				}
				pids := []int{}
				for _, p := range processList {
					if p.PPid() != 1 {
						continue
					}
					penviron, err := os.ReadFile("/proc/" + strconv.Itoa(p.Pid()) + "/environ")
					if err == nil {
						envs := strings.Split(string(penviron), string([]byte{0}))
						for _, nenv := range envs {
							if !strings.HasPrefix(nenv, "SYSTEMD_SERVICE_NAME=") {
								continue
							}
							if strings.Split(nenv, "=")[1] == d.name {
								pids = append(pids, p.Pid())
							}
						}
					}
				}
				d.pids = pids
				for _, p := range pids {
					ans = append(ans, procwait.Wait(p))
				}
				if len(pids) == 0 {
					break
				}
			}
		}
		for {
			kids := []int{}
			found := false
			for _, cmd := range cmdpids {
				children := pidtracker.Find(cmd)
				for _, c := range children {
					if procwait.Is(c) {
						kids = append(kids, c)
					}
				}
			}
			d.pids = kids
			for _, c := range kids {
				if rr := procwait.Wait(c); rr != nil {
					found = true
					ans = append(ans, rr)
				}
			}
			if !found {
				break
			}
		}
		d.pids = []int{}
	}
	defer l.Close()
	actionType := d.def.SuccessAction
	for _, aa := range ans {
		if aa != nil && aa.ExitStatus() != 0 {
			actionType = d.def.FailureAction
		}
	}
	if strings.HasPrefix(actionType, "poweroff") {
		procwait.Run(exec.Command("poweroff"))
		return
	}

	restart := d.def.Restart
	if d.state == StateStopped || d.state == StateStopping || d.state == StateRestarting {
		restart = "no"
	} else {
		for _, dep := range d.def.UpheldBy {
			if dep.State() == StateRunning {
				restart = "always"
			}
		}
	}
	if d.def.RestartSleep == 0 {
		d.def.RestartSleep = time.Second
	}
	switch restart {
	case "always":
		d.state = StateRestarting
		time.Sleep(d.def.RestartSleep)
		d.stateError = nil
		for _, aa := range ans {
			if aa != nil && aa.ExitStatus() != 0 {
				log.Printf("<%s> Process exited with error code %d", d.name, aa.ExitStatus())
			}
		}
		if d.state == StateStopped {
			return
		}
		err := d.start(d.isManual)
		if err != nil {
			log.Printf("RESTART failed: %s", err)
			return
		}
	case "on-failure", "on-abnormal", "on-watchdog", "on-abort":
		d.stateError = nil
		for _, aa := range ans {
			if aa != nil && aa.ExitStatus() != 0 {
				d.stateError = fmt.Errorf("process exited with error code %d", aa.ExitStatus())
			}
		}
		if d.stateError != nil {
			log.Printf("Will restart %s in %v", d.name, d.def.RestartSleep)
			d.state = StateRestarting
			time.Sleep(d.def.RestartSleep)
			if d.state == StateStopped {
				return
			}
			log.Printf("Restarting %s", d.name)
			err := d.start(d.isManual)
			if err != nil {
				log.Printf("RESTART failed: %s", err)
				return
			}
		} else if d.stateError == nil && !d.def.RemainAfterExit {
			d.state = StateStopped
			d.isManual = false
			d.handleStopDeps()
		}
	case "on-success":
		d.stateError = nil
		for _, aa := range ans {
			if aa != nil && aa.ExitStatus() != 0 {
				d.stateError = fmt.Errorf("process exited with error code %d", aa.ExitStatus())
			}
		}
		if d.stateError == nil && !d.def.RemainAfterExit {
			d.state = StateRestarting
			time.Sleep(d.def.RestartSleep)
			if d.state == StateStopped {
				return
			}
			err := d.start(d.isManual)
			if err != nil {
				log.Printf("RESTART failed: %s", err)
				d.handleStopDeps()
				return
			}
		} else if d.stateError != nil {
			d.state = StateStopped
			d.isManual = false
			d.handleStopDeps()
		}
	default:
		for _, aa := range ans {
			if aa != nil && aa.ExitStatus() != 0 {
				d.stateError = fmt.Errorf("process exited with error code %d", aa.ExitStatus())
			}
		}
		if !d.def.RemainAfterExit || d.stateError != nil {
			d.state = StateStopped
			d.isManual = false
			d.handleStopDeps()
		}
	}
}

func (d *daemon) runOnFailure(*Logger) {
	for ii, i := range d.def.OnFailure {
		err := i.start(false)
		if err != nil {
			log.Printf("<%s> OnFailure dependency %s start failed: %s", d.name, ii, err)
		}
	}
}

func (d *daemon) runOnSuccess(*Logger) {
	for ii, i := range d.def.OnSuccess {
		err := i.start(false)
		if err != nil {
			log.Printf("<%s> OnSuccess dependency %s start failed: %s", d.name, ii, err)
		}
	}
}

func (d *daemon) Start() error {
	return d.start(true)
}

func (d *daemon) start(isManual bool) error {
	if d == nil {
		return nil
	}
	d.Lock()
	defer d.Unlock()
	log.Printf("START: %s Starting", d.name)
	defer log.Printf("START: %s Done", d.name)
	if d.isMasked {
		return errors.New("service is masked")
	}
	if d.def.LimitCpu != "" {
		log.Printf("<%s> WARNING: LimitCPU=%s specified in service file, cannot set in docker", d.name, d.def.LimitCpu)
	}
	if d.def.LimitFsize != "" {
		log.Printf("<%s> WARNING: LimitFsize=%s specified in service file, cannot set in docker", d.name, d.def.LimitFsize)
	}
	if d.def.LimitData != "" {
		log.Printf("<%s> WARNING: LimitData=%s specified in service file, cannot set in docker", d.name, d.def.LimitData)
	}
	if d.def.LimitStack != "" {
		log.Printf("<%s> WARNING: LimitStack=%s specified in service file, cannot set in docker", d.name, d.def.LimitStack)
	}
	if d.def.LimitCore != "" {
		log.Printf("<%s> WARNING: LimitCore=%s specified in service file, cannot set in docker", d.name, d.def.LimitCore)
	}
	if d.def.LimitRss != "" {
		log.Printf("<%s> WARNING: LimitRss=%s specified in service file, cannot set in docker", d.name, d.def.LimitRss)
	}
	if d.def.LimitNoFile != "" {
		log.Printf("<%s> WARNING: LimitNoFile=%s specified in service file, cannot set in docker", d.name, d.def.LimitNoFile)
	}
	if d.def.LimitAs != "" {
		log.Printf("<%s> WARNING: LimitAs=%s specified in service file, cannot set in docker", d.name, d.def.LimitAs)
	}
	if d.def.LimitNProc != "" {
		log.Printf("<%s> WARNING: LimitNProc=%s specified in service file, cannot set in docker", d.name, d.def.LimitNProc)
	}
	if d.def.LimitMemLock != "" {
		log.Printf("<%s> WARNING: LimitMemLock=%s specified in service file, cannot set in docker", d.name, d.def.LimitMemLock)
	}
	if d.def.LimitLocks != "" {
		log.Printf("<%s> WARNING: LimitLocks=%s specified in service file, cannot set in docker", d.name, d.def.LimitLocks)
	}
	if d.def.LimitSigPending != "" {
		log.Printf("<%s> WARNING: LimitSigPending=%s specified in service file, cannot set in docker", d.name, d.def.LimitSigPending)
	}
	if d.def.LimitMsgQueue != "" {
		log.Printf("<%s> WARNING: LimitMsgQueue=%s specified in service file, cannot set in docker", d.name, d.def.LimitMsgQueue)
	}
	if d.def.LimitNice != "" {
		log.Printf("<%s> WARNING: LimitNice=%s specified in service file, cannot set in docker", d.name, d.def.LimitNice)
	}
	if d.def.LimitRtPrio != "" {
		log.Printf("<%s> WARNING: LimitRtPrio=%s specified in service file, cannot set in docker", d.name, d.def.LimitRtPrio)
	}
	if d.def.LimitRtTime != "" {
		log.Printf("<%s> WARNING: LimitRtTime=%s specified in service file, cannot set in docker", d.name, d.def.LimitRtTime)
	}
	if isManual {
		d.isManual = true
	}
	if d.state == StateRunning {
		return nil
	}
	if d.state != StateRestarting {
		d.state = StateStarting
	}
	if d.def.ServiceType == "oneshot" {
		d.def.RemainAfterExit = true
	}
	d.Unlock()
	err := d.stop(false)
	d.Lock()
	if err != nil {
		d.state = StateStopped
		d.stateError = err
		return fmt.Errorf("could not cleanup old run jobs: %s", err)
	}
	d.stateError = nil
	denv := d.def.Env
	for _, ef := range d.def.EnvFile {
		failOnNotFound := true
		if strings.HasPrefix(ef, "-") {
			failOnNotFound = false
			ef = strings.TrimPrefix(ef, "-")
		}
		ct, err := os.ReadFile(ef)
		if err != nil {
			if failOnNotFound {
				return fmt.Errorf("env file %s not found: %s", ef, err)
			} else {
				continue
			}
		}
		denv = append(denv, strings.Split(string(ct), "\n")...)
	}
	var uid, gid int64
	if d.def.User != "" {
		u, err := user.Lookup(d.def.User)
		if err != nil {
			return fmt.Errorf("failed to find user %s: %s", d.def.User, err)
		}
		uid, _ = strconv.ParseInt(u.Uid, 10, 32)
		gid, _ = strconv.ParseInt(u.Gid, 10, 32)
	}
	if d.def.Group != "" {
		g, err := user.LookupGroup(d.def.Group)
		if err != nil {
			return fmt.Errorf("failed to find user %s: %s", d.def.User, err)
		}
		gid, _ = strconv.ParseInt(g.Gid, 10, 32)
		if uid == 0 {
			u, err := user.Current()
			if err != nil {
				return fmt.Errorf("failed to find user %s: %s", d.def.User, err)
			}
			uid, _ = strconv.ParseInt(u.Uid, 10, 32)
		}
	}
	l, err := NewLogger(d.name)
	if err != nil {
		d.state = StateStopped
		d.stateError = err
		return fmt.Errorf("could not open log file: %s", err)
	}
	for _, line := range d.def.ExecCondition {
		cmd := exec.Command("/bin/bash", "-c", line)
		cmd.Stdin = os.Stdin
		cmd.Stdout = l
		cmd.Stderr = l
		cmd.Env = denv
		pstate, err := procwait.Run(cmd)
		if err != nil {
			d.state = StateStopped
			d.stateError = nil
			log.Printf("<%s> Condition %s not met", d.name, line)
			l.Close()
			return nil
		}
		if pstate.ExitStatus() != 0 {
			d.state = StateStopped
			d.stateError = nil
			log.Printf("<%s> Condition %s not met (returned %d)", d.name, line, pstate.ExitStatus())
			l.Close()
			return nil
		}
	}
	for ii, i := range d.def.Requisite {
		if i.State() != StateRunning {
			log.Printf("<%s> Dependency %s not running, aborting: %s", d.name, ii, err)
			l.Close()
			d.stateError = fmt.Errorf("%s: %s", i.Name(), err)
			d.runOnFailure(l)
			return err
		}
	}
	for ii, i := range d.def.Requires {
		err := i.start(false)
		if err != nil {
			log.Printf("<%s> Dependency %s start failed, aborting: %s", d.name, ii, err)
			l.Close()
			d.stateError = fmt.Errorf("%s: %s", i.Name(), err)
			d.runOnFailure(l)
			return err
		}
	}
	for ii, i := range d.def.BindsTo {
		err := i.start(false)
		if err != nil {
			log.Printf("<%s> Dependency %s start failed, aborting: %s", d.name, ii, err)
			l.Close()
			d.stateError = fmt.Errorf("%s: %s", i.Name(), err)
			d.runOnFailure(l)
			return err
		}
	}
	for ii, i := range d.def.Wants {
		err := i.start(false)
		if err != nil {
			log.Printf("<%s> Dependency %s start failed: %s", d.name, ii, err)
		}
	}
	for ii, i := range d.def.Upholds {
		err := i.start(false)
		if err != nil {
			log.Printf("<%s> Dependency %s start failed: %s", d.name, ii, err)
		}
	}
	for ii, i := range d.def.Conflicts {
		err := i.Stop()
		if err != nil {
			log.Printf("<%s> Dependency %s stop failed, aborting: %s", d.name, ii, err)
			l.Close()
			d.stateError = fmt.Errorf("%s: %s", i.Name(), err)
			d.runOnFailure(l)
			return err
		}
	}
	for _, line := range d.def.ExecStartPre {
		failOnErr := true
		if strings.HasPrefix(line, "-") {
			failOnErr = false
			line = strings.TrimPrefix(line, "-")
		}
		cmd := exec.Command("/bin/bash", "-c", line)
		cmd.Stdin = os.Stdin
		cmd.Stdout = l
		cmd.Stderr = l
		cmd.Env = denv
		_, err := procwait.Run(cmd)
		if err != nil {
			log.Printf("<%s> Failed: %s: %s", d.name, line, err)
			if failOnErr {
				d.Unlock()
				d.Stop()
				d.Lock()
				d.stateError = fmt.Errorf("<%s> Failed StartPre: %s: %s", d.name, line, err)
				l.Close()
				d.runOnFailure(l)
				return err
			}
		}
	}
	cmds := []*exec.Cmd{}
	for _, line := range d.def.ExecStart {
		failOnErr := true
		if strings.HasPrefix(line, "-") {
			failOnErr = false
			line = strings.TrimPrefix(line, "-")
		}
		cmd := exec.Command("/bin/bash", "-c", line)
		cmd.Stdin = os.Stdin
		cmd.Stdout = l
		cmd.Stderr = l
		cmd.Env = append(denv, "SYSTEMD_SERVICE_NAME="+d.name)
		if uid != 0 {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
			cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
		}
		if d.def.WorkingDirectory != "" {
			cmd.Dir = d.def.WorkingDirectory
		}
		err := cmd.Start()
		if err != nil {
			log.Printf("<%s> Failed: %s: %s", d.name, line, err)
			if failOnErr {
				d.Unlock()
				d.Stop()
				d.Lock()
				d.stateError = fmt.Errorf("<%s> Failed Start: %s: %s", d.name, line, err)
				l.Close()
				d.runOnFailure(l)
				return err
			}
		}
		cmds = append(cmds, cmd)
	}
	for _, line := range d.def.ExecStartPost {
		failOnErr := true
		if strings.HasPrefix(line, "-") {
			failOnErr = false
			line = strings.TrimPrefix(line, "-")
		}
		cmd := exec.Command("/bin/bash", "-c", line)
		cmd.Stdin = os.Stdin
		cmd.Stdout = l
		cmd.Stderr = l
		cmd.Env = denv
		_, err := procwait.Run(cmd)
		if err != nil {
			log.Printf("<%s> Failed: %s: %s", d.name, line, err)
			if failOnErr {
				d.Unlock()
				d.Stop()
				d.Lock()
				d.stateError = fmt.Errorf("<%s> Failed StartPost: %s: %s", d.name, line, err)
				l.Close()
				d.runOnFailure(l)
				return err
			}
		}
	}
	d.state = StateRunning
	d.stateError = nil
	d.runOnSuccess(l)
	d.cmds = cmds
	go d.monitorCmds(l)
	return nil
}

func (d *daemon) Stop() error {
	if d == nil {
		return nil
	}
	return d.stop(true)
}

func (d *daemon) stop(printStopping bool) error {
	if d == nil {
		return nil
	}
	d.Lock()
	defer d.Unlock()
	if printStopping {
		log.Printf("STOP: %s Stopping", d.name)
		defer log.Printf("STOP: %s Done", d.name)
	}
	if d.isMasked {
		return errors.New("service is masked")
	}
	if d.state != StateRestarting {
		d.state = StateStopping
	}
	l, err := NewLogger(d.name)
	if err != nil {
		d.stateError = err
		return fmt.Errorf("could not open log file: %s", err)
	}
	for _, line := range d.def.ExecStopPre {
		cmd := exec.Command("/bin/bash", "-c", line)
		cmd.Stdin = os.Stdin
		cmd.Stdout = l
		cmd.Stderr = l
		_, err := procwait.Run(cmd)
		if err != nil {
			d.stateError = fmt.Errorf("<%s> Failed StopPre: %s: %s", d.name, line, err)
			log.Printf("<%s> Failed to run StopPre action (%s): %s", d.name, line, err)
		}
	}
	for _, line := range d.def.ExecStop {
		cmd := exec.Command("/bin/bash", "-c", line)
		cmd.Stdin = os.Stdin
		cmd.Stdout = l
		cmd.Stderr = l
		if d.def.WorkingDirectory != "" {
			cmd.Dir = d.def.WorkingDirectory
		}
		_, err := procwait.Run(cmd)
		if err != nil {
			d.stateError = fmt.Errorf("<%s> Failed Stop: %s: %s", d.name, line, err)
			log.Printf("<%s> Failed to run Stop action (%s): %s", d.name, line, err)
		}
	}
	for _, line := range d.def.ExecStopPost {
		cmd := exec.Command("/bin/bash", "-c", line)
		cmd.Stdin = os.Stdin
		cmd.Stdout = l
		cmd.Stderr = l
		_, err := procwait.Run(cmd)
		if err != nil {
			d.stateError = fmt.Errorf("<%s> Failed StopPost: %s: %s", d.name, line, err)
			log.Printf("<%s> Failed to run StopPost action (%s): %s", d.name, line, err)
		}
	}
	for _, cmd := range d.cmds {
		if cmd.Process != nil {
			log.Printf("Sending SIGTERM to %d", cmd.Process.Pid)
			syscall.Kill(cmd.Process.Pid, syscall.SIGTERM)
		}
	}
	exited := true
	tout := 5 * time.Second
	if d.def.StopTimeout != 0 {
		tout = d.def.StopTimeout
	}
	waitStop := time.Now()
	for {
		exited = true
		time.Sleep(10 * time.Millisecond)
		for _, cmd := range d.cmds {
			if procwait.Is(cmd.Process.Pid) {
				exited = false
				break
			}
		}
		if exited {
			break
		}
		if time.Since(waitStop) > tout {
			break
		}
	}
	if !exited {
		for _, cmd := range d.cmds {
			if cmd.Process != nil {
				syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
			}
		}
		d.stateError = errors.New("failed to exit using SIGTERM, applied SIGKILL")
	}
	d.state = StateStopped
	return nil
}

func (d *daemon) Restart() error {
	d.Lock()
	if d.isMasked {
		d.Unlock()
		return errors.New("service is masked")
	}
	d.state = StateRestarting
	d.Unlock()
	err := d.Stop()
	if err != nil {
		return err
	}
	return d.Start()
}

func (d *daemon) Reload() error {
	d.Lock()
	if d.isMasked {
		d.Unlock()
		return errors.New("service is masked")
	}
	defer d.Unlock()
	if d.def.ExecReload != "" {
		var buf bytes.Buffer
		cmd := exec.Command("/bin/bash", "-c", d.def.ExecReload)
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		_, err := procwait.Run(cmd)
		out := buf.Bytes()
		if err != nil {
			return fmt.Errorf("failed reload: %s: %s", err, string(out))
		}
	} else {
		for _, cmd := range d.cmds {
			syscall.Kill(cmd.Process.Pid, syscall.SIGHUP)
		}
	}
	return nil
}

func (d *daemon) IsEnabled() bool {
	d.Lock()
	defer d.Unlock()
	target := "/etc/systemd/system/multi-user.target.wants"
	if d.def == nil {
		return false
	}
	serviceDest := path.Join(target, d.name+".service")
	if _, err := os.Stat(serviceDest); err != nil {
		return false
	}
	return true
}

func (d *daemon) CreateInstance(name string) error {
	for _, p := range d.paths {
		if !strings.Contains(p, "@") {
			return errors.New("not an instance")
		}
	}
	for _, p := range d.paths {
		d, f := path.Split(p)
		f = strings.TrimSuffix(f, ".service") + name + ".service"
		dest := path.Join(d, f)
		err := os.Link(p, dest)
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *daemon) DeleteService() error {
	for _, p := range d.paths {
		os.RemoveAll(p)
	}
	return nil
}

func (d *daemon) Enable() error {
	d.Lock()
	defer d.Unlock()
	target := "/etc/systemd/system/multi-user.target.wants"
	if _, err := os.Stat(target); err != nil {
		os.MkdirAll(target, 0755)
	}
	if d.def == nil {
		return errors.New("service is removed")
	}
	if len(d.paths) == 0 {
		return errors.New("service path not found")
	}
	serviceDest := path.Join(target, d.name+".service")
	if _, err := os.Stat(serviceDest); err != nil {
		err = os.WriteFile(serviceDest, []byte("OK"), 0644)
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *daemon) Disable() error {
	d.Lock()
	defer d.Unlock()
	target := "/etc/systemd/system/multi-user.target.wants"
	serviceDest := path.Join(target, d.name+".service")
	if _, err := os.Stat(serviceDest); err == nil {
		err = os.Remove(serviceDest)
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *daemon) Mask() error {
	d.Lock()
	defer d.Unlock()
	target := path.Join("/etc/systemd/system/", d.name+".service")
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("masking failed: %s exists", target)
	}
	err := os.Symlink("/dev/null", "target")
	if err != nil {
		return err
	}
	d.isMasked = true
	return nil

}

func (d *daemon) Unmask() error {
	d.Lock()
	defer d.Unlock()
	target := path.Join("/etc/systemd/system/", d.name+".service")
	if nstat, err := os.Lstat(target); err != nil || nstat.Mode()&os.ModeSymlink == 0 {
		d.isMasked = false
		return nil
	} else if t, err := os.Readlink(target); err != nil || t != "/dev/null" {
		d.isMasked = false
		return nil
	}
	err := os.Remove(target)
	if err != nil {
		return err
	}
	d.isMasked = false
	return nil
}

func (d *daemon) State() DaemonState {
	d.RLock()
	defer d.RUnlock()
	return d.state
}

func (d *daemon) Detail() string {
	d.RLock()
	w, _ := yaml.Marshal(d.def)
	d.RUnlock()
	s := string(w)
	s = s + fmt.Sprintf("Masked: %t\n", d.isMasked)
	return s
}

func (d *daemon) Status() (string, error) {
	d.RLock()
	defer d.RUnlock()
	msg := "State: "
	switch d.state {
	case StateRestarting:
		msg += "Restarting"
	case StateRunning:
		pids := []string{}
		for _, cmd := range d.cmds {
			pids = append(pids, strconv.Itoa(cmd.Process.Pid))
		}
		for _, pid := range d.pids {
			pids = append(pids, strconv.Itoa(pid))
		}
		msg += "Running (" + strings.Join(pids, ", ") + ")"
	case StateStarting:
		msg += "Starting"
	case StateStopped:
		msg += "Stopped"
	case StateStopping:
		msg += "Stopping"
	default:
		msg += "Unknown"
	}
	if d.isMasked {
		msg += " (masked)"
	}
	return msg, d.stateError
}
