package unitfile

import "testing"

// Defect C7: v0.5.x implemented only %i and %I, expanded them only in Exec*
// values, and treated the two as identical.
func TestExpandSpecifiers(t *testing.T) {
	ctx := SpecifierContext{
		UnitName:  "getty@tty-a\\x2db.service",
		UserName:  "root",
		UID:       0,
		Group:     "root",
		GID:       0,
		Home:      "/root",
		Hostname:  "container",
		MachineID: "abc123",
		BootID:    "def456",
		Kernel:    "6.17.0",
		Arch:      "x86-64",
	}
	ctx.Prefix, ctx.Instance = SplitTemplate(ctx.UnitName)

	cases := []struct{ in, want string }{
		{"%n", "getty@tty-a\\x2db.service"},
		{"%N", "getty@tty-a\\x2db"},
		{"%p", "getty"},
		{"%P", "getty"},
		{"%i", "tty-a\\x2db"},
		{"%I", "tty/a-b"},
		{"%f", "/tty/a-b"},
		{"%t", "/run"},
		{"%S", "/var/lib"},
		{"%C", "/var/cache"},
		{"%L", "/var/log"},
		{"%E", "/etc"},
		{"%h", "/root"},
		{"%u", "root"},
		{"%U", "0"},
		{"%g", "root"},
		{"%G", "0"},
		{"%H", "container"},
		{"%m", "abc123"},
		{"%b", "def456"},
		{"%v", "6.17.0"},
		{"%a", "x86-64"},
		{"%%", "%"},
		{"a%pb%ic", "agettybtty-a\\x2dbc"},
		{"%z", "%z"},
	}
	for _, c := range cases {
		if got := ctx.ExpandSpecifiers(c.in); got != c.want {
			t.Errorf("ExpandSpecifiers(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

// %I must differ from %i for an escaped instance.
func TestUnescapedInstanceDiffers(t *testing.T) {
	ctx := DefaultSpecifierContext("foo@a-b.service")
	if ctx.ExpandSpecifiers("%i") == ctx.ExpandSpecifiers("%I") {
		t.Error("%i and %I must differ for an instance containing '-'")
	}
	if got := ctx.ExpandSpecifiers("%I"); got != "a/b" {
		t.Errorf("%%I = %q; want a/b", got)
	}
}

func TestEscapeUnescapeRoundTrip(t *testing.T) {
	for _, s := range []string{"/dev/sda1", "a b", "plain", "x/y-z"} {
		if got := Unescape(Escape(s)); got != s {
			t.Errorf("Unescape(Escape(%q)) = %q", s, got)
		}
	}
}

func TestSplitTemplate(t *testing.T) {
	cases := []struct{ in, prefix, instance string }{
		{"foo.service", "foo", ""},
		{"foo@bar.service", "foo", "bar"},
		{"foo@.service", "foo", ""},
		{"a-b@c.target", "a-b", "c"},
	}
	for _, c := range cases {
		p, i := SplitTemplate(c.in)
		if p != c.prefix || i != c.instance {
			t.Errorf("SplitTemplate(%q) = (%q, %q); want (%q, %q)", c.in, p, i, c.prefix, c.instance)
		}
	}
}

func TestCanonicalName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"nginx", "nginx.service"},
		{"nginx.service", "nginx.service"},
		{"multi-user.target", "multi-user.target"},
		{"foo.bar", "foo.bar.service"},
		{"dbus.socket", "dbus.socket"},
	}
	for _, c := range cases {
		if got := CanonicalName(c.in); got != c.want {
			t.Errorf("CanonicalName(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}
