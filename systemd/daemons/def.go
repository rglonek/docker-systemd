package daemons

import "errors"

// implements the Daemons interface for interacting with daemons
type Daemons interface {
	LoadAndStart() error
	Reload() error
	StopAll() error
	Find(name string) (Daemon, error)
	List() []string
}

// implements the Daemon interface for interactive with a single daemon
type Daemon interface {
	Name() string
	Start() error
	Stop() error
	Restart() error
	Enable() error
	Disable() error
	Mask() error
	Unmask() error
	Status() (string, error)
	Detail() string
	State() DaemonState
	Reload() error
	IsEnabled() bool
	CreateInstance(name string) error
	DeleteService() error
}

type DaemonState int

var ErrNotFound = errors.New("daemon not found")

const (
	StateRunning    = DaemonState(1)
	StateStopped    = DaemonState(2)
	StateStarting   = DaemonState(3)
	StateStopping   = DaemonState(4)
	StateRestarting = DaemonState(5)
)

func New() (Daemons, error) {
	d := new(daemons)
	d.list = make(map[string]*daemon)
	return d, d.LoadAndStart()
}
