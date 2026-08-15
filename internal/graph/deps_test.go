package graph

import (
	"reflect"
	"strings"
	"testing"

	"docker-systemd/internal/unitfile"
)

// fakeResolver is a registry stub.
type fakeResolver struct {
	units    map[string]*unitfile.Unit
	active   map[string]bool
	wants    map[string][]string
	requires map[string][]string
}

func newResolver() *fakeResolver {
	return &fakeResolver{
		units: map[string]*unitfile.Unit{}, active: map[string]bool{},
		wants: map[string][]string{}, requires: map[string][]string{},
	}
}

func (f *fakeResolver) add(name string, mutate func(*unitfile.Unit)) *unitfile.Unit {
	u := unitfile.NewUnit(name)
	u.LoadState = unitfile.LoadLoaded
	if mutate != nil {
		mutate(u)
	}
	f.units[name] = u
	return u
}

func (f *fakeResolver) Get(name string) *unitfile.Unit        { return f.units[unitfile.CanonicalName(name)] }
func (f *fakeResolver) InjectedWants(name string) []string    { return f.wants[name] }
func (f *fakeResolver) InjectedRequires(name string) []string { return f.requires[name] }
func (f *fakeResolver) IsActive(name string) bool             { return f.active[name] }

// Defect B8: Requisite= was wired into Requires=, inverting its meaning from
// "fail if not already running" to "start it".
func TestRequisiteDoesNotCreateAStartJob(t *testing.T) {
	r := newResolver()
	r.add("a.service", func(u *unitfile.Unit) { u.Unit.Requisite = []string{"b.service"} })
	r.add("b.service", nil)

	tr, err := BuildStart(r, []string{"a.service"})
	if err != nil {
		t.Fatalf("BuildStart: %v", err)
	}
	job, ok := tr.JobFor("b.service")
	if !ok {
		t.Fatal("b.service should be in the transaction as a check")
	}
	if !job.RequireActive {
		t.Error("a Requisite= dependency must be marked RequireActive, not started")
	}
}

// A Requisite= dependency's own dependencies are not pulled in.
func TestRequisiteDoesNotExpandTransitively(t *testing.T) {
	r := newResolver()
	r.add("a.service", func(u *unitfile.Unit) { u.Unit.Requisite = []string{"b.service"} })
	r.add("b.service", func(u *unitfile.Unit) { u.Unit.Requires = []string{"c.service"} })
	r.add("c.service", nil)

	tr, _ := BuildStart(r, []string{"a.service"})
	if _, ok := tr.JobFor("c.service"); ok {
		t.Error("Requisite= must not expand its dependency's own requirements")
	}
}

// Defect B9: OnFailure= was cross-wired to OnSuccess= as its inverse.
func TestOnFailureHasNoInverseEdge(t *testing.T) {
	for _, k := range Kinds {
		if k.Name == "OnFailure" || k.Name == "OnSuccess" {
			if k.Inverse != "" {
				t.Errorf("%s must have no inverse edge, got %q", k.Name, k.Inverse)
			}
			if k.StartDep {
				t.Errorf("%s must not pull its target into a start transaction", k.Name)
			}
		}
	}
}

func TestRequiresPullsDependency(t *testing.T) {
	r := newResolver()
	r.add("a.service", func(u *unitfile.Unit) { u.Unit.Requires = []string{"b.service"} })
	r.add("b.service", nil)

	tr, err := BuildStart(r, []string{"a.service"})
	if err != nil {
		t.Fatalf("BuildStart: %v", err)
	}
	job, ok := tr.JobFor("b.service")
	if !ok {
		t.Fatal("b.service should be pulled in")
	}
	if !job.Mandatory {
		t.Error("a Requires= dependency is mandatory")
	}
}

func TestWantsFailureIsNotFatal(t *testing.T) {
	r := newResolver()
	r.add("a.service", func(u *unitfile.Unit) { u.Unit.Wants = []string{"b.service"} })
	r.add("b.service", nil)

	tr, _ := BuildStart(r, []string{"a.service"})
	job, _ := tr.JobFor("b.service")
	if job.Mandatory {
		t.Error("a Wants= dependency must not be mandatory")
	}
}

// A missing Wants= target is a warning; a missing Requires= target is an error.
func TestMissingDependencies(t *testing.T) {
	r := newResolver()
	r.add("a.service", func(u *unitfile.Unit) { u.Unit.Wants = []string{"gone.service"} })
	tr, err := BuildStart(r, []string{"a.service"})
	if err != nil {
		t.Fatalf("a missing Wants= target must not fail the transaction: %v", err)
	}
	if len(tr.Warnings) == 0 {
		t.Error("a missing Wants= target should warn")
	}

	r2 := newResolver()
	r2.add("a.service", func(u *unitfile.Unit) { u.Unit.Requires = []string{"gone.service"} })
	if _, err := BuildStart(r2, []string{"a.service"}); err == nil {
		t.Error("a missing Requires= target must fail the transaction")
	}
}

func TestMaskedUnitCannotBeStarted(t *testing.T) {
	r := newResolver()
	r.add("m.service", func(u *unitfile.Unit) { u.LoadState = unitfile.LoadMasked })
	_, err := BuildStart(r, []string{"m.service"})
	if err == nil || !strings.Contains(err.Error(), "masked") {
		t.Errorf("starting a masked unit should fail with a masked message, got %v", err)
	}
}

// Defect B11: Before=/After= were parsed, stored, wired — and never consulted.
func TestOrderingProducesStrata(t *testing.T) {
	r := newResolver()
	r.add("first.service", func(u *unitfile.Unit) {
		u.Unit.Before = []string{"second.service"}
	})
	r.add("second.service", func(u *unitfile.Unit) {
		u.Unit.Requires = []string{"first.service"}
		u.Unit.After = []string{"first.service"}
	})

	tr, err := BuildStart(r, []string{"second.service"})
	if err != nil {
		t.Fatalf("BuildStart: %v", err)
	}
	strata := tr.Strata()
	if len(strata) != 2 {
		t.Fatalf("got %d strata; want 2: %v", len(strata), strata)
	}
	if !reflect.DeepEqual(strata[0], []string{"first.service"}) {
		t.Errorf("stratum 0 = %v; want [first.service]", strata[0])
	}
	if !reflect.DeepEqual(strata[1], []string{"second.service"}) {
		t.Errorf("stratum 1 = %v; want [second.service]", strata[1])
	}
}

// Independent units share a stratum so they start in parallel.
func TestIndependentUnitsShareAStratum(t *testing.T) {
	r := newResolver()
	r.add("root.target", func(u *unitfile.Unit) {
		u.Unit.Wants = []string{"a.service", "b.service", "c.service"}
	})
	r.add("a.service", nil)
	r.add("b.service", nil)
	r.add("c.service", nil)

	tr, _ := BuildStart(r, []string{"root.target"})
	strata := tr.Strata()
	if len(strata) != 1 {
		t.Fatalf("got %d strata; want 1 (nothing is ordered): %v", len(strata), strata)
	}
	if len(strata[0]) != 4 {
		t.Errorf("stratum 0 has %d units; want 4", len(strata[0]))
	}
}

// A cycle is broken with a warning rather than a hang (defect A7's cousin).
func TestOrderingCycleIsBrokenNotHung(t *testing.T) {
	r := newResolver()
	r.add("a.service", func(u *unitfile.Unit) {
		u.Unit.After = []string{"b.service"}
		u.Unit.Wants = []string{"b.service"}
	})
	r.add("b.service", func(u *unitfile.Unit) {
		u.Unit.After = []string{"a.service"}
	})

	tr, err := BuildStart(r, []string{"a.service"})
	if err != nil {
		t.Fatalf("BuildStart: %v", err)
	}
	strata := tr.Strata()
	total := 0
	for _, s := range strata {
		total += len(s)
	}
	if total != 2 {
		t.Errorf("every unit must still be scheduled after breaking a cycle, got %v", strata)
	}
	found := false
	for _, w := range tr.Warnings {
		if strings.Contains(w, "ordering cycle") {
			found = true
		}
	}
	if !found {
		t.Errorf("breaking a cycle must be reported; warnings = %v", tr.Warnings)
	}
}

// Ordering edges to units outside the transaction are ignored, which is what
// keeps boot from serialising on absent units.
func TestOrderingEdgesOutsideTransactionAreDropped(t *testing.T) {
	r := newResolver()
	r.add("a.service", func(u *unitfile.Unit) { u.Unit.After = []string{"absent.service"} })
	tr, _ := BuildStart(r, []string{"a.service"})
	if len(tr.Order) != 0 {
		t.Errorf("Order = %v; edges to units outside the transaction must be dropped", tr.Order)
	}
}

func TestSelfOrderingEdgeIgnored(t *testing.T) {
	r := newResolver()
	r.add("s.service", func(u *unitfile.Unit) { u.Unit.After = []string{"s.service"} })
	tr, _ := BuildStart(r, []string{"s.service"})
	strata := tr.Strata()
	if len(strata) != 1 || len(strata[0]) != 1 {
		t.Errorf("a self-ordering edge must not deadlock the scheduler, got %v", strata)
	}
}

func TestConflictsStopsActiveUnit(t *testing.T) {
	r := newResolver()
	r.add("a.service", func(u *unitfile.Unit) { u.Unit.Conflicts = []string{"b.service"} })
	r.add("b.service", nil)
	r.active["b.service"] = true

	tr, err := BuildStart(r, []string{"a.service"})
	if err != nil {
		t.Fatalf("BuildStart: %v", err)
	}
	job, ok := tr.JobFor("b.service")
	if !ok || job.Type != JobStop {
		t.Errorf("a Conflicts= edge should produce a stop job, got %+v ok=%v", job, ok)
	}
}

func TestBuildStopPullsBoundUnits(t *testing.T) {
	r := newResolver()
	r.add("db.service", nil)
	r.add("app.service", func(u *unitfile.Unit) { u.Unit.BindsTo = []string{"db.service"} })

	tr := BuildStop(r, []string{"db.service"}, []string{"db.service", "app.service"})
	if _, ok := tr.JobFor("app.service"); !ok {
		t.Error("a unit that BindsTo= a stopping unit must stop with it")
	}
}

func TestBuildStopPullsPartOfUnits(t *testing.T) {
	r := newResolver()
	r.add("group.target", nil)
	r.add("member.service", func(u *unitfile.Unit) { u.Unit.PartOf = []string{"group.target"} })

	tr := BuildStop(r, []string{"group.target"}, []string{"group.target", "member.service"})
	if _, ok := tr.JobFor("member.service"); !ok {
		t.Error("a unit that is PartOf= a stopping unit must stop with it")
	}
}

func TestDependencyTableIsComplete(t *testing.T) {
	// Every directive named in 04 §7 must be present exactly once.
	want := []string{"Requires", "Requisite", "Wants", "BindsTo", "PartOf",
		"Upholds", "Conflicts", "Before", "After", "OnFailure", "OnSuccess"}
	for _, name := range want {
		if _, ok := KindByName(name); !ok {
			t.Errorf("dependency kind %s is missing from the table", name)
		}
	}
	seen := map[string]bool{}
	for _, k := range Kinds {
		if seen[k.Name] {
			t.Errorf("dependency kind %s appears twice", k.Name)
		}
		seen[k.Name] = true
	}
}
