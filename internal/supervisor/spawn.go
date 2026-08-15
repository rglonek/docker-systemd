package supervisor

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"docker-systemd/internal/unitfile"
	"golang.org/x/sys/unix"
)

// process is a spawned unit process.
type process struct {
	pid      int
	exit     <-chan ExitStatus
	status   *os.File // read end of the exec-status pipe
	cmd      unitfile.Command
	execDone chan error // closed when the exec outcome is known
}

// credentials is the resolved User=/Group= identity.
type credentials struct {
	uid    *int
	gid    *int
	groups []int
	name   string
	home   string
	shell  string
}

// resolveCredentials looks up User= and Group=.
//
// A numeric value is used directly and does not require a passwd entry — many
// container images have no passwd entry for the uid a unit wants. A name that
// cannot be resolved fails the unit with 217/USER rather than falling back to
// root.
func resolveCredentials(svc *unitfile.ServiceSection) (credentials, error) {
	var c credentials
	if svc.User == "" && svc.Group == "" && len(svc.SupplementaryGroups) == 0 {
		return c, nil
	}

	c.name = svc.User
	c.home = "/root"
	c.shell = "/bin/sh"

	if svc.User != "" {
		if n, err := strconv.Atoi(svc.User); err == nil {
			uid := n
			c.uid = &uid
			if u, err := user.LookupId(svc.User); err == nil {
				c.name, c.home = u.Username, u.HomeDir
				if gid, err := strconv.Atoi(u.Gid); err == nil {
					c.gid = &gid
				}
			}
		} else {
			u, err := user.Lookup(svc.User)
			if err != nil {
				return c, fmt.Errorf("User=%s: %w", svc.User, err)
			}
			uid, err := strconv.Atoi(u.Uid)
			if err != nil {
				return c, fmt.Errorf("User=%s: bad uid %q", svc.User, u.Uid)
			}
			gid, _ := strconv.Atoi(u.Gid)
			c.uid, c.gid = &uid, &gid
			c.name, c.home = u.Username, u.HomeDir
			// A user's own group memberships are part of its identity;
			// v0.5.x set no supplementary groups at all, so a service
			// dropped to www-data lost access to group-readable files
			// (defect C6).
			if gids, err := u.GroupIds(); err == nil {
				for _, g := range gids {
					if n, err := strconv.Atoi(g); err == nil {
						c.groups = append(c.groups, n)
					}
				}
			}
		}
	}

	if svc.Group != "" {
		gid, err := lookupGID(svc.Group)
		if err != nil {
			return c, fmt.Errorf("Group=%s: %w", svc.Group, err)
		}
		c.gid = &gid
	}

	for _, g := range svc.SupplementaryGroups {
		gid, err := lookupGID(g)
		if err != nil {
			return c, fmt.Errorf("SupplementaryGroups=%s: %w", g, err)
		}
		c.groups = append(c.groups, gid)
	}
	return c, nil
}

func lookupGID(name string) (int, error) {
	if n, err := strconv.Atoi(name); err == nil {
		return n, nil
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}

// buildEnvironment assembles the unit's environment in the precedence order of
// 05 §7.3, lowest first.
func (s *Supervisor) buildEnvironment(cred credentials) (map[string]string, []unitfile.Warning) {
	var warns []unitfile.Warning
	env := map[string]string{}

	// 1. The container's environment, which units genuinely rely on because it
	//    is how `docker run -e` reaches them, plus a fixed base.
	for _, e := range s.cfg.Environment {
		if eq := strings.IndexByte(e, '='); eq > 0 {
			env[e[:eq]] = e[eq+1:]
		}
	}
	if env["PATH"] == "" {
		env["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env["INVOCATION_ID"] = s.unit.InvocationID
	// The invocation marker is what orphan recovery searches /proc/*/environ
	// for when a supervisor dies (03 §6.8).
	env["MANAGED_BY_UNIT"] = s.unit.Name
	env["MANAGED_BY_INVOCATION"] = s.unit.InvocationID
	// Kept for compatibility with units written against v0.5.x.
	env["SYSTEMD_SERVICE_NAME"] = s.unit.Name

	svc := s.unit.Service

	// 3. EnvironmentFile=, in declaration order; later files win.
	for _, f := range svc.EnvironmentFiles {
		entries, err := unitfile.LoadEnvironmentFile(f)
		if err != nil {
			warns = append(warns, unitfile.Warning{
				Unit: s.unit.Name, Directive: "EnvironmentFile", Value: f.Path,
				Reason: err.Error(), Action: "ignored",
			})
			continue
		}
		for _, e := range entries {
			if eq := strings.IndexByte(e, '='); eq > 0 {
				env[e[:eq]] = e[eq+1:]
			}
		}
	}

	// 4. Environment=, in declaration order.
	for _, e := range svc.Environment {
		if eq := strings.IndexByte(e, '='); eq > 0 {
			env[e[:eq]] = e[eq+1:]
		}
	}

	// User-derived variables, which systemd exports for the target user.
	if cred.name != "" {
		env["USER"] = cred.name
		env["LOGNAME"] = cred.name
		env["HOME"] = cred.home
		if cred.shell != "" {
			env["SHELL"] = cred.shell
		}
	}

	for _, k := range svc.UnsetEnvironment {
		delete(env, k)
	}
	return env, warns
}

// spawn starts one command under the supervisor, wiring stdio and returning a
// handle. Every unit process goes through the `--exec-helper` trampoline, so
// there is exactly one code path for the execution context.
func (s *Supervisor) spawn(cmd unitfile.Command, env map[string]string, cred credentials, isMain bool) (*process, error) {
	argv := cmd.Expand(env)
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command line")
	}
	path, err := unitfile.ResolveExecutable(argv[0], isExecutable)
	if err != nil {
		return nil, fmt.Errorf("%w (status=203/EXEC)", err)
	}
	if cmd.ArgvZero != "" {
		argv = append([]string{cmd.ArgvZero}, argv[1:]...)
	} else {
		argv[0] = path
	}

	spec := ExecSpec{
		Path: path,
		Argv: argv,
		Env:  unitfile.EnvSlice(env),
	}
	svc := s.unit.Service
	spec.Dir = svc.WorkingDirectory
	spec.DirOptional = svc.WorkingDirOptional
	spec.UMask = svc.UMask
	spec.Nice = svc.Nice
	spec.OOMScoreAdjust = svc.OOMScoreAdjust
	spec.NoNewPrivileges = svc.NoNewPrivileges
	if len(svc.Limits) > 0 {
		spec.Limits = map[string]Limit{}
		for k, v := range svc.Limits {
			spec.Limits[k] = Limit{Soft: v.Soft, Hard: v.Hard}
		}
	}
	// The `+`, `!` and `!!` prefixes run the command with full privileges,
	// ignoring User=/Group=.
	if !cmd.Privileged {
		spec.UID, spec.GID, spec.Groups = cred.uid, cred.gid, cred.groups
	}

	specR, specW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	statusR, statusW, err := os.Pipe()
	if err != nil {
		specR.Close()
		specW.Close()
		return nil, err
	}

	stdin, stdout, stderr, closers, err := s.stdio(isMain)
	if err != nil {
		specR.Close()
		specW.Close()
		statusR.Close()
		statusW.Close()
		return nil, err
	}

	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}
	attr := &os.ProcAttr{
		// setsid() detaches the unit from init's controlling tty, which is
		// what lets `docker attach` + Ctrl-C reach init without stray-killing
		// services. It also creates a new process group, so no separate
		// setpgid is needed (and setpgid(0,0) on a session leader would fail).
		Sys:   &syscall.SysProcAttr{Setsid: true},
		Files: []*os.File{stdin, stdout, stderr, specR, statusW},
		Env:   []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
	}
	proc, err := os.StartProcess(self, []string{"docker-systemd-exec-helper", "--exec-helper"}, attr)
	// The child holds its own copies now.
	specR.Close()
	statusW.Close()
	for _, c := range closers {
		c.Close()
	}
	if err != nil {
		specW.Close()
		statusR.Close()
		return nil, err
	}

	// Register the waiter before the spec is delivered, so the process cannot
	// exit before we are listening for it.
	exit := s.reaper.Register(proc.Pid)

	if err := json.NewEncoder(specW).Encode(spec); err != nil {
		specW.Close()
		statusR.Close()
		_ = proc.Kill()
		s.reaper.Forget(proc.Pid)
		return nil, err
	}
	specW.Close()

	return &process{pid: proc.Pid, exit: exit, status: statusR, cmd: cmd}, nil
}

// execOutcome reads the trampoline's status pipe. EOF means execve succeeded;
// a message means it did not, and carries the systemd-conventional code.
func (p *process) execOutcome() (int, string) {
	if p.status == nil {
		return 0, ""
	}
	data, _ := io.ReadAll(p.status)
	p.status.Close()
	p.status = nil
	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0, ""
	}
	code := ExitExec
	msg := s
	if sp := strings.IndexByte(s, ' '); sp > 0 {
		if n, err := strconv.Atoi(s[:sp]); err == nil {
			code, msg = n, s[sp+1:]
		}
	}
	return code, msg
}

// isExecutable reports whether path names a regular executable file.
func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return unix.Access(path, unix.X_OK) == nil
}
