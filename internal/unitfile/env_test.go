package unitfile

import (
	"reflect"
	"strings"
	"testing"
)

// Defect C3: v0.5.x split the file on "\n" with no comment, blank-line, quote
// or `export ` handling, so comment lines became environment entries.
func TestParseEnvironmentFile(t *testing.T) {
	content := `
# a comment
   ; not a comment for env files, but not an assignment either

export FOO=bar
BAZ="quoted value"
SINGLE='literal $NOTEXPANDED'
ESCAPED="line\tbreak"
EQUALS=a=b=c
CONT=one\
two
   INDENTED=yes
NOTANASSIGNMENT
9INVALID=x
`
	got, err := ParseEnvironmentFile(strings.NewReader(content))
	if err != nil {
		t.Fatalf("ParseEnvironmentFile: %v", err)
	}
	m := EnvMap(got)

	if m["FOO"] != "bar" {
		t.Errorf("FOO = %q; want bar (the `export ` prefix must be stripped)", m["FOO"])
	}
	if m["BAZ"] != "quoted value" {
		t.Errorf("BAZ = %q", m["BAZ"])
	}
	if m["SINGLE"] != "literal $NOTEXPANDED" {
		t.Errorf("SINGLE = %q", m["SINGLE"])
	}
	if m["ESCAPED"] != "line\tbreak" {
		t.Errorf("ESCAPED = %q; escapes are processed inside double quotes only", m["ESCAPED"])
	}
	// Defect C16: splitting on every "=" truncated any value containing one.
	if m["EQUALS"] != "a=b=c" {
		t.Errorf("EQUALS = %q; want a=b=c", m["EQUALS"])
	}
	if m["CONT"] != "onetwo" {
		t.Errorf("CONT = %q; want onetwo", m["CONT"])
	}
	if m["INDENTED"] != "yes" {
		t.Errorf("INDENTED = %q", m["INDENTED"])
	}
	if _, ok := m["# a comment"]; ok {
		t.Error("a comment line became an environment entry")
	}
	if _, ok := m["9INVALID"]; ok {
		t.Error("an invalid variable name was accepted")
	}
	if _, ok := m["NOTANASSIGNMENT"]; ok {
		t.Error("a line with no '=' became an entry")
	}
}

func TestParseEnvironmentAssignments(t *testing.T) {
	got, err := ParseEnvironmentAssignments(`"A=1 2" B=3 C="x y"`)
	if err != nil {
		t.Fatalf("ParseEnvironmentAssignments: %v", err)
	}
	want := []string{"A=1 2", "B=3", "C=x y"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q; want %q", got, want)
	}
	if _, err := ParseEnvironmentAssignments("NOTANASSIGNMENT"); err == nil {
		t.Error("a bare word should be rejected")
	}
}

func TestParseEnvFileRef(t *testing.T) {
	if f := ParseEnvFileRef("/etc/default/x"); f.Optional || f.Path != "/etc/default/x" {
		t.Errorf("got %+v", f)
	}
	if f := ParseEnvFileRef("-/etc/default/x"); !f.Optional || f.Path != "/etc/default/x" {
		t.Errorf("a leading '-' means ignore-if-missing; got %+v", f)
	}
}

func TestEnvMapSplitsOnFirstEqualsOnly(t *testing.T) {
	m := EnvMap([]string{"A=b=c=d"})
	if m["A"] != "b=c=d" {
		t.Errorf("A = %q; want b=c=d", m["A"])
	}
}
