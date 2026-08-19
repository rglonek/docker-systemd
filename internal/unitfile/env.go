package unitfile

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// EnvFile is one EnvironmentFile= entry. A leading `-` on the filename means
// "ignore if missing".
type EnvFile struct {
	Path     string `json:"path"`
	Optional bool   `json:"optional,omitempty"`
}

// ParseEnvFileRef splits the optional `-` prefix off an EnvironmentFile= value.
func ParseEnvFileRef(v string) EnvFile {
	if strings.HasPrefix(v, "-") {
		return EnvFile{Path: strings.TrimSpace(v[1:]), Optional: true}
	}
	return EnvFile{Path: strings.TrimSpace(v)}
}

// ParseEnvironmentAssignments implements the Environment= grammar: several
// space-separated assignments per line, with quoting (defect C4).
//
//	Environment="A=1 2" B=3   ->  A="1 2", B="3"
func ParseEnvironmentAssignments(value string) ([]string, error) {
	tokens, err := Tokenize(value)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		s := literalOf(t)
		if s == "" {
			continue
		}
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("environment assignment %q is not KEY=VALUE", s)
		}
		if !validVarName(s[:eq]) {
			return nil, fmt.Errorf("invalid environment variable name %q", s[:eq])
		}
		out = append(out, s)
	}
	return out, nil
}

// ParseEnvironmentFile reads a systemd-style environment file (05 §7.3). The
// v0.5.x implementation split the content on "\n" with no comment, blank-line,
// quote or `export ` handling, so comment lines became environment entries
// (defect C3).
func ParseEnvironmentFile(r io.Reader) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var pending strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if cont, body := splitContinuation(line); cont {
			pending.WriteString(body)
			continue
		}
		if pending.Len() > 0 {
			pending.WriteString(line)
			line = pending.String()
			pending.Reset()
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed[0] == '#' {
			continue
		}
		trimmed = strings.TrimPrefix(trimmed, "export ")
		trimmed = strings.TrimSpace(trimmed)
		eq := strings.IndexByte(trimmed, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:eq])
		if !validVarName(key) {
			continue
		}
		val := strings.TrimSpace(trimmed[eq+1:])
		val = unquoteEnvValue(val)
		out = append(out, key+"="+val)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadEnvironmentFile reads an EnvironmentFile= entry, honouring the optional
// marker.
func LoadEnvironmentFile(f EnvFile) ([]string, error) {
	fh, err := os.Open(f.Path)
	if err != nil {
		if f.Optional && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer fh.Close()
	return ParseEnvironmentFile(fh)
}

// unquoteEnvValue strips surrounding quotes and processes backslash escapes
// inside double quotes only.
func unquoteEnvValue(v string) string {
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		inner := v[1 : len(v)-1]
		var b strings.Builder
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\\' && i+1 < len(inner) {
				r, n, err := unescape(inner, i)
				if err == nil {
					b.WriteRune(r)
					i += n - 1
					continue
				}
			}
			b.WriteByte(inner[i])
		}
		return b.String()
	}
	return v
}

// EnvMap converts a KEY=VALUE slice into a map, later entries winning.
// Splitting on the first `=` only is deliberate: v0.5.x used
// strings.Split(v, "=")[1], truncating any value containing `=` (defect C16).
func EnvMap(entries []string) map[string]string {
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		if eq := strings.IndexByte(e, '='); eq > 0 {
			m[e[:eq]] = e[eq+1:]
		}
	}
	return m
}

// EnvSlice converts a map back into a sorted-by-insertion KEY=VALUE slice
// suitable for execve. Order follows keys so the result is deterministic.
func EnvSlice(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
