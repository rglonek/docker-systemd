package proctree

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fakeProc builds a /proc-shaped fixture so the stat parser and the ppid walk
// can be tested without real processes.
func fakeProc(t *testing.T, procs map[int]struct {
	comm  string
	ppid  int
	pgid  int
	start uint64
}) string {
	t.Helper()
	dir := t.TempDir()
	for pid, p := range procs {
		pdir := filepath.Join(dir, fmt.Sprint(pid))
		if err := os.MkdirAll(pdir, 0o755); err != nil {
			t.Fatal(err)
		}
		// pid (comm) state ppid pgrp session tty tpgid flags minflt cminflt
		// majflt cmajflt utime stime cutime cstime priority nice num_threads
		// itrealvalue starttime — starttime is field 22.
		stat := fmt.Sprintf("%d (%s) S %d %d 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 0",
			pid, p.comm, p.ppid, p.pgid, p.start)
		if err := os.WriteFile(filepath.Join(pdir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Non-numeric entries must be skipped.
	if err := os.MkdirAll(filepath.Join(dir, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

type procSpec = struct {
	comm  string
	ppid  int
	pgid  int
	start uint64
}

func withFakeProc(t *testing.T, dir string) {
	t.Helper()
	old := ProcDir()
	SetProcDir(dir)
	t.Cleanup(func() { SetProcDir(old) })
}

// The unit's membership is exactly the set of processes whose ppid chain
// reaches the supervisor (property P1).
func TestEnumerateWalksTheWholeChain(t *testing.T) {
	dir := fakeProc(t, map[int]procSpec{
		1:   {"init", 0, 1, 1},
		100: {"supervisor", 1, 100, 10},
		101: {"nginx", 100, 101, 20},
		102: {"worker", 101, 101, 30},
		103: {"grandchild", 102, 101, 40},
		200: {"unrelated", 1, 200, 50},
		201: {"unrelated-child", 200, 200, 60},
	})
	withFakeProc(t, dir)

	got, err := Enumerate(100)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	want := map[int]bool{101: true, 102: true, 103: true}
	if len(got) != len(want) {
		t.Fatalf("got %d members (%v); want %d", len(got), pids(got), len(want))
	}
	for _, r := range got {
		if !want[r.PID] {
			t.Errorf("pid %d should not be a member", r.PID)
		}
	}
}

// An escaped daemon reparented to the supervisor is still a member; a process
// that reparented to PID 1 is not.
func TestEnumerateExcludesProcessesReparentedToInit(t *testing.T) {
	dir := fakeProc(t, map[int]procSpec{
		1:   {"init", 0, 1, 1},
		100: {"supervisor", 1, 100, 10},
		101: {"escaped-daemon", 100, 101, 20}, // adopted by the supervisor
		300: {"orphan", 1, 300, 30},           // adopted by PID 1
	})
	withFakeProc(t, dir)

	got, _ := Enumerate(100)
	if len(got) != 1 || got[0].PID != 101 {
		t.Errorf("members = %v; want [101]", pids(got))
	}
}

// A corrupt /proc must not hang the walk.
func TestEnumerateCycleGuard(t *testing.T) {
	dir := fakeProc(t, map[int]procSpec{
		100: {"a", 101, 100, 10},
		101: {"b", 100, 100, 20},
	})
	withFakeProc(t, dir)

	done := make(chan struct{})
	go func() {
		_, _ = Enumerate(999)
		close(done)
	}()
	select {
	case <-done:
	case <-timeoutChan():
		t.Fatal("Enumerate did not terminate on a ppid cycle")
	}
}

// Field 2 is the command in parentheses and may contain spaces and ')', so the
// split must be after the LAST ')'.
func TestStatParsingWithAwkwardComm(t *testing.T) {
	dir := t.TempDir()
	pdir := filepath.Join(dir, "42")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := "42 (weird ) name) S 7 9 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 12345 0 0"
	if err := os.WriteFile(filepath.Join(pdir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	withFakeProc(t, dir)

	ref, ok := Lookup(42)
	if !ok {
		t.Fatal("Lookup failed")
	}
	if ref.PPID != 7 {
		t.Errorf("PPID = %d; want 7", ref.PPID)
	}
	if ref.PGID != 9 {
		t.Errorf("PGID = %d; want 9", ref.PGID)
	}
	if ref.StartTime != 12345 {
		t.Errorf("StartTime = %d; want 12345", ref.StartTime)
	}
	if ref.Comm != "weird ) name" {
		t.Errorf("Comm = %q", ref.Comm)
	}
}

func TestChildrenReturnsDirectChildrenOnly(t *testing.T) {
	dir := fakeProc(t, map[int]procSpec{
		100: {"sup", 1, 100, 10},
		101: {"child", 100, 101, 20},
		102: {"grandchild", 101, 101, 30},
	})
	withFakeProc(t, dir)

	got, err := Children(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PID != 101 {
		t.Errorf("Children = %v; want [101]", pids(got))
	}
}

func TestDistinctPGIDs(t *testing.T) {
	refs := []ProcRef{
		{PID: 1, PGID: 10}, {PID: 2, PGID: 10}, {PID: 3, PGID: 20}, {PID: 4, PGID: 1},
	}
	got := DistinctPGIDs(refs)
	if len(got) != 2 || got[0] != 10 || got[1] != 20 {
		t.Errorf("DistinctPGIDs = %v; want [10 20]", got)
	}
}

// The real /proc must be readable and parseable, which the backend probe
// depends on.
func TestRealProcIsParseable(t *testing.T) {
	refs, err := All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(refs) == 0 {
		t.Fatal("no processes found in /proc")
	}
	self := os.Getpid()
	found := false
	for _, r := range refs {
		if r.PID == self {
			found = true
			if r.PPID != os.Getppid() {
				t.Errorf("our PPID reads as %d; want %d", r.PPID, os.Getppid())
			}
		}
	}
	if !found {
		t.Error("our own pid was not found in /proc")
	}
}

func pids(refs []ProcRef) []int {
	out := make([]int, len(refs))
	for i, r := range refs {
		out[i] = r.PID
	}
	return out
}
