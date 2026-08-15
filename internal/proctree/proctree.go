// Package proctree enumerates a process subtree from /proc and signals its
// members.
//
// The design rests on the closure property of PR_SET_CHILD_SUBREAPER
// (03 §6.2): no descendant of a subreaper can leave its subtree, so the set of
// processes belonging to a unit is exactly the set whose ppid chain reaches the
// unit's supervisor, and a single /proc pass is exhaustive.
package proctree

import (
	"os"
	"sort"
	"strconv"
	"strings"
)

// MaxDepth bounds the ppid walk. /proc is not guaranteed to be internally
// consistent across a scan, so a cycle guard is mandatory rather than
// defensive.
const MaxDepth = 64

// ProcRef identifies a process together with enough state to detect PID reuse.
type ProcRef struct {
	PID       int
	PPID      int
	PGID      int
	StartTime uint64
	Comm      string
}

// procDir is the mount point scanned; a variable so tests can point it at a
// fixture tree.
var procDir = "/proc"

// SetProcDir overrides the /proc location. Intended for tests.
func SetProcDir(d string) { procDir = d }

// ProcDir returns the current /proc location.
func ProcDir() string { return procDir }

// snapshot is one pass over /proc.
type snapshot struct {
	byPID map[int]ProcRef
}

// scan reads every numeric entry in /proc once.
func scan() (*snapshot, error) {
	f, err := os.Open(procDir)
	if err != nil {
		return nil, err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	s := &snapshot{byPID: make(map[int]ProcRef, len(names))}
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		ref, ok := readStat(pid)
		if !ok {
			continue
		}
		s.byPID[pid] = ref
	}
	return s, nil
}

// readStat parses /proc/<pid>/stat.
//
// Field 2 is the command in parentheses and may itself contain spaces and
// ')', so the split must be after the LAST ')' rather than by naive
// whitespace tokenising.
func readStat(pid int) (ProcRef, bool) {
	data, err := os.ReadFile(procDir + "/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ProcRef{}, false
	}
	line := string(data)
	close := strings.LastIndexByte(line, ')')
	if close < 0 || close+2 >= len(line) {
		return ProcRef{}, false
	}
	comm := ""
	if open := strings.IndexByte(line, '('); open >= 0 && open < close {
		comm = line[open+1 : close]
	}
	fields := strings.Fields(line[close+2:])
	// After the comm, field indices shift: fields[0] is state (field 3),
	// fields[1] is ppid (4), fields[2] is pgrp (5), fields[19] is starttime
	// (22).
	if len(fields) < 20 {
		return ProcRef{}, false
	}
	ppid, err1 := strconv.Atoi(fields[1])
	pgid, err2 := strconv.Atoi(fields[2])
	start, err3 := strconv.ParseUint(fields[19], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return ProcRef{}, false
	}
	return ProcRef{PID: pid, PPID: ppid, PGID: pgid, StartTime: start, Comm: comm}, true
}

// Lookup returns the current ProcRef for a pid.
func Lookup(pid int) (ProcRef, bool) { return readStat(pid) }

// Enumerate returns every process whose ppid chain reaches root, excluding
// root itself. One pass over /proc, O(processes in the container).
//
// Races are one-sided: a process forked during the scan may be missed, which
// is why the termination ladder re-enumerates in a loop.
func Enumerate(root int) ([]ProcRef, error) {
	s, err := scan()
	if err != nil {
		return nil, err
	}
	return s.descendants(root), nil
}

func (s *snapshot) descendants(root int) []ProcRef {
	var out []ProcRef
	for pid, ref := range s.byPID {
		if pid == root {
			continue
		}
		p := pid
		for hops := 0; hops < MaxDepth; hops++ {
			cur, ok := s.byPID[p]
			if !ok {
				break
			}
			pp := cur.PPID
			if pp <= 1 {
				break
			}
			if pp == root {
				out = append(out, ref)
				break
			}
			p = pp
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// Children returns the direct children of a pid. This is the `degraded`
// backend's notion of membership.
func Children(root int) ([]ProcRef, error) {
	s, err := scan()
	if err != nil {
		return nil, err
	}
	var out []ProcRef
	for _, ref := range s.byPID {
		if ref.PPID == root {
			out = append(out, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

// All returns every visible process.
func All() ([]ProcRef, error) {
	s, err := scan()
	if err != nil {
		return nil, err
	}
	out := make([]ProcRef, 0, len(s.byPID))
	for _, r := range s.byPID {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

// DistinctPGIDs returns the distinct process-group ids present in refs.
func DistinctPGIDs(refs []ProcRef) []int {
	seen := map[int]bool{}
	var out []int
	for _, r := range refs {
		if r.PGID <= 1 || seen[r.PGID] {
			continue
		}
		seen[r.PGID] = true
		out = append(out, r.PGID)
	}
	sort.Ints(out)
	return out
}

// Cmdline reads a process's command line with NULs replaced by spaces, falling
// back to the bracketed comm for a kernel thread or a vanished process.
func Cmdline(pid int) string {
	data, err := os.ReadFile(procDir + "/" + strconv.Itoa(pid) + "/cmdline")
	if err == nil && len(data) > 0 {
		s := strings.TrimRight(string(data), "\x00")
		return strings.ReplaceAll(s, "\x00", " ")
	}
	if ref, ok := readStat(pid); ok {
		return "[" + ref.Comm + "]"
	}
	return ""
}

// Environ reads a process's environment block. It is used only by orphan
// recovery, to find processes carrying a known invocation id.
func Environ(pid int) map[string]string {
	data, err := os.ReadFile(procDir + "/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, entry := range strings.Split(string(data), "\x00") {
		if eq := strings.IndexByte(entry, '='); eq > 0 {
			out[entry[:eq]] = entry[eq+1:]
		}
	}
	return out
}

// FindByEnv returns the pids whose environment has key=value. This is the
// orphan-recovery mechanism of 03 §6.8: on supervisor loss, PID 1 finds the
// unit's surviving processes by their invocation-id marker.
func FindByEnv(key, value string) []int {
	refs, err := All()
	if err != nil {
		return nil
	}
	var out []int
	for _, r := range refs {
		if r.PID <= 1 {
			continue
		}
		env := Environ(r.PID)
		if env != nil && env[key] == value {
			out = append(out, r.PID)
		}
	}
	return out
}

// IsZombie reports whether a pid is in state Z. procwait's Signal(0) liveness
// check succeeded for zombies, so "has it exited?" polls could spin until
// timeout (defect F4).
func IsZombie(pid int) bool {
	data, err := os.ReadFile(procDir + "/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	line := string(data)
	close := strings.LastIndexByte(line, ')')
	if close < 0 || close+2 >= len(line) {
		return false
	}
	fields := strings.Fields(line[close+2:])
	return len(fields) > 0 && fields[0] == "Z"
}

// Alive reports whether a pid exists and is not a zombie.
func Alive(pid int) bool {
	ref, ok := readStat(pid)
	if !ok {
		return false
	}
	_ = ref
	return !IsZombie(pid)
}
