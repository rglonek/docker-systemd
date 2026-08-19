package unitfile

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// SpecifierContext supplies the values for %-specifier expansion (05 §7.1).
// It is a plain struct rather than a set of lookups so that expansion is pure
// and testable: everything that needs the filesystem is resolved once, by the
// manager, before parsing.
type SpecifierContext struct {
	UnitName  string // foo@bar.service
	Prefix    string // foo, escaped
	Instance  string // bar, escaped
	UserName  string
	UID       int
	Group     string
	GID       int
	Home      string
	Shell     string
	Hostname  string
	MachineID string
	BootID    string
	Kernel    string
	Arch      string
}

// DefaultSpecifierContext fills in the host-derived fields.
func DefaultSpecifierContext(unit string) SpecifierContext {
	host, _ := os.Hostname()
	ctx := SpecifierContext{
		UnitName: unit,
		UserName: "root",
		UID:      0,
		Group:    "root",
		GID:      0,
		Home:     "/root",
		Shell:    "/bin/sh",
		Hostname: host,
		Arch:     archName(),
		Kernel:   kernelRelease(),
	}
	ctx.Prefix, ctx.Instance = SplitTemplate(unit)
	return ctx
}

// SplitTemplate splits `foo@bar.service` into its prefix and instance. For a
// non-template name the instance is empty.
func SplitTemplate(unit string) (prefix, instance string) {
	name := unit
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	at := strings.IndexByte(name, '@')
	if at < 0 {
		return name, ""
	}
	return name[:at], name[at+1:]
}

// UnitSuffix returns the ".service"-style suffix of a unit name.
func UnitSuffix(unit string) string {
	if i := strings.LastIndexByte(unit, '.'); i >= 0 {
		return unit[i:]
	}
	return ""
}

// Unescape reverses systemd's path escaping: `-` becomes `/` and `\xNN`
// becomes the byte NN. This is %I / %P relative to %i / %p.
func Unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '-' {
			b.WriteByte('/')
			i++
			continue
		}
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 4
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// Escape applies systemd's path escaping: `/` becomes `-` and any other
// character outside [a-zA-Z0-9:_.] becomes `\xNN`.
func Escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case isAlpha(c) || (c >= '0' && c <= '9') || c == ':' || c == '_' || c == '.':
			b.WriteByte(c)
		default:
			b.WriteString(`\x`)
			const hex = "0123456789abcdef"
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		}
	}
	return b.String()
}

// ExpandSpecifiers replaces every %-specifier in s. Unknown specifiers are
// left as written, which matches systemd's behaviour of only expanding the set
// it knows.
//
// Unlike v0.5.x, which expanded only %i and %I and only inside Exec* values
// (defect C7), this is applied to every directive value systemd expands.
func (c SpecifierContext) ExpandSpecifiers(s string) string {
	if !strings.ContainsRune(s, '%') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		spec := s[i+1]
		i++
		switch spec {
		case '%':
			b.WriteByte('%')
		case 'n':
			b.WriteString(c.UnitName)
		case 'N':
			b.WriteString(strings.TrimSuffix(c.UnitName, UnitSuffix(c.UnitName)))
		case 'p':
			b.WriteString(c.Prefix)
		case 'P':
			b.WriteString(Unescape(c.Prefix))
		case 'i':
			b.WriteString(c.Instance)
		case 'I':
			b.WriteString(Unescape(c.Instance))
		case 'f':
			if c.Instance != "" {
				b.WriteString("/" + strings.TrimPrefix(Unescape(c.Instance), "/"))
			} else {
				b.WriteString("/" + strings.TrimPrefix(Unescape(c.Prefix), "/"))
			}
		case 'j':
			p := c.Prefix
			if k := strings.LastIndexByte(p, '-'); k >= 0 {
				p = p[k+1:]
			}
			b.WriteString(p)
		case 't':
			b.WriteString("/run")
		case 'S':
			b.WriteString("/var/lib")
		case 'C':
			b.WriteString("/var/cache")
		case 'L':
			b.WriteString("/var/log")
		case 'E':
			b.WriteString("/etc")
		case 'h':
			b.WriteString(c.Home)
		case 'u':
			b.WriteString(c.UserName)
		case 'U':
			b.WriteString(strconv.Itoa(c.UID))
		case 'g':
			b.WriteString(c.Group)
		case 'G':
			b.WriteString(strconv.Itoa(c.GID))
		case 'H':
			b.WriteString(c.Hostname)
		case 'm':
			b.WriteString(c.MachineID)
		case 'b':
			b.WriteString(c.BootID)
		case 'v':
			b.WriteString(c.Kernel)
		case 'a':
			b.WriteString(c.Arch)
		case 's':
			b.WriteString(c.Shell)
		default:
			b.WriteByte('%')
			b.WriteByte(spec)
		}
	}
	return b.String()
}

func archName() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86-64"
	case "386":
		return "x86"
	case "arm64":
		return "arm64"
	case "arm":
		return "arm"
	default:
		return runtime.GOARCH
	}
}
