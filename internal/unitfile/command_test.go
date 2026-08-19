package unitfile

import (
	"reflect"
	"testing"
)

func argvOf(t *testing.T, value string, env map[string]string) []string {
	t.Helper()
	cmd, err := ParseCommand(value)
	if err != nil {
		t.Fatalf("ParseCommand(%q): %v", value, err)
	}
	return cmd.Expand(env)
}

// Defect C5: v0.5.x stripped the outer quotes from the whole value in the line
// splitter, so a quoted path containing a space was mangled.
func TestTokenizeKeepsQuotedSpaces(t *testing.T) {
	got := argvOf(t, `"/opt/my app/bin" --flag`, nil)
	want := []string{"/opt/my app/bin", "--flag"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q; want %q", got, want)
	}
}

// Defect C8: only `-` was honoured; the others became part of the path.
func TestCommandPrefixes(t *testing.T) {
	cases := []struct {
		in     string
		check  func(Command) bool
		reason string
	}{
		{"-/bin/true", func(c Command) bool { return c.IgnoreFailure }, "- sets IgnoreFailure"},
		{"+/bin/true", func(c Command) bool { return c.Privileged }, "+ sets Privileged"},
		{"!/bin/true", func(c Command) bool { return c.Privileged }, "! sets Privileged"},
		{"!!/bin/true", func(c Command) bool { return c.Privileged }, "!! sets Privileged"},
		{":/bin/true", func(c Command) bool { return c.NoExpand }, ": sets NoExpand"},
		{"@/bin/sh mysh -c x", func(c Command) bool { return c.ArgvZero == "mysh" }, "@ sets argv[0]"},
		{"-@/bin/sh mysh", func(c Command) bool { return c.IgnoreFailure && c.ArgvZero == "mysh" },
			"prefixes combine in any order"},
	}
	for _, c := range cases {
		cmd, err := ParseCommand(c.in)
		if err != nil {
			t.Errorf("ParseCommand(%q): %v", c.in, err)
			continue
		}
		if !c.check(cmd) {
			t.Errorf("ParseCommand(%q): %s failed (%+v)", c.in, c.reason, cmd)
		}
		if len(cmd.Tokens) == 0 || literalOf(cmd.Tokens[0]) != "/bin/true" && literalOf(cmd.Tokens[0]) != "/bin/sh" {
			t.Errorf("ParseCommand(%q): the prefix leaked into the path: %q",
				c.in, literalOf(cmd.Tokens[0]))
		}
	}
}

// systemd's rule: an unquoted $VAR word-splits after expansion, ${VAR} does
// not, and neither splits inside double quotes.
func TestVariableExpansionSplitting(t *testing.T) {
	env := map[string]string{"OPTS": "-a -b", "EMPTY": "", "ONE": "x"}

	if got, want := argvOf(t, `/bin/foo $OPTS`, env), []string{"/bin/foo", "-a", "-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("unquoted $VAR: argv = %q; want %q", got, want)
	}
	if got, want := argvOf(t, `/bin/foo ${OPTS}`, env), []string{"/bin/foo", "-a -b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("${VAR}: argv = %q; want %q", got, want)
	}
	if got, want := argvOf(t, `/bin/foo "$OPTS"`, env), []string{"/bin/foo", "-a -b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("quoted $VAR: argv = %q; want %q", got, want)
	}
	if got, want := argvOf(t, `/bin/foo $MISSING`, env), []string{"/bin/foo"}; !reflect.DeepEqual(got, want) {
		t.Errorf("unset $VAR: argv = %q; want %q", got, want)
	}
	if got, want := argvOf(t, `/bin/foo a${ONE}b`, env), []string{"/bin/foo", "axb"}; !reflect.DeepEqual(got, want) {
		t.Errorf("embedded ${VAR}: argv = %q; want %q", got, want)
	}
	if got, want := argvOf(t, `/bin/foo $$HOME`, env), []string{"/bin/foo", "$HOME"}; !reflect.DeepEqual(got, want) {
		t.Errorf("$$: argv = %q; want %q", got, want)
	}
}

// The `:` prefix disables expansion entirely.
func TestNoExpandPrefix(t *testing.T) {
	env := map[string]string{"OPTS": "-a"}
	got := argvOf(t, `:/bin/foo $OPTS`, env)
	want := []string{"/bin/foo", "$OPTS"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q; want %q", got, want)
	}
}

// Defect C1: shell metacharacters are ordinary argument characters, because
// there is no shell.
func TestNoShellMetacharacters(t *testing.T) {
	got := argvOf(t, `/bin/echo a;b | c > d`, nil)
	want := []string{"/bin/echo", "a;b", "|", "c", ">", "d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q; want %q", got, want)
	}
}

func TestEscapes(t *testing.T) {
	got := argvOf(t, `/bin/echo a\tb "c\nd" 'e\nf' \x41 \101`, nil)
	want := []string{"/bin/echo", "a\tb", "c\nd", `e\nf`, "A", "A"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q; want %q", got, want)
	}
}

func TestSingleQuotesAreLiteral(t *testing.T) {
	env := map[string]string{"X": "expanded"}
	got := argvOf(t, `/bin/echo '$X'`, env)
	want := []string{"/bin/echo", "$X"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q; want %q", got, want)
	}
}

func TestUnterminatedQuoteIsAnError(t *testing.T) {
	for _, in := range []string{`/bin/echo "abc`, `/bin/echo 'abc`} {
		if _, err := ParseCommand(in); err == nil {
			t.Errorf("ParseCommand(%q) should have failed", in)
		}
	}
}

func TestResolveExecutable(t *testing.T) {
	present := map[string]bool{"/usr/bin/env": true, "/bin/sh": true}
	lookup := func(p string) bool { return present[p] }

	if got, err := ResolveExecutable("env", lookup); err != nil || got != "/usr/bin/env" {
		t.Errorf("ResolveExecutable(env) = %q, %v; want /usr/bin/env", got, err)
	}
	if got, err := ResolveExecutable("/bin/sh", lookup); err != nil || got != "/bin/sh" {
		t.Errorf("ResolveExecutable(/bin/sh) = %q, %v", got, err)
	}
	if _, err := ResolveExecutable("nope", lookup); err == nil {
		t.Error("an unresolvable bare name must be an error")
	}
	if _, err := ResolveExecutable("/no/such/path", lookup); err == nil {
		t.Error("an absolute path that does not exist must be an error")
	}
}

func FuzzTokenize(f *testing.F) {
	f.Add(`/bin/sh -c "echo $HOME"`)
	f.Add(`-@/bin/foo bar '\x41'`)
	f.Add(`${}`)
	f.Add(`\`)
	f.Fuzz(func(t *testing.T, s string) {
		// The lexer must never panic on arbitrary input; an error return is
		// the only acceptable failure.
		if toks, err := Tokenize(s); err == nil {
			for _, tok := range toks {
				_ = expandToken(tok, map[string]string{})
			}
		}
	})
}

func FuzzParseDuration(f *testing.F) {
	f.Add("5min 20s")
	f.Add("infinity")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = ParseDuration(s)
	})
}
