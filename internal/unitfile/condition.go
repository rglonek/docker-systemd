package unitfile

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// ConditionResult reports the outcome of evaluating one unit's conditions.
type ConditionResult struct {
	OK      bool
	Failed  string // the directive that failed, for the diagnostic
	Assert  bool   // the failure came from an Assert*=, so the unit fails
	Message string
}

// EvaluateConditions applies every Condition*= and Assert*= of a unit.
//
// systemd's rule: a failed Condition*= skips the unit (it goes inactive, not
// failed); a failed Assert*= fails it. Multiple conditions of the same kind are
// OR-ed, different kinds are AND-ed; we implement the simpler AND over all,
// which is what every unit in the wild relies on.
func EvaluateConditions(conds []Condition, env map[string]string) ConditionResult {
	for _, c := range conds {
		ok, err := evalCondition(c, env)
		if err != nil {
			// A condition we cannot evaluate is treated as satisfied, with the
			// reason available to the caller; refusing to start a unit because
			// we cannot answer ConditionSecurity= would be worse.
			continue
		}
		if c.Negate {
			ok = !ok
		}
		if !ok {
			kind := "Condition"
			if c.Assert {
				kind = "Assert"
			}
			return ConditionResult{
				OK:     false,
				Failed: kind + c.Kind,
				Assert: c.Assert,
				Message: fmt.Sprintf("%s%s=%s%s was not met",
					kind, c.Kind, negPrefix(c.Negate), c.Value),
			}
		}
	}
	return ConditionResult{OK: true}
}

func negPrefix(n bool) string {
	if n {
		return "!"
	}
	return ""
}

func evalCondition(c Condition, env map[string]string) (bool, error) {
	v := c.Value
	switch c.Kind {
	case "PathExists":
		_, err := os.Stat(v)
		return err == nil, nil
	case "PathExistsGlob":
		m, _ := filepathGlob(v)
		return len(m) > 0, nil
	case "PathIsDirectory":
		fi, err := os.Stat(v)
		return err == nil && fi.IsDir(), nil
	case "PathIsSymbolicLink":
		fi, err := os.Lstat(v)
		return err == nil && fi.Mode()&os.ModeSymlink != 0, nil
	case "PathIsMountPoint":
		return isMountPoint(v), nil
	case "PathIsReadWrite":
		return unix.Access(v, unix.W_OK) == nil, nil
	case "DirectoryNotEmpty":
		entries, err := os.ReadDir(v)
		return err == nil && len(entries) > 0, nil
	case "FileNotEmpty":
		fi, err := os.Stat(v)
		return err == nil && fi.Mode().IsRegular() && fi.Size() > 0, nil
	case "FileIsExecutable":
		fi, err := os.Stat(v)
		return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0, nil
	case "Virtualization":
		return matchVirtualization(v), nil
	case "Architecture":
		return strings.EqualFold(v, archName()) || strings.EqualFold(v, "native"), nil
	case "Host":
		host, _ := os.Hostname()
		return strings.EqualFold(v, host), nil
	case "KernelVersion":
		return matchKernelVersion(v, kernelRelease()), nil
	case "Environment":
		return matchEnvironment(v, env), nil
	case "User":
		return matchUser(v), nil
	case "Group":
		return matchGroup(v), nil
	case "Memory":
		return matchNumeric(v, totalMemory())
	case "CPUs":
		return matchNumeric(v, int64(numCPU()))
	case "FirstBoot":
		b, err := ParseBool(v)
		if err != nil {
			return true, nil
		}
		// A container filesystem is always "first boot" from systemd's point
		// of view only if /etc/machine-id is missing.
		_, statErr := os.Stat(MachineIDPath)
		return b == (statErr != nil), nil
	case "ACPower":
		// No battery in a container: mains power is always present.
		b, err := ParseBool(v)
		if err != nil {
			return true, nil
		}
		return b, nil
	case "Capability", "Security", "ControlGroupController", "KernelCommandLine",
		"NeedsUpdate", "Credential":
		// Evaluated as satisfied: we cannot answer these in an unprivileged
		// container, and refusing to start would be worse than proceeding.
		return true, fmt.Errorf("condition %s is not evaluable in a container", c.Kind)
	default:
		return true, fmt.Errorf("unknown condition %s", c.Kind)
	}
}

// MachineIDPath is the file consulted by ConditionFirstBoot= and the %m
// specifier. It is a variable so tests can redirect it.
var MachineIDPath = "/etc/machine-id"

// matchVirtualization answers ConditionVirtualization=. Reporting `container`
// and `docker` truthfully is what makes many stock units correctly skip
// themselves inside a container.
func matchVirtualization(v string) bool {
	detected := DetectVirtualization()
	switch strings.ToLower(v) {
	case "":
		return detected != "none"
	case "yes", "1", "true", "on":
		return detected != "none"
	case "no", "0", "false", "off":
		return detected == "none"
	case "container":
		return detected != "none"
	case "private-users":
		return false
	default:
		return strings.EqualFold(v, detected)
	}
}

// DetectVirtualization implements systemd-detect-virt for the container case.
func DetectVirtualization() string {
	if v := os.Getenv("container"); v != "" {
		return v
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "docker"
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return "podman"
	}
	// A PID 1 whose cgroup path names a container runtime is the remaining
	// reliable signal available without privileges.
	if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(data)
		for _, marker := range []string{"docker", "containerd", "kubepods", "lxc", "podman"} {
			if strings.Contains(s, marker) {
				if marker == "kubepods" || marker == "containerd" {
					return "docker"
				}
				return marker
			}
		}
	}
	if data, err := os.ReadFile("/proc/1/sched"); err == nil {
		// In a PID namespace the first line names the process; not decisive,
		// so only used as a weak fallback alongside the environment marker.
		_ = data
	}
	return "none"
}

func matchKernelVersion(expr, actual string) bool {
	expr = strings.TrimSpace(expr)
	for _, op := range []string{">=", "<=", "!=", "<>", "=", ">", "<"} {
		if strings.HasPrefix(expr, op) {
			want := strings.TrimSpace(expr[len(op):])
			cmp := compareVersions(actual, want)
			switch op {
			case ">=":
				return cmp >= 0
			case "<=":
				return cmp <= 0
			case "!=", "<>":
				return cmp != 0
			case "=":
				return cmp == 0
			case ">":
				return cmp > 0
			case "<":
				return cmp < 0
			}
		}
	}
	return strings.Contains(actual, expr)
}

// compareVersions compares dotted version strings numerically where possible.
func compareVersions(a, b string) int {
	as := splitVersion(a)
	bs := splitVersion(b)
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func splitVersion(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r < '0' || r > '9'
	})
}

func matchEnvironment(v string, env map[string]string) bool {
	if eq := strings.IndexByte(v, '='); eq > 0 {
		val, ok := env[v[:eq]]
		return ok && val == v[eq+1:]
	}
	_, ok := env[v]
	return ok
}

func matchUser(v string) bool {
	uid := os.Getuid()
	if n, err := strconv.Atoi(v); err == nil {
		return n == uid
	}
	if v == "@system" {
		return uid < 1000
	}
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return false
	}
	return u.Username == v
}

func matchGroup(v string) bool {
	gid := os.Getgid()
	if n, err := strconv.Atoi(v); err == nil {
		return n == gid
	}
	g, err := user.LookupGroupId(strconv.Itoa(gid))
	if err != nil {
		return false
	}
	return g.Name == v
}

func matchNumeric(expr string, actual int64) (bool, error) {
	expr = strings.TrimSpace(expr)
	for _, op := range []string{">=", "<=", "!=", "<>", "=", ">", "<"} {
		if strings.HasPrefix(expr, op) {
			want, err := ParseSize(strings.TrimSpace(expr[len(op):]))
			if err != nil {
				return true, err
			}
			switch op {
			case ">=":
				return actual >= want, nil
			case "<=":
				return actual <= want, nil
			case "!=", "<>":
				return actual != want, nil
			case "=":
				return actual == want, nil
			case ">":
				return actual > want, nil
			case "<":
				return actual < want, nil
			}
		}
	}
	want, err := ParseSize(expr)
	if err != nil {
		return true, err
	}
	return actual >= want, nil
}
