package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"docker-systemd/internal/paths"
	"docker-systemd/internal/proto"
	"docker-systemd/internal/unitfile"
	"golang.org/x/sys/unix"
)

// listenControl binds the control socket inside the 0700 runtime directory and
// chmods it to 0600.
//
// v0.5.x put it at /tmp/docker-systemd.sock with the process umask in a
// world-writable directory, so any uid in the container could start a root
// unit or power the container off (defect E1).
func (m *Manager) listenControl() (net.Listener, error) {
	_ = os.Remove(paths.ControlSocket)
	ln, err := net.Listen("unix", paths.ControlSocket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(paths.ControlSocket, paths.ModeControlSck); err != nil {
		ln.Close()
		return nil, err
	}
	m.log.Infof("control socket listening on %s (mode 0600 in a 0700 directory)", paths.ControlSocket)
	if len(m.opts.ControlAllowUID) > 0 || len(m.opts.ControlAllowGID) > 0 {
		m.log.Infof("control access policy: uid 0 plus uids %v and gids %v",
			m.opts.ControlAllowUID, m.opts.ControlAllowGID)
	}

	if m.opts.CompatTmpSocket {
		m.log.Warnf("--compat-tmp-socket: creating the legacy %s symlink. "+
			"This is insecure and will be removed in a future release.", paths.LegacySocket)
		_ = os.Remove(paths.LegacySocket)
		if err := os.Symlink(paths.ControlSocket, paths.LegacySocket); err != nil {
			m.log.Warnf("cannot create %s: %v", paths.LegacySocket, err)
		}
	}
	return ln, nil
}

func (m *Manager) serveControl(ln net.Listener) {
	defer ln.Close()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if m.shuttingDown.Load() {
				return
			}
			m.log.Warnf("control accept: %v", err)
			continue
		}
		go m.handleConn(conn)
	}
}

// peerCred reads the kernel-supplied peer credentials. SO_PEERCRED is filled in
// at connect(2) time and cannot be spoofed by the client.
func peerCred(conn net.Conn) (*unix.Ucred, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var cerr error
	err = raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err != nil {
		return nil, err
	}
	return cred, cerr
}

// authorised applies the default uid-0-only policy plus any widening flags.
func (m *Manager) authorised(cred *unix.Ucred) bool {
	if cred.Uid == 0 {
		return true
	}
	for _, u := range m.opts.ControlAllowUID {
		if uint32(u) == cred.Uid {
			return true
		}
	}
	for _, g := range m.opts.ControlAllowGID {
		if uint32(g) == cred.Gid {
			return true
		}
	}
	return false
}

func (m *Manager) handleConn(conn net.Conn) {
	defer conn.Close()
	defer func() {
		if r := recover(); r != nil {
			m.log.Errorf("control connection panic: %v", r)
			_ = proto.WriteJSON(conn, proto.TypeResult, proto.Result{
				ExitCode: 1, ErrorKind: proto.ErrKindFailed,
				Message: fmt.Sprintf("internal error: %v", r),
			})
		}
	}()

	cred, err := peerCred(conn)
	if err != nil {
		m.log.Warnf("cannot read peer credentials: %v", err)
		return
	}
	if !m.authorised(cred) {
		m.log.Warnf("denied control connection from uid %d pid %d", cred.Uid, cred.Pid)
		_ = proto.WriteJSON(conn, proto.TypeResult, proto.Result{
			ExitCode: 1, ErrorKind: proto.ErrKindDenied,
			Message: "Access denied: only uid 0 may control the manager",
		})
		return
	}

	f, err := proto.Read(conn)
	if err != nil {
		return
	}
	if f.Type != proto.TypeHello {
		_ = proto.WriteJSON(conn, proto.TypeResult, proto.Result{
			ExitCode: 1, ErrorKind: proto.ErrKindInvalidArgs,
			Message: "expected HELLO",
		})
		return
	}
	var hello proto.Hello
	_ = json.Unmarshal(f.Payload, &hello)
	if err := proto.WriteJSON(conn, proto.TypeHelloAck, proto.HelloAck{
		Version: proto.ProtocolVersion,
		Server:  m.opts.Version,
	}); err != nil {
		return
	}
	if hello.Version != proto.ProtocolVersion {
		_ = proto.WriteJSON(conn, proto.TypeResult, proto.Result{
			ExitCode: 1, ErrorKind: proto.ErrKindInvalidArgs,
			Message: fmt.Sprintf(
				"protocol version mismatch (client %d, manager %d). "+
					"The systemctl binary is stale; re-run the installer.",
				hello.Version, proto.ProtocolVersion),
		})
		return
	}

	for {
		f, err := proto.Read(conn)
		if err != nil {
			return
		}
		if f.Type != proto.TypeRequest {
			continue
		}
		var req proto.Request
		if err := json.Unmarshal(f.Payload, &req); err != nil {
			_ = proto.WriteJSON(conn, proto.TypeResult, proto.Result{
				ExitCode: 2, ErrorKind: proto.ErrKindInvalidArgs,
				Message: "malformed request",
			})
			continue
		}
		m.auditLog(cred, req)
		res := m.dispatch(conn, req)
		_ = proto.WriteJSON(conn, proto.TypeResult, res)
	}
}

// mutatingVerbs are audit-logged with the caller's identity.
var mutatingVerbs = map[string]bool{
	"start": true, "stop": true, "restart": true, "try-restart": true,
	"reload": true, "reload-or-restart": true, "try-reload-or-restart": true,
	"kill": true, "isolate": true, "enable": true, "disable": true,
	"reenable": true, "preset": true, "preset-all": true, "mask": true,
	"unmask": true, "link": true, "revert": true, "add-wants": true,
	"add-requires": true, "daemon-reload": true, "daemon-reexec": true,
	"set-environment": true, "unset-environment": true, "import-environment": true,
	"poweroff": true, "halt": true, "reboot": true, "default": true,
	"rescue": true, "emergency": true, "reset-failed": true,
	"create-instance": true, "delete-instance": true,
}

func (m *Manager) auditLog(cred *unix.Ucred, req proto.Request) {
	if !mutatingVerbs[req.Verb] {
		m.log.Debugf("control: uid=%d pid=%d verb=%s units=%v",
			cred.Uid, cred.Pid, req.Verb, req.Units)
		return
	}
	m.log.Infof("control: uid=%d pid=%d verb=%s units=%v",
		cred.Uid, cred.Pid, req.Verb, req.Units)
}

// progressWriter streams PROGRESS frames for human display, suppressed under
// --quiet.
func progressWriter(conn net.Conn, quiet bool) func(string) {
	if quiet {
		return func(string) {}
	}
	return func(s string) { _ = proto.WriteText(conn, proto.TypeProgress, s) }
}

// dispatch routes one verb. An unknown verb returns exit code 2, never 0:
// v0.5.x's flag parser returned 0 for an unknown command, so a maintainer
// script's `systemctl --no-block start foo` looked like it succeeded while
// doing nothing (defect D6).
func (m *Manager) dispatch(conn net.Conn, req proto.Request) proto.Result {
	if m.shuttingDown.Load() && mutatingVerbs[req.Verb] {
		return proto.Result{ExitCode: 1, ErrorKind: proto.ErrKindShutdown,
			Message: "Manager is shutting down"}
	}
	progress := progressWriter(conn, req.Options.Quiet)

	switch req.Verb {
	case "version":
		_ = proto.WriteJSON(conn, proto.TypeData, map[string]string{"version": m.opts.Version})
		return ok()

	case "capabilities":
		_ = proto.WriteJSON(conn, proto.TypeData, m.Capabilities())
		return ok()

	case "start":
		return m.resultOf(m.StartUnits(req.Units, progress))

	case "stop":
		return m.resultOf(m.StopUnits(req.Units, progress))

	case "restart":
		return m.resultOf(m.RestartUnits(req.Units, false, progress))

	case "try-restart", "condrestart":
		return m.resultOf(m.RestartUnits(req.Units, true, progress))

	case "reload":
		var err error
		for _, u := range req.Units {
			if e := m.reloadUnit(u); e != nil {
				err = e
			}
		}
		return m.resultOf(err)

	case "reload-or-restart", "force-reload":
		return m.resultOf(m.ReloadOrRestart(req.Units, false, progress))

	case "try-reload-or-restart":
		return m.resultOf(m.ReloadOrRestart(req.Units, true, progress))

	case "kill":
		var err error
		for _, u := range req.Units {
			if e := m.killUnit(u, req.Options.Signal, req.Options.KillWho); e != nil {
				err = e
			}
		}
		return m.resultOf(err)

	case "isolate":
		if len(req.Units) != 1 {
			return badArgs("isolate takes exactly one unit")
		}
		return m.resultOf(m.Isolate(req.Units[0], progress))

	case "status", "show":
		return m.verbStatus(conn, req)

	case "is-active":
		return m.verbPredicate(conn, req, func(st proto.UnitStatus) (string, bool) {
			return st.ActiveState, st.ActiveState == proto.StateActive || st.ActiveState == proto.StateReloading
		})

	case "is-failed":
		return m.verbPredicate(conn, req, func(st proto.UnitStatus) (string, bool) {
			return st.ActiveState, st.ActiveState == proto.StateFailed
		})

	case "is-enabled":
		return m.verbIsEnabled(conn, req)

	case "cat":
		var b strings.Builder
		for _, u := range req.Units {
			text, err := m.Cat(u)
			if err != nil {
				return proto.Result{ExitCode: 4, ErrorKind: proto.ErrKindNoSuchUnit, Message: err.Error()}
			}
			b.WriteString(text)
		}
		_ = proto.WriteJSON(conn, proto.TypeData, map[string]string{"text": b.String()})
		return ok()

	case "list-units", "list":
		_ = proto.WriteJSON(conn, proto.TypeData,
			m.ListUnits(req.Options.Types, req.Options.States, req.Options.All))
		return ok()

	case "list-unit-files":
		_ = proto.WriteJSON(conn, proto.TypeData, m.ListUnitFiles(req.Options.Types))
		return ok()

	case "list-dependencies":
		return m.verbListDependencies(conn, req)

	case "reset-failed":
		m.ResetFailed(req.Units)
		return ok()

	case "daemon-reload":
		m.DaemonReload()
		return ok()

	case "daemon-reexec":
		// Implemented as "reload and keep going", which 04 §10 permits for v1
		// provided it does not restart units.
		m.DaemonReload()
		return ok()

	case "enable", "disable", "reenable", "preset", "preset-all", "mask", "unmask",
		"link", "revert", "add-wants", "add-requires", "create-instance", "delete-instance":
		return m.verbInstall(conn, req, progress)

	case "show-environment":
		_ = proto.WriteJSON(conn, proto.TypeData, m.ShowEnvironment())
		return ok()

	case "set-environment":
		return m.resultOf(m.SetEnvironment(req.Args))

	case "unset-environment":
		m.UnsetEnvironment(req.Args)
		return ok()

	case "import-environment":
		var assigns []string
		for _, name := range req.Args {
			if v, present := os.LookupEnv(name); present {
				assigns = append(assigns, name+"="+v)
			}
		}
		return m.resultOf(m.SetEnvironment(assigns))

	case "poweroff", "halt":
		m.RequestShutdown(shutdownRequest{Reason: req.Verb})
		return ok()

	case "reboot":
		m.RequestShutdown(shutdownRequest{Reason: req.Verb, Reboot: true})
		return ok()

	case "default":
		return m.resultOf(m.Isolate(m.opts.DefaultTarget, progress))

	case "rescue":
		return m.resultOf(m.Isolate("rescue.target", progress))

	case "emergency":
		return m.resultOf(m.Isolate("emergency.target", progress))

	default:
		return proto.Result{
			ExitCode: 2, ErrorKind: proto.ErrKindInvalidArgs,
			Message: fmt.Sprintf("Unknown operation %s.", req.Verb),
		}
	}
}

func ok() proto.Result { return proto.Result{ExitCode: 0} }

func badArgs(msg string) proto.Result {
	return proto.Result{ExitCode: 2, ErrorKind: proto.ErrKindInvalidArgs, Message: msg}
}

func (m *Manager) resultOf(err error) proto.Result {
	if err == nil {
		return ok()
	}
	kind := proto.ErrKindFailed
	code := 1
	msg := err.Error()
	switch {
	case strings.Contains(msg, "is masked"):
		kind = proto.ErrKindMasked
	case strings.Contains(msg, "not found"):
		kind, code = proto.ErrKindNoSuchUnit, 5
	}
	return proto.Result{ExitCode: code, ErrorKind: kind, Message: msg}
}

func (m *Manager) verbStatus(conn net.Conn, req proto.Request) proto.Result {
	units := req.Units
	if len(units) == 0 {
		units = m.activeUnits()
	}
	var out []proto.UnitStatus
	exit := 0
	for _, name := range units {
		st := m.UnitState(name)
		if st.LoadState == "not-found" {
			exit = 4
		} else if st.ActiveState != proto.StateActive && st.ActiveState != proto.StateReloading && exit == 0 {
			exit = 3
		}
		out = append(out, st)
	}
	_ = proto.WriteJSON(conn, proto.TypeData, out)
	return proto.Result{ExitCode: exit}
}

func (m *Manager) verbPredicate(conn net.Conn, req proto.Request, pred func(proto.UnitStatus) (string, bool)) proto.Result {
	if len(req.Units) == 0 {
		return badArgs("expected at least one unit")
	}
	allTrue := true
	var values []string
	for _, name := range req.Units {
		st := m.UnitState(name)
		v, okv := pred(st)
		values = append(values, v)
		if !okv {
			allTrue = false
		}
	}
	_ = proto.WriteJSON(conn, proto.TypeData, values)
	if allTrue {
		return ok()
	}
	return proto.Result{ExitCode: 3}
}

func (m *Manager) verbIsEnabled(conn net.Conn, req proto.Request) proto.Result {
	if len(req.Units) == 0 {
		return badArgs("expected at least one unit")
	}
	reg := m.Registry()
	allEnabled := true
	var values []string
	for _, name := range req.Units {
		state := "not-found"
		if reg != nil {
			state = reg.UnitFileState(name)
		}
		values = append(values, state)
		if state != "enabled" && state != "static" && state != "alias" {
			allEnabled = false
		}
	}
	_ = proto.WriteJSON(conn, proto.TypeData, values)
	if allEnabled {
		return ok()
	}
	return proto.Result{ExitCode: 1}
}

func (m *Manager) verbListDependencies(conn net.Conn, req proto.Request) proto.Result {
	names := req.Units
	if len(names) == 0 {
		names = []string{m.opts.DefaultTarget}
	}
	out := map[string][]string{}
	var walk func(string, int)
	seen := map[string]bool{}
	walk = func(name string, depth int) {
		if depth > 16 || seen[name] {
			return
		}
		seen[name] = true
		u := m.Get(name)
		if u == nil {
			return
		}
		deps := append([]string{}, u.Unit.Requires...)
		deps = append(deps, u.Unit.Wants...)
		deps = append(deps, u.Unit.BindsTo...)
		deps = append(deps, m.InjectedWants(name)...)
		deps = append(deps, m.InjectedRequires(name)...)
		out[name] = dedupeSorted(deps)
		for _, d := range out[name] {
			walk(d, depth+1)
		}
	}
	for _, n := range names {
		walk(unitfile.CanonicalName(n), 0)
	}
	_ = proto.WriteJSON(conn, proto.TypeData, out)
	return ok()
}
