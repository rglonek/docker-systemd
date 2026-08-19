// Package graph builds and orders the job transactions that the manager
// executes. Everything about dependency semantics lives in one table rather
// than in eighteen copy-pasted loops, which is how v0.5.x came to wire
// Requisite= into Requires= (defect B8) and OnFailure= into OnSuccess= (B9).
package graph

import (
	"fmt"
	"sort"
	"strings"

	"docker-systemd/internal/unitfile"
)

// Kind describes one dependency directive.
type Kind struct {
	// Name is the directive as written in a unit file.
	Name string
	// Inverse is the directive the edge also implies on the other unit, or ""
	// when there is none. OnFailure=/OnSuccess= deliberately have none: the
	// inverse of OnFailure is not OnSuccess (defect B9).
	Inverse string
	// StartDep is true when starting this unit pulls the dependency into the
	// transaction as a start job.
	StartDep bool
	// FailOnDepFail is true when the dependency failing fails this unit.
	FailOnDepFail bool
	// StopWithDep is true when the dependency stopping stops this unit.
	StopWithDep bool
	// RequireActive is true when the dependency must already be active and is
	// never started by us. This is Requisite=, whose whole point v0.5.x
	// inverted.
	RequireActive bool
	// Conflicting is true when the edge means "stop the other unit".
	Conflicting bool
	// Ordering is true when the edge only constrains order, not membership.
	Ordering bool
}

// Kinds is the full dependency table.
var Kinds = []Kind{
	{Name: "Requires", Inverse: "RequiredBy", StartDep: true, FailOnDepFail: true, StopWithDep: false},
	{Name: "Requisite", Inverse: "RequisiteOf", StartDep: false, FailOnDepFail: true, RequireActive: true},
	{Name: "Wants", Inverse: "WantedBy", StartDep: true},
	{Name: "BindsTo", Inverse: "BoundBy", StartDep: true, FailOnDepFail: true, StopWithDep: true},
	{Name: "PartOf", Inverse: "ConsistsOf", StopWithDep: true},
	{Name: "Upholds", Inverse: "UpheldBy", StartDep: true},
	{Name: "Conflicts", Inverse: "ConflictedBy", Conflicting: true},
	{Name: "Before", Inverse: "After", Ordering: true},
	{Name: "After", Inverse: "Before", Ordering: true},
	{Name: "OnFailure", StartDep: false},
	{Name: "OnSuccess", StartDep: false},
}

// KindByName looks a directive up in the table.
func KindByName(name string) (Kind, bool) {
	for _, k := range Kinds {
		if k.Name == name {
			return k, true
		}
	}
	return Kind{}, false
}

// Deps returns the named dependency list of a unit.
func Deps(u *unitfile.Unit, kind string) []string {
	if u == nil {
		return nil
	}
	switch kind {
	case "Requires":
		return u.Unit.Requires
	case "Requisite":
		return u.Unit.Requisite
	case "Wants":
		return u.Unit.Wants
	case "BindsTo":
		return u.Unit.BindsTo
	case "PartOf":
		return u.Unit.PartOf
	case "Upholds":
		return u.Unit.Upholds
	case "Conflicts":
		return u.Unit.Conflicts
	case "Before":
		return u.Unit.Before
	case "After":
		return u.Unit.After
	case "OnFailure":
		return u.Unit.OnFailure
	case "OnSuccess":
		return u.Unit.OnSuccess
	}
	return nil
}

// JobType is what a transaction asks of a unit.
type JobType string

// Job types.
const (
	JobStart   JobType = "start"
	JobStop    JobType = "stop"
	JobRestart JobType = "restart"
	JobReload  JobType = "reload"
)

// Job is one unit's entry in a transaction.
type Job struct {
	Unit string
	Type JobType
	// Mandatory is true when a failure of this job fails the whole request.
	// Wants=-pulled jobs are not mandatory.
	Mandatory bool
	// RequireActive marks a Requisite= edge: never start, only check.
	RequireActive bool
	// Trigger names the unit that pulled this job in, for diagnostics.
	Trigger string
}

// Transaction is a set of jobs plus the ordering constraints between them.
type Transaction struct {
	Jobs []Job
	// Order holds `a must come before b` pairs restricted to jobs in the
	// transaction. Ordering edges to units outside the transaction are
	// dropped, which is exactly systemd's rule and what keeps boot from
	// serialising on absent units.
	Order [][2]string
	// Warnings records dropped edges and broken cycles.
	Warnings []string
}

// Resolver gives the builder access to the registry without importing the
// manager.
type Resolver interface {
	Get(name string) *unitfile.Unit
	InjectedWants(name string) []string
	InjectedRequires(name string) []string
	IsActive(name string) bool
}

// BuildStart expands a start request into a transaction (04 §7).
func BuildStart(r Resolver, roots []string) (*Transaction, error) {
	t := &Transaction{}
	seen := map[string]int{} // unit -> index into t.Jobs
	var queue []Job
	for _, root := range roots {
		queue = append(queue, Job{Unit: unitfile.CanonicalName(root), Type: JobStart, Mandatory: true})
	}

	for len(queue) > 0 {
		job := queue[0]
		queue = queue[1:]
		if idx, ok := seen[job.Unit]; ok {
			// Merge: a mandatory pull upgrades an optional one.
			if job.Mandatory {
				t.Jobs[idx].Mandatory = true
			}
			if !job.RequireActive {
				t.Jobs[idx].RequireActive = false
			}
			continue
		}
		u := r.Get(job.Unit)
		if u == nil {
			if job.Mandatory {
				return nil, fmt.Errorf("unit %s not found", job.Unit)
			}
			t.Warnings = append(t.Warnings,
				fmt.Sprintf("ignoring %s: not found (wanted by %s)", job.Unit, job.Trigger))
			continue
		}
		if u.LoadState == unitfile.LoadMasked {
			if job.Mandatory {
				return nil, fmt.Errorf("Unit %s is masked", job.Unit)
			}
			t.Warnings = append(t.Warnings, fmt.Sprintf("ignoring masked unit %s", job.Unit))
			continue
		}
		seen[job.Unit] = len(t.Jobs)
		t.Jobs = append(t.Jobs, job)

		if job.RequireActive {
			// Requisite=: the unit must already be active; we neither start it
			// nor pull in its own dependencies.
			continue
		}

		for _, k := range Kinds {
			if k.Ordering {
				continue
			}
			names := Deps(u, k.Name)
			if k.Name == "Wants" {
				names = append(append([]string{}, names...), r.InjectedWants(u.Name)...)
			}
			if k.Name == "Requires" {
				names = append(append([]string{}, names...), r.InjectedRequires(u.Name)...)
			}
			for _, dep := range names {
				dep = unitfile.CanonicalName(dep)
				switch {
				case k.Conflicting:
					if r.IsActive(dep) {
						queue = append(queue, Job{Unit: dep, Type: JobStop, Trigger: u.Name})
					}
				case k.RequireActive:
					queue = append(queue, Job{
						Unit: dep, Type: JobStart, Mandatory: true,
						RequireActive: true, Trigger: u.Name,
					})
				case k.StartDep:
					queue = append(queue, Job{
						Unit: dep, Type: JobStart,
						Mandatory: k.FailOnDepFail, Trigger: u.Name,
					})
				}
			}
		}
	}

	// A transaction holding both a start and a stop job for the same unit is
	// inconsistent. Drop the optional side if there is one, reject otherwise.
	if err := t.resolveConflicts(); err != nil {
		return nil, err
	}
	t.collectOrdering(r)
	return t, nil
}

// BuildStop expands a stop request. Dependents that BindsTo= or are PartOf=
// the stopping units come along.
func BuildStop(r Resolver, roots []string, active []string) *Transaction {
	t := &Transaction{}
	seen := map[string]bool{}
	var queue []string
	for _, root := range roots {
		queue = append(queue, unitfile.CanonicalName(root))
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		t.Jobs = append(t.Jobs, Job{Unit: name, Type: JobStop, Mandatory: true})
		// Anything that BindsTo= or is PartOf= this unit stops with it.
		for _, other := range active {
			if seen[other] {
				continue
			}
			ou := r.Get(other)
			if ou == nil {
				continue
			}
			for _, k := range Kinds {
				if !k.StopWithDep {
					continue
				}
				for _, dep := range Deps(ou, k.Name) {
					if unitfile.CanonicalName(dep) == name {
						queue = append(queue, other)
					}
				}
			}
		}
	}
	t.collectOrdering(r)
	return t
}

func (t *Transaction) resolveConflicts() error {
	byUnit := map[string][]int{}
	for i, j := range t.Jobs {
		byUnit[j.Unit] = append(byUnit[j.Unit], i)
	}
	drop := map[int]bool{}
	for unit, idxs := range byUnit {
		if len(idxs) < 2 {
			continue
		}
		var starts, stops []int
		for _, i := range idxs {
			if t.Jobs[i].Type == JobStop {
				stops = append(stops, i)
			} else {
				starts = append(starts, i)
			}
		}
		if len(starts) == 0 || len(stops) == 0 {
			continue
		}
		mandatoryStart := false
		for _, i := range starts {
			if t.Jobs[i].Mandatory {
				mandatoryStart = true
			}
		}
		if mandatoryStart {
			for _, i := range stops {
				drop[i] = true
			}
			t.Warnings = append(t.Warnings,
				fmt.Sprintf("transaction wanted both start and stop of %s; keeping the required start", unit))
			continue
		}
		return fmt.Errorf("transaction is destructive: %s is both started and stopped", unit)
	}
	if len(drop) == 0 {
		return nil
	}
	kept := t.Jobs[:0]
	for i, j := range t.Jobs {
		if !drop[i] {
			kept = append(kept, j)
		}
	}
	t.Jobs = kept
	return nil
}

// collectOrdering turns Before=/After= into edges restricted to the units
// present in the transaction.
func (t *Transaction) collectOrdering(r Resolver) {
	in := map[string]bool{}
	for _, j := range t.Jobs {
		in[j.Unit] = true
	}
	for _, j := range t.Jobs {
		u := r.Get(j.Unit)
		if u == nil {
			continue
		}
		for _, before := range u.Unit.Before {
			before = unitfile.CanonicalName(before)
			if in[before] {
				t.Order = append(t.Order, [2]string{j.Unit, before})
			}
		}
		for _, after := range u.Unit.After {
			after = unitfile.CanonicalName(after)
			if in[after] {
				t.Order = append(t.Order, [2]string{after, j.Unit})
			}
		}
	}
	sort.Slice(t.Order, func(i, k int) bool {
		if t.Order[i][0] != t.Order[k][0] {
			return t.Order[i][0] < t.Order[k][0]
		}
		return t.Order[i][1] < t.Order[k][1]
	})
	t.Order = dedupePairs(t.Order)
}

func dedupePairs(in [][2]string) [][2]string {
	out := in[:0]
	var last [2]string
	for i, p := range in {
		if i > 0 && p == last {
			continue
		}
		out = append(out, p)
		last = p
	}
	return out
}

// Strata orders the transaction topologically and returns the units grouped
// into strata; every unit in a stratum may run in parallel.
//
// A cycle is broken with a warning rather than a refusal to boot, matching
// systemd. v0.5.x had no ordering pass at all: boot order was ReadDir order
// (defect B11).
func (t *Transaction) Strata() [][]string {
	nodes := make([]string, 0, len(t.Jobs))
	for _, j := range t.Jobs {
		nodes = append(nodes, j.Unit)
	}
	sort.Strings(nodes)

	edges := map[string][]string{}
	indeg := map[string]int{}
	for _, n := range nodes {
		indeg[n] = 0
	}
	addEdge := func(from, to string) {
		for _, e := range edges[from] {
			if e == to {
				return
			}
		}
		edges[from] = append(edges[from], to)
		indeg[to]++
	}
	for _, p := range t.Order {
		if p[0] == p[1] {
			t.Warnings = append(t.Warnings,
				fmt.Sprintf("ignoring self-ordering edge on %s", p[0]))
			continue
		}
		addEdge(p[0], p[1])
	}

	var out [][]string
	remaining := len(nodes)
	placed := map[string]bool{}
	for remaining > 0 {
		var ready []string
		for _, n := range nodes {
			if !placed[n] && indeg[n] == 0 {
				ready = append(ready, n)
			}
		}
		if len(ready) == 0 {
			// A cycle: break the lowest-numbered remaining edge, name it, and
			// continue.
			broken := t.breakCycle(nodes, placed, edges, indeg)
			if broken == "" {
				// Defensive: nothing left to break, emit the rest as one
				// stratum rather than looping forever.
				for _, n := range nodes {
					if !placed[n] {
						ready = append(ready, n)
					}
				}
			} else {
				continue
			}
		}
		sort.Strings(ready)
		out = append(out, ready)
		for _, n := range ready {
			placed[n] = true
			remaining--
			for _, to := range edges[n] {
				indeg[to]--
			}
			edges[n] = nil
		}
	}
	return out
}

// breakCycle removes one edge from a remaining cycle and returns the unit whose
// edge was dropped.
func (t *Transaction) breakCycle(nodes []string, placed map[string]bool, edges map[string][]string, indeg map[string]int) string {
	for _, from := range nodes {
		if placed[from] || len(edges[from]) == 0 {
			continue
		}
		to := edges[from][0]
		edges[from] = edges[from][1:]
		indeg[to]--
		t.Warnings = append(t.Warnings,
			fmt.Sprintf("Found ordering cycle on %s/start; breaking at %s", from, to))
		return from
	}
	return ""
}

// Units returns the transaction's unit names in job order.
func (t *Transaction) Units() []string {
	out := make([]string, 0, len(t.Jobs))
	for _, j := range t.Jobs {
		out = append(out, j.Unit)
	}
	return out
}

// JobFor returns the job for a unit.
func (t *Transaction) JobFor(unit string) (Job, bool) {
	for _, j := range t.Jobs {
		if j.Unit == unit {
			return j, true
		}
	}
	return Job{}, false
}

// String renders a transaction for debug logging.
func (t *Transaction) String() string {
	var b strings.Builder
	for _, j := range t.Jobs {
		fmt.Fprintf(&b, "%s/%s ", j.Unit, j.Type)
	}
	return strings.TrimSpace(b.String())
}
