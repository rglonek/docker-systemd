package procwait

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"
)

type procLocks struct {
	sync.Mutex
	w   map[int]*proc
	run map[*exec.Cmd]*proc
}

type proc struct {
	sync.RWMutex
	ws syscall.WaitStatus
}

var waits = &procLocks{
	w:   make(map[int]*proc),
	run: make(map[*exec.Cmd]*proc),
}

func Is(pid int) bool {
	p, _ := os.FindProcess(pid)
	return p.Signal(syscall.Signal(0)) == nil
}

// replaces cmd.Run()
func Run(cmd *exec.Cmd) (*syscall.WaitStatus, error) {
	waits.Lock()
	waits.run[cmd] = &proc{}
	waits.run[cmd].Lock()
	waits.Unlock()
	err := cmd.Start()
	if err != nil {
		return nil, err
	}
	ws, err := waitCmd(cmd)
	if err != nil {
		return ws, err
	}
	if ws.ExitStatus() == 0 {
		return ws, nil
	} else {
		return ws, fmt.Errorf("process returned with exit status %d", ws.ExitStatus())
	}
}

var ErrNotFound = errors.New("command not found")

func waitCmd(cmd *exec.Cmd) (*syscall.WaitStatus, error) {
	waits.Lock()
	l, ok := waits.run[cmd]
	waits.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	l.RLock()
	defer l.RUnlock()
	return &l.ws, nil
}

// wait on pid to exit; if pid is already exited, return nil
func Wait(pid int) *syscall.WaitStatus {
	// if we have a wait already, hook onto it
	waits.Lock()
	if l, ok := waits.w[pid]; ok {
		waits.Unlock()
		l.RLock()
		defer l.RUnlock()
		return &l.ws
	}
	// check if the process exists, but is dead
	p, _ := os.FindProcess(pid)
	if p.Signal(syscall.Signal(0)) != nil {
		waits.Unlock()
		return nil
	}
	// create a tracking instance
	l := new(proc)
	waits.w[pid] = l
	waits.w[pid].Lock()
	waits.Unlock()
	l.RLock()
	defer l.RUnlock()
	return &l.ws
}

// set this to log all reaper wait results
var Debug = false

// run this to setup the tracker and star the wait system
func Init() {
	go func() {
		for {
			err := wait(0)
			if err != nil {
				time.Sleep(time.Second)
			}
		}
	}()
}

// defer running this to the very end, it will run the final reap
func FinalReap() {
	for {
		err := wait(syscall.WNOHANG)
		if err != nil {
			break
		}
	}
}

func wait(opts int) error {
	var ws syscall.WaitStatus
	wpid, err := syscall.Wait4(-1, &ws, opts, nil)
	if err != nil {
		if Debug {
			log.Printf("syscall.Wait4: opts:%d wpid:%d err:%s", opts, wpid, err)
		}
		return err
	} else {
		if Debug {
			log.Printf("syscall.Wait4 opts:%d wpid:%d exited:%t exitStatus:%d signaled:%t stopped:%t continued:%t coreDump:%t", opts, wpid, ws.Exited(), ws.ExitStatus(), ws.Signaled(), ws.Stopped(), ws.Continued(), ws.CoreDump())
		}
		if ws.Stopped() || ws.Continued() {
			return nil
		}
		waits.Lock()
		defer waits.Unlock()
		if a, ok := waits.w[wpid]; ok {
			a.ws = ws
			a.TryLock()
			a.Unlock()
			runtime.Gosched()
			delete(waits.w, wpid)
		}
		for a, p := range waits.run {
			if a.Process != nil && a.Process.Pid == wpid {
				p.ws = ws
				p.TryLock()
				p.Unlock()
				runtime.Gosched()
				delete(waits.run, a)
			}
		}
	}
	return nil
}
