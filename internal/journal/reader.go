package journal

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Query is a journalctl filter.
type Query struct {
	// Units restricts to the named units; empty means all units, interleaved
	// by timestamp. `-u` being optional is a deliberate change: v0.5.x
	// required it.
	Units []string
	Since time.Time
	Until time.Time
	// Priority keeps records at or below this severity; -1 disables the
	// filter.
	Priority int
	// Lines tails the last N records; 0 means all.
	Lines   int
	Reverse bool
	Grep    string
}

// NewQuery returns a query with the filters disabled.
func NewQuery() Query { return Query{Priority: -1} }

// matches applies the filters that do not depend on ordering.
func (q Query) matches(r Record) bool {
	if q.Priority >= 0 && r.Priority > q.Priority {
		return false
	}
	if !q.Since.IsZero() && !r.Legacy && r.Time.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && !r.Legacy && r.Time.After(q.Until) {
		return false
	}
	if q.Grep != "" && !strings.Contains(r.Message, q.Grep) {
		return false
	}
	return true
}

// Reader reads log files from a directory.
type Reader struct{ Dir string }

// NewReader returns a reader over dir.
func NewReader(dir string) *Reader { return &Reader{Dir: dir} }

// Units lists the units that have a log file.
func (rd *Reader) Units() []string {
	entries, err := os.ReadDir(rd.Dir)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		i := strings.Index(name, ".log")
		if i < 0 {
			continue
		}
		unit := name[:i]
		if !seen[unit] {
			seen[unit] = true
			out = append(out, unit)
		}
	}
	sort.Strings(out)
	return out
}

// filesFor returns a unit's log files oldest-generation first, so that
// concatenating them yields chronological order.
func (rd *Reader) filesFor(unit string) []string {
	entries, err := os.ReadDir(rd.Dir)
	if err != nil {
		return nil
	}
	prefix := unit + ".log"
	var rotated []string
	active := ""
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if e.Name() == prefix {
			active = filepath.Join(rd.Dir, e.Name())
			continue
		}
		if isRotatedName(e.Name()) {
			rotated = append(rotated, filepath.Join(rd.Dir, e.Name()))
		}
	}
	// .log.3 is older than .log.1, so sort descending by generation.
	sort.Sort(sort.Reverse(sort.StringSlice(rotated)))
	if active != "" {
		rotated = append(rotated, active)
	}
	return rotated
}

// Read returns the records matching q.
func (rd *Reader) Read(q Query) ([]Record, error) {
	units := q.Units
	if len(units) == 0 {
		units = rd.Units()
	}
	var out []Record
	for _, unit := range units {
		for _, path := range rd.filesFor(unit) {
			recs, err := readFile(path, unit, q)
			if err != nil {
				continue
			}
			out = append(out, recs...)
		}
	}
	// Interleave by timestamp; legacy records without a usable time keep their
	// file order by sorting stably.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	if q.Lines > 0 && len(out) > q.Lines {
		out = out[len(out)-q.Lines:]
	}
	if q.Reverse {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

// readFile decodes one file, applying the record-level filters.
//
// A legacy un-timestamped record inherits the preceding record's time, so an
// upgraded container's old logs still sort into place.
func readFile(path, unit string, q Query) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var out []Record
	var last time.Time
	for sc.Scan() {
		rec := Decode(sc.Text(), unit)
		if rec.Legacy {
			rec.Time = last
		} else {
			last = rec.Time
		}
		if !q.matches(rec) {
			continue
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}

// DiskUsage returns the total size of the log directory.
func (rd *Reader) DiskUsage() int64 {
	entries, err := os.ReadDir(rd.Dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && !fi.IsDir() {
			total += fi.Size()
		}
	}
	return total
}

// VacuumSize removes the oldest rotated files until the directory is at or
// below max bytes.
func (rd *Reader) VacuumSize(max int64) (removed int, freed int64) {
	type entry struct {
		path string
		size int64
		mod  time.Time
	}
	entries, err := os.ReadDir(rd.Dir)
	if err != nil {
		return 0, 0
	}
	var total int64
	var rotated []entry
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || fi.IsDir() {
			continue
		}
		total += fi.Size()
		if isRotatedName(e.Name()) {
			rotated = append(rotated, entry{filepath.Join(rd.Dir, e.Name()), fi.Size(), fi.ModTime()})
		}
	}
	sort.Slice(rotated, func(i, j int) bool { return rotated[i].mod.Before(rotated[j].mod) })
	for _, r := range rotated {
		if total <= max {
			break
		}
		if os.Remove(r.path) == nil {
			total -= r.size
			freed += r.size
			removed++
		}
	}
	return removed, freed
}

// VacuumTime removes rotated files last modified before cutoff.
func (rd *Reader) VacuumTime(cutoff time.Time) (removed int, freed int64) {
	entries, err := os.ReadDir(rd.Dir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || fi.IsDir() || !isRotatedName(e.Name()) {
			continue
		}
		if fi.ModTime().Before(cutoff) {
			if os.Remove(filepath.Join(rd.Dir, e.Name())) == nil {
				removed++
				freed += fi.Size()
			}
		}
	}
	return removed, freed
}
