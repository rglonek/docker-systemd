# docker-systemd

A mostly-compatible systemd-like init system for docker.

The binary installs helpers and behaves like systemd, reading service files, executing enabled startup services, and allowing the use of common management tools, such as `systemctl` and `journalctl`. This makes docker containers behave more like proper virtual machines.

Note that only `.service` files are supported. Timer events as well as sockets and other unit file types are not supported.

Support is given to multiple systemd service file locations, as well as multi-instance service files and basic dependency handling for dependent services.

## Quickstart: getting started with prebuilt images

Prebuild images are available from latest with just the binary entrypoint added.

```
docker run -itd robertglonek/ubuntu:24.04
docker run -itd robertglonek/ubuntu:22.04
docker run -itd robertglonek/ubuntu:20.04

docker run -itd robertglonek/debian:12
docker run -itd robertglonek/debian:11
docker run -itd robertglonek/debian:10
docker run -itd robertglonek/debian:9

docker run -itd robertglonek/rockylinux:9
docker run -itd robertglonek/rockylinux:8

docker run -itd robertglonek/centos:stream9
```

## TL;DR Quickstart please

See [QUICKSTART.md](QUICKSTART.md) for a getting started guide.

## Behaviours and startup parameters

The binary will symlink itslf to the first possible local directory specified in `$PATH`, to the following names `journalctl,systemctl,service,poweroff,shutdown,systemd,init`. Running using the relevant names will result in that behaviour being triggered.

The following command line switches can be provided to systemd/init process on startup of the container to modify the behaviour:

Parameter | Description
--- | ---
`--log-to-stderr` | Will cause logging of all started services to be sent to stderr, this allows `docker logs` to view all service logs
`--no-logfile` | By default all services are logged to `/var/log/services/{SERVICENAME}.log`; this paramter disables the logging behaviour. Note that this will make `journalctl` not work, as it reads from that directory.
`--no-pidtrack` | Inside unprivileged docker containers, there is not cgroup access. This makes tracking many forking services extremely difficult. This system employs ingection of a wrapper to `execve` and `fork` calls, which allows for precise PID tracking. Use this paramter to disable wrapping of `libc` function calls (for example only ever starting non-forking services).

## Supported Commands

Command | Description
--- | ---
`journalctl` | Most common parameters are provided; the underlying system just reads the service files from `/var/log/services/`, which is where `systemd` puts the service logs
`systemctl` | Most common parameters are provided; mostly compatible, including `daemon-reload, start, stop, restart, status, enable, disable, mask, unmask, show, list`, though output format may vary from original
`poweroff/shutdown` | Executing this inside the container will cause systemd to perform a clean controlled shutdown
`service` | Old-school `service NAME start/stop/restart...` is also provided, symlinks behaviour to `systemctl start/stop/restart... NAME`
`systemd/init` | This is the init system which starts the whole thing up, should be used as the entrypoint to the container

## Supported systemd features

* parse `service` unit files
* autostart `multi-user.target` service units
* handle masking, unmasking, enabling and disabling services
* handle start/stop/restart as well as provide a `daemon-reload`` feature
* provide added features, such as `status, list, show` which provide service status, service list or the parsed definition of the service file, respectively
* handles receiving and tracking service start/stop signals and correctly reaps processes (no zombies)
* provides a `create-instance` and `delete-instance` set of commands; instances created will exist until they are deleted (they can be enabled, disabled, started, stoppped, etc); instances will be auto-created on `enable,start` commands

## Systemctl parameters

```
# systemctl -h
Usage:
  init [OPTIONS] <command>

Available commands:
  create-instance  create a new instance (for multi-instance services)
  daemon-reload    reload unit files
  delete-instance  delete an instance (for multi-instance services)
  disable          disable services
  enable           enable services
  list             list services
  mask             mask a service
  poweroff         shutdown the system
  reload           reload a service (send SIGHUP)
  restart          restart a service
  show             show details of a service
  start            start a service
  status           status of a service
  stop             stop a service
  unmask           unmask a service
```

## Journalctl parameters

```
# journalctl -h
Usage:
  journalctl

Application Options:
  -S, --since=    format: 2012-10-30 18:17:16
  -U, --until=    format: 2012-10-30 18:17:16
  -b, --boot      since reboot
  -u, --unit=     unit name
  -n, --lines=    show max X last lines
  -f, --follow    follow log; implies lines
      --no-pager  do not page results; implied with follow
  -h, --help      display help
```

## Service file supported definitions

### Supported

The following is a list of supported definitions in the unit files; these should be as closely compatible as possible with the original implementation, possibly with some limitations and workarounds.

```go
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
	OnFailure    map[string]*daemon
	OnSuccess    map[string]*daemon
	// behaviour
	StopWhenUnneeded bool
	FailureAction    string
	SuccessAction    string
	// service section
	ServiceType      string // NOTE: treats all as either simple/oneshot/forking, no support for dbus (treats as forking)
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
	Restart          string // NOTE: basic always/on-failure/on-success are supported, anything else is auto-mapped to one of those 3
	WorkingDirectory string
	User             string
	Group            string
	Env              []string
	EnvFile          []string
```

## Planned

These 2 features have not been implemented yet, and service start order is only controlled by dependencies (Wants/WantedBy/etc). Extra ordering by these parameters is not yet in.

```golang
	Before       map[string]*daemon
	After        map[string]*daemon
```

## Special

The following will provide a WARNING in `docker logs` during startup, but will otherwise be ignored. This is due to permissions in default docker capabilities. Use Docker's limit setting command line instead when starting containers.

```golang
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
```
