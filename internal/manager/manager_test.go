package manager

import (
	"testing"

	"docker-systemd/internal/graph"
	"docker-systemd/internal/proto"
)

// Defect B3: set-environment and unset-environment were swapped —
// cmdSetEnvironment called os.Unsetenv and cmdUnsetEnvironment called os.Setenv.
func TestSetAndUnsetEnvironment(t *testing.T) {
	m := testManager(t, nil)

	if err := m.SetEnvironment([]string{"FOO=bar", "BAZ=qux"}); err != nil {
		t.Fatalf("SetEnvironment: %v", err)
	}
	env := m.ShowEnvironment()
	if !contains(env, "FOO=bar") || !contains(env, "BAZ=qux") {
		t.Fatalf("set-environment did not set: %v", env)
	}

	m.UnsetEnvironment([]string{"FOO"})
	env = m.ShowEnvironment()
	if contains(env, "FOO=bar") {
		t.Errorf("unset-environment did not unset: %v", env)
	}
	if !contains(env, "BAZ=qux") {
		t.Errorf("unset-environment removed the wrong variable: %v", env)
	}
}

// A value containing '=' must survive: v0.5.x split on every '=' (defect C16).
func TestSetEnvironmentKeepsEqualsInValues(t *testing.T) {
	m := testManager(t, nil)
	if err := m.SetEnvironment([]string{"OPTS=a=b=c"}); err != nil {
		t.Fatal(err)
	}
	if !contains(m.ShowEnvironment(), "OPTS=a=b=c") {
		t.Errorf("environment = %v", m.ShowEnvironment())
	}
}

func TestSetEnvironmentRejectsNonAssignments(t *testing.T) {
	m := testManager(t, nil)
	if err := m.SetEnvironment([]string{"NOTANASSIGNMENT"}); err == nil {
		t.Error("a bare word should be rejected")
	}
}

// A unit that is present but has never run reports inactive/dead with its
// load state intact, rather than being represented by a null configuration
// (defect A4).
func TestUnitStateForALoadedButInactiveUnit(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/idle.service": "[Unit]\nDescription=Idle\n\n" +
			"[Service]\nExecStart=/bin/idle\n",
	})
	st := m.UnitState("idle")
	if st.Name != "idle.service" {
		t.Errorf("Name = %q; the CLI's bare name should be canonicalised", st.Name)
	}
	if st.LoadState != "loaded" {
		t.Errorf("LoadState = %q", st.LoadState)
	}
	if st.ActiveState != proto.StateInactive {
		t.Errorf("ActiveState = %q", st.ActiveState)
	}
	if st.Description != "Idle" {
		t.Errorf("Description = %q", st.Description)
	}
}

func TestUnitStateForAMissingUnit(t *testing.T) {
	m := testManager(t, nil)
	st := m.UnitState("nope.service")
	if st.LoadState != "not-found" {
		t.Errorf("LoadState = %q; want not-found", st.LoadState)
	}
}

// A unit that fails to parse stays in the registry with a populated LoadError,
// so `systemctl status` can explain itself (04 §5).
func TestBadUnitKeepsItsLoadError(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/bad.service": "[Service]\nType=simple\nExecStart=/bin/a\nExecStart=/bin/b\n",
	})
	st := m.UnitState("bad.service")
	if st.LoadState != "bad-setting" {
		t.Fatalf("LoadState = %q; want bad-setting", st.LoadState)
	}
	if st.LoadError == "" {
		t.Error("LoadError should explain why the unit did not load")
	}
}

func TestCatShowsFragmentAndDropIns(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/c.service":             "[Service]\nExecStart=/bin/c\n",
		"etc/systemd/system/c.service.d/over.conf": "[Service]\nRestart=always\n",
	})
	text, err := m.Cat("c.service")
	if err != nil {
		t.Fatalf("Cat: %v", err)
	}
	if !contains2(text, "ExecStart=/bin/c") || !contains2(text, "Restart=always") {
		t.Errorf("Cat output is missing content:\n%s", text)
	}
	if !contains2(text, "# ") {
		t.Error("Cat should prefix each file with a `# <path>` header")
	}
}

func TestCatMissingUnit(t *testing.T) {
	m := testManager(t, nil)
	if _, err := m.Cat("nope.service"); err == nil {
		t.Error("Cat of a missing unit should fail")
	}
}

// A dependency naming a unit that does not exist must not produce a job for a
// null unit (defect A6).
func TestTransactionSkipsAbsentOptionalDependencies(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/root.service": "[Unit]\nWants=absent.service\n\n" +
			"[Service]\nExecStart=/bin/root\n",
	})
	tr, err := graph.BuildStart(m, []string{"root.service"})
	if err != nil {
		t.Fatalf("BuildStart: %v", err)
	}
	if _, ok := tr.JobFor("absent.service"); ok {
		t.Error("an absent Wants= target must not become a job")
	}
	if len(tr.Warnings) == 0 {
		t.Error("an absent Wants= target should be warned about")
	}
}

// Synthetic targets make After=network.target resolve instead of dangling
// (defect C18).
func TestSyntheticTargetsResolveInTransactions(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/net.service": "[Unit]\nAfter=network-online.target\n" +
			"Wants=network-online.target\n\n[Service]\nExecStart=/bin/net\n",
	})
	tr, err := graph.BuildStart(m, []string{"net.service"})
	if err != nil {
		t.Fatalf("BuildStart: %v", err)
	}
	if _, ok := tr.JobFor("network-online.target"); !ok {
		t.Fatal("network-online.target should have been synthesised and pulled in")
	}
	strata := tr.Strata()
	if len(strata) != 2 {
		t.Fatalf("got %d strata; the After= edge should order them: %v", len(strata), strata)
	}
	if strata[0][0] != "network-online.target" {
		t.Errorf("stratum 0 = %v; the target must come first", strata[0])
	}
}

func TestDefaultTargetBootTransactionIncludesEnabledUnits(t *testing.T) {
	m := testManager(t, map[string]string{
		"lib/systemd/system/boot.service": "[Service]\nExecStart=/bin/boot\n\n" +
			"[Install]\nWantedBy=multi-user.target\n",
	})
	if _, err := m.enable([]string{"boot.service"}, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	m.loadRegistry()

	tr, err := graph.BuildStart(m, []string{"multi-user.target"})
	if err != nil {
		t.Fatalf("BuildStart: %v", err)
	}
	if _, ok := tr.JobFor("boot.service"); !ok {
		t.Error("an enabled unit should be pulled into the boot transaction")
	}
}

func contains2(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
