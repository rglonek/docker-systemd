package unitfile

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// searchPath is the fixed PATH used to resolve a bare executable name in an
// Exec* line (systemd >= 239 relaxation, 05 §4.4).
var searchPath = []string{
	"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin",
}

// Piece is one segment of an argument: either a literal run of text or a
// reference to an environment variable.
//
// Keeping the distinction until spawn time is what allows systemd's word
// splitting rule to be honoured exactly: an unquoted $VAR splits on whitespace
// after expansion, ${VAR} and anything inside quotes does not.
type Piece struct {
	Literal string `json:"lit,omitempty"`
	Var     string `json:"var,omitempty"`
	Split   bool   `json:"split,omitempty"`
}

// Token is one pre-expansion argument.
type Token struct {
	Pieces []Piece `json:"pieces"`
}

// Command is one parsed Exec* line.
type Command struct {
	// Raw is the value as written, for diagnostics and `systemctl show`.
	Raw string `json:"raw"`
	// Tokens are the argv words before variable expansion. Tokens[0] is the
	// executable unless ArgvZero is set.
	Tokens []Token `json:"tokens"`
	// ArgvZero is the `@`-prefix override for argv[0].
	ArgvZero string `json:"argv0,omitempty"`
	// IgnoreFailure is the `-` prefix.
	IgnoreFailure bool `json:"ignore_failure,omitempty"`
	// NoExpand is the `:` prefix.
	NoExpand bool `json:"no_expand,omitempty"`
	// Privileged is the `+`, `!` or `!!` prefix: run without applying
	// User=/Group=.
	Privileged bool `json:"privileged,omitempty"`
	// File and Line locate the directive.
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
}

// String renders the command roughly as written, for `systemctl show`.
func (c Command) String() string { return c.Raw }

// ParseCommand parses one Exec* value: prefixes, then the tokenised argv.
func ParseCommand(value string) (Command, error) {
	cmd := Command{Raw: value}
	rest := strings.TrimLeft(value, " \t")

	// Prefixes may appear in any order and repeat; `!!` must be tested before
	// `!`.
prefixes:
	for {
		switch {
		case strings.HasPrefix(rest, "!!"):
			cmd.Privileged = true
			rest = rest[2:]
		case strings.HasPrefix(rest, "-"):
			cmd.IgnoreFailure = true
			rest = rest[1:]
		case strings.HasPrefix(rest, "@"):
			// The @ prefix means argv[0] is given separately: the first token
			// is the executable path, the second is argv[0].
			cmd.ArgvZero = "\x00pending"
			rest = rest[1:]
		case strings.HasPrefix(rest, ":"):
			cmd.NoExpand = true
			rest = rest[1:]
		case strings.HasPrefix(rest, "+"), strings.HasPrefix(rest, "!"):
			cmd.Privileged = true
			rest = rest[1:]
		default:
			break prefixes
		}
		rest = strings.TrimLeft(rest, " \t")
	}

	tokens, err := Tokenize(rest)
	if err != nil {
		return cmd, err
	}
	if len(tokens) == 0 {
		return cmd, fmt.Errorf("empty command line")
	}
	if cmd.ArgvZero == "\x00pending" {
		// path argv0 args...  ->  keep the path in Tokens[0] and record argv[0]
		// literally; a variable in argv[0] is expanded at spawn time from
		// Tokens[1], so keep the token there too.
		if len(tokens) < 2 {
			cmd.ArgvZero = ""
		} else {
			cmd.ArgvZero = literalOf(tokens[1])
		}
	}
	cmd.Tokens = tokens
	return cmd, nil
}

// literalOf flattens a token's literal pieces; used only for the `@` argv[0]
// override, which systemd does not variable-expand differently from the rest.
func literalOf(t Token) string {
	var b strings.Builder
	for _, p := range t.Pieces {
		if p.Var != "" {
			b.WriteString("$")
			b.WriteString(p.Var)
			continue
		}
		b.WriteString(p.Literal)
	}
	return b.String()
}

// Tokenize implements the command-line grammar of 05 §4.2: whitespace
// splitting, single and double quotes, C-style escapes and variable
// references. There is deliberately no glob expansion, no pipes, no
// redirection, no command substitution and no `~` — a `;` or `|` is an
// ordinary argument character.
func Tokenize(s string) ([]Token, error) {
	var out []Token
	var cur Token
	var lit strings.Builder
	started := false

	flushLit := func() {
		if lit.Len() > 0 {
			cur.Pieces = append(cur.Pieces, Piece{Literal: lit.String()})
			lit.Reset()
		}
	}
	endToken := func() {
		flushLit()
		if started {
			out = append(out, cur)
			cur = Token{}
			started = false
		}
	}

	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			endToken()
			i++
		case c == '\'':
			started = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, fmt.Errorf("unterminated single quote")
			}
			lit.WriteString(s[i+1 : i+1+j])
			i += j + 2
		case c == '"':
			started = true
			var err error
			i, err = scanDoubleQuoted(s, i+1, &cur, &lit)
			if err != nil {
				return nil, err
			}
		case c == '\\':
			started = true
			r, n, err := unescape(s, i)
			if err != nil {
				return nil, err
			}
			lit.WriteRune(r)
			i += n
		case c == '$':
			started = true
			name, split, n, ok := scanVariable(s, i)
			if !ok {
				if n == 1 {
					// A literal `$$` yields a single `$`.
					lit.WriteByte('$')
					i += 2
					continue
				}
				lit.WriteByte('$')
				i++
				continue
			}
			flushLit()
			cur.Pieces = append(cur.Pieces, Piece{Var: name, Split: split})
			i += n
		default:
			started = true
			lit.WriteByte(c)
			i++
		}
	}
	endToken()
	return out, nil
}

// scanDoubleQuoted consumes a double-quoted run starting at s[i] (just past the
// opening quote) and returns the index just past the closing quote.
func scanDoubleQuoted(s string, i int, cur *Token, lit *strings.Builder) (int, error) {
	for i < len(s) {
		switch s[i] {
		case '"':
			return i + 1, nil
		case '\\':
			r, n, err := unescape(s, i)
			if err != nil {
				return 0, err
			}
			lit.WriteRune(r)
			i += n
		case '$':
			name, _, n, ok := scanVariable(s, i)
			if !ok {
				if n == 1 {
					lit.WriteByte('$')
					i += 2
					continue
				}
				lit.WriteByte('$')
				i++
				continue
			}
			if lit.Len() > 0 {
				cur.Pieces = append(cur.Pieces, Piece{Literal: lit.String()})
				lit.Reset()
			}
			// Inside double quotes a variable never word-splits.
			cur.Pieces = append(cur.Pieces, Piece{Var: name})
			i += n
		default:
			lit.WriteByte(s[i])
			i++
		}
	}
	return 0, fmt.Errorf("unterminated double quote")
}

// scanVariable recognises `$NAME`, `${NAME}` and `$$` at s[i]. It returns the
// variable name, whether the reference word-splits after expansion, and the
// number of bytes consumed. ok is false for `$$` (with n==1) and for a bare
// `$` that starts no valid reference (n==0).
func scanVariable(s string, i int) (name string, split bool, n int, ok bool) {
	if i+1 >= len(s) {
		return "", false, 0, false
	}
	if s[i+1] == '$' {
		return "", false, 1, false
	}
	if s[i+1] == '{' {
		end := strings.IndexByte(s[i+2:], '}')
		if end < 0 {
			return "", false, 0, false
		}
		name = s[i+2 : i+2+end]
		if !validVarName(name) {
			return "", false, 0, false
		}
		return name, false, end + 3, true
	}
	j := i + 1
	for j < len(s) && (isAlpha(s[j]) || s[j] == '_' || (s[j] >= '0' && s[j] <= '9')) {
		j++
	}
	if j == i+1 {
		return "", false, 0, false
	}
	name = s[i+1 : j]
	if !validVarName(name) {
		return "", false, 0, false
	}
	// An unquoted $VAR word-splits after expansion; ${VAR} does not.
	return name, true, j - i, true
}

func validVarName(s string) bool {
	if s == "" {
		return false
	}
	if s[0] >= '0' && s[0] <= '9' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isAlpha(c) && c != '_' && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// unescape decodes the C-style escape at s[i] (which must be a backslash) and
// returns the rune and the number of bytes consumed.
func unescape(s string, i int) (rune, int, error) {
	if i+1 >= len(s) {
		return '\\', 1, nil
	}
	switch c := s[i+1]; c {
	case 'a':
		return '\a', 2, nil
	case 'b':
		return '\b', 2, nil
	case 'f':
		return '\f', 2, nil
	case 'n':
		return '\n', 2, nil
	case 'r':
		return '\r', 2, nil
	case 't':
		return '\t', 2, nil
	case 'v':
		return '\v', 2, nil
	case 's':
		return ' ', 2, nil
	case '\\', '"', '\'', '$':
		return rune(c), 2, nil
	case 'x':
		if i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				return rune(v), 4, nil
			}
		}
		return 0, 0, fmt.Errorf(`invalid \x escape`)
	case 'u':
		if i+5 < len(s) {
			if v, err := strconv.ParseUint(s[i+2:i+6], 16, 32); err == nil && utf8.ValidRune(rune(v)) {
				return rune(v), 6, nil
			}
		}
		return 0, 0, fmt.Errorf(`invalid \u escape`)
	case 'U':
		if i+9 < len(s) {
			if v, err := strconv.ParseUint(s[i+2:i+10], 16, 32); err == nil && utf8.ValidRune(rune(v)) {
				return rune(v), 10, nil
			}
		}
		return 0, 0, fmt.Errorf(`invalid \U escape`)
	default:
		if c >= '0' && c <= '7' {
			end := i + 2
			for end < len(s) && end < i+5 && s[end] >= '0' && s[end] <= '7' {
				end++
			}
			if v, err := strconv.ParseUint(s[i+1:end], 8, 32); err == nil {
				return rune(v), end - i, nil
			}
		}
		// An unrecognised escape keeps the backslash, matching systemd's
		// tolerance rather than failing a whole unit file over it.
		return '\\', 1, nil
	}
}

// Expand resolves a command's tokens against env, applying systemd's word
// splitting rule, and returns the argv.
func (c Command) Expand(env map[string]string) []string {
	var argv []string
	for _, t := range c.Tokens {
		if c.NoExpand {
			argv = append(argv, literalOf(t))
			continue
		}
		argv = append(argv, expandToken(t, env)...)
	}
	return argv
}

func expandToken(t Token, env map[string]string) []string {
	// Fast path: no variables at all.
	simple := true
	for _, p := range t.Pieces {
		if p.Var != "" {
			simple = false
			break
		}
	}
	if simple {
		var b strings.Builder
		for _, p := range t.Pieces {
			b.WriteString(p.Literal)
		}
		return []string{b.String()}
	}

	words := []string{""}
	appendText := func(s string) {
		words[len(words)-1] += s
	}
	for _, p := range t.Pieces {
		if p.Var == "" {
			appendText(p.Literal)
			continue
		}
		val := env[p.Var]
		if !p.Split {
			appendText(val)
			continue
		}
		fields := strings.Fields(val)
		if len(fields) == 0 {
			// An unset or all-whitespace $VAR contributes nothing and does not
			// create an empty argument on its own.
			continue
		}
		appendText(fields[0])
		for _, f := range fields[1:] {
			words = append(words, f)
		}
	}
	// Drop a trailing empty word produced solely by an empty split expansion.
	if len(words) > 1 && words[len(words)-1] == "" {
		words = words[:len(words)-1]
	}
	if len(words) == 1 && words[0] == "" {
		hadOnlySplit := true
		for _, p := range t.Pieces {
			if p.Var == "" && p.Literal != "" {
				hadOnlySplit = false
			}
		}
		if hadOnlySplit {
			return nil
		}
	}
	return words
}

// ResolveExecutable turns the first argv element into an absolute path,
// searching the fixed PATH of 05 §4.4 for a bare name. lookup reports whether a
// candidate path names an executable file; it is a parameter so tests need no
// filesystem.
func ResolveExecutable(name string, lookup func(string) bool) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty executable name")
	}
	if strings.ContainsRune(name, '/') {
		if !lookup(name) {
			return "", fmt.Errorf("%s: not found or not executable", name)
		}
		return name, nil
	}
	for _, dir := range searchPath {
		cand := dir + "/" + name
		if lookup(cand) {
			return cand, nil
		}
	}
	return "", fmt.Errorf("%s: not found in %s", name, strings.Join(searchPath, ":"))
}
