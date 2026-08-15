package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordRoundTrip(t *testing.T) {
	ts := time.Date(2026, 8, 11, 9, 14, 2, 117384000, time.UTC)
	r := Record{
		Time: ts, Priority: 6, Unit: "nginx.service", PID: 431,
		Message: "starting worker processes",
	}
	line := r.Encode()
	want := "2026-08-11T09:14:02.117384Z <6> nginx.service[431]: starting worker processes\n"
	if line != want {
		t.Errorf("Encode() = %q; want %q", line, want)
	}
	got := Decode(strings.TrimRight(line, "\n"), "nginx.service")
	if got.Legacy {
		t.Error("a well-formed record must not be flagged legacy")
	}
	if !got.Time.Equal(ts) || got.Priority != 6 || got.PID != 431 ||
		got.Message != r.Message || got.Identifier != "nginx.service" {
		t.Errorf("Decode round trip lost data: %+v", got)
	}
}

// Fixed-width timestamps make lexicographic order chronological order, which
// is what lets --since be answered by binary search.
func TestTimestampsAreFixedWidthAndSortable(t *testing.T) {
	a := Record{Time: time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC), Priority: 6}.Encode()
	b := Record{Time: time.Date(2026, 12, 31, 23, 59, 59, 999999000, time.UTC), Priority: 6}.Encode()
	if len(strings.SplitN(a, " ", 2)[0]) != len(strings.SplitN(b, " ", 2)[0]) {
		t.Error("timestamps must be fixed width")
	}
	if !(a < b) {
		t.Error("lexicographic order must match chronological order")
	}
}

// Compatibility: a container upgraded in place must still show its old,
// un-timestamped logs (defect B7's migration path).
func TestLegacyLineTolerated(t *testing.T) {
	got := Decode("just some old raw output", "old.service")
	if !got.Legacy {
		t.Error("an un-timestamped line must be flagged legacy")
	}
	if got.Priority != 6 {
		t.Errorf("Priority = %d; want 6", got.Priority)
	}
	if got.Message != "just some old raw output" {
		t.Errorf("Message = %q", got.Message)
	}
}

func TestSplitPriorityPrefix(t *testing.T) {
	p, msg := SplitPriorityPrefix("<3>something failed", 6)
	if p != 3 || msg != "something failed" {
		t.Errorf("got (%d, %q)", p, msg)
	}
	p, msg = SplitPriorityPrefix("no prefix", 6)
	if p != 6 || msg != "no prefix" {
		t.Errorf("got (%d, %q)", p, msg)
	}
	// A digit above 7 is not a priority prefix.
	p, msg = SplitPriorityPrefix("<9>x", 6)
	if p != 6 || msg != "<9>x" {
		t.Errorf("got (%d, %q)", p, msg)
	}
}

func TestInvalidUTF8Escaped(t *testing.T) {
	r := Record{Time: time.Now(), Priority: 6, Unit: "x.service", Message: "a\xffb"}
	line := r.Encode()
	if strings.Contains(line, "\xff") {
		t.Error("invalid UTF-8 must be escaped")
	}
	if !strings.Contains(line, `\xff`) {
		t.Errorf("expected a \\xNN escape, got %q", line)
	}
	if strings.Count(line, "\n") != 1 {
		t.Error("a record must occupy exactly one line")
	}
}

func TestBrokerWritesAndRotates(t *testing.T) {
	dir := t.TempDir()
	b := NewBroker(dir, RotateConfig{SizeMax: 200, FileCount: 2, TotalMax: 1 << 20})
	defer b.Close()

	for i := 0; i < 50; i++ {
		b.Write(Record{
			Time: time.Now(), Priority: 6, Unit: "spam.service",
			Message: "a reasonably long message to force rotation quickly",
		})
	}

	if _, err := os.Stat(filepath.Join(dir, "spam.service.log")); err != nil {
		t.Fatalf("the active log is missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "spam.service.log.1")); err != nil {
		t.Errorf("rotation did not happen: %v", err)
	}
	// FileCount=2 means .3 must never appear.
	if _, err := os.Stat(filepath.Join(dir, "spam.service.log.3")); err == nil {
		t.Error("more generations were kept than FileCount allows")
	}
}

func TestBrokerEnforcesTotalMax(t *testing.T) {
	dir := t.TempDir()
	b := NewBroker(dir, RotateConfig{SizeMax: 100, FileCount: 10, TotalMax: 2000})
	defer b.Close()
	for i := 0; i < 300; i++ {
		b.Write(Record{Time: time.Now(), Priority: 6, Unit: "big.service",
			Message: "0123456789012345678901234567890123456789"})
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, e := range entries {
		if fi, err := e.Info(); err == nil {
			total += fi.Size()
		}
	}
	// The active file may exceed the cap between rotations; allow one
	// generation's worth of slack.
	if total > 2000+200 {
		t.Errorf("log directory is %d bytes; LogTotalMax was 2000", total)
	}
}

func TestBrokerLogFileMode(t *testing.T) {
	dir := t.TempDir()
	b := NewBroker(dir, DefaultRotateConfig())
	defer b.Close()
	b.Write(Record{Time: time.Now(), Priority: 6, Unit: "m.service", Message: "x"})

	fi, err := os.Stat(filepath.Join(dir, "m.service.log"))
	if err != nil {
		t.Fatal(err)
	}
	// Services frequently echo credentials on startup; log files must not be
	// world-readable (defect E5).
	if fi.Mode().Perm()&0o007 != 0 {
		t.Errorf("log file mode is %v; it must not be world-readable", fi.Mode().Perm())
	}
}

func TestBrokerMirror(t *testing.T) {
	dir := t.TempDir()
	b := NewBroker(dir, DefaultRotateConfig())
	defer b.Close()
	var sb strings.Builder
	b.SetMirror(&sb, []string{"wanted.service"})

	b.Write(Record{Time: time.Now(), Priority: 6, Unit: "wanted.service", Message: "yes"})
	b.Write(Record{Time: time.Now(), Priority: 6, Unit: "other.service", Message: "no"})

	if !strings.Contains(sb.String(), "yes") {
		t.Error("the selected unit should be mirrored")
	}
	if strings.Contains(sb.String(), "no") {
		t.Error("an unselected unit must not be mirrored")
	}
}

// Defect B6: time filters were applied only when both --since and --until were
// given, so -S alone silently did nothing.
func TestReaderTimeFiltersWorkIndependently(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	var content strings.Builder
	for i := 0; i < 10; i++ {
		content.WriteString(Record{
			Time: base.Add(time.Duration(i) * time.Minute), Priority: 6,
			Unit: "t.service", Message: "line",
		}.Encode())
	}
	if err := os.WriteFile(filepath.Join(dir, "t.service.log"), []byte(content.String()), 0o640); err != nil {
		t.Fatal(err)
	}
	rd := NewReader(dir)

	q := NewQuery()
	q.Since = base.Add(5 * time.Minute)
	recs, err := rd.Read(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 5 {
		t.Errorf("--since alone returned %d records; want 5", len(recs))
	}

	q = NewQuery()
	q.Until = base.Add(2 * time.Minute)
	recs, _ = rd.Read(q)
	if len(recs) != 3 {
		t.Errorf("--until alone returned %d records; want 3", len(recs))
	}
}

// Defect B5: -n ran `tail -n N` with no filename, tailing init's stdin.
func TestReaderLinesTailsTheFile(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	var content strings.Builder
	for i := 0; i < 20; i++ {
		content.WriteString(Record{
			Time: base.Add(time.Duration(i) * time.Second), Priority: 6,
			Unit: "t.service", Message: "line",
		}.Encode())
	}
	if err := os.WriteFile(filepath.Join(dir, "t.service.log"), []byte(content.String()), 0o640); err != nil {
		t.Fatal(err)
	}
	q := NewQuery()
	q.Lines = 5
	recs, err := NewReader(dir).Read(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 5 {
		t.Fatalf("got %d records; want the last 5", len(recs))
	}
	if !recs[0].Time.Equal(base.Add(15 * time.Second)) {
		t.Errorf("the wrong window was returned: first record at %v", recs[0].Time)
	}
}

func TestReaderPriorityFilter(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	var content strings.Builder
	for p := 0; p <= 7; p++ {
		content.WriteString(Record{Time: now, Priority: p, Unit: "p.service", Message: "m"}.Encode())
	}
	if err := os.WriteFile(filepath.Join(dir, "p.service.log"), []byte(content.String()), 0o640); err != nil {
		t.Fatal(err)
	}
	q := NewQuery()
	q.Priority = 3
	recs, _ := NewReader(dir).Read(q)
	if len(recs) != 4 {
		t.Errorf("-p err returned %d records; want 4 (priorities 0..3)", len(recs))
	}
}

// journalctl reads <unit>.log.N .. <unit>.log in order.
func TestReaderConcatenatesRotatedFiles(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	write := func(name string, offset int) {
		var sb strings.Builder
		for i := 0; i < 3; i++ {
			sb.WriteString(Record{
				Time:     base.Add(time.Duration(offset+i) * time.Minute),
				Priority: 6, Unit: "r.service", Message: name,
			}.Encode())
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(sb.String()), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	write("r.service.log.2", 0)
	write("r.service.log.1", 10)
	write("r.service.log", 20)

	recs, err := NewReader(dir).Read(NewQuery())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 9 {
		t.Fatalf("got %d records; want 9 across three generations", len(recs))
	}
	for i := 1; i < len(recs); i++ {
		if recs[i].Time.Before(recs[i-1].Time) {
			t.Fatalf("records are out of order at index %d", i)
		}
	}
}

func TestReaderInterleavesUnits(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	writeOne := func(unit string, offsets ...int) {
		var sb strings.Builder
		for _, o := range offsets {
			sb.WriteString(Record{
				Time:     base.Add(time.Duration(o) * time.Second),
				Priority: 6, Unit: unit, Message: unit,
			}.Encode())
		}
		if err := os.WriteFile(filepath.Join(dir, unit+".log"), []byte(sb.String()), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	writeOne("a.service", 0, 2, 4)
	writeOne("b.service", 1, 3, 5)

	recs, _ := NewReader(dir).Read(NewQuery())
	if len(recs) != 6 {
		t.Fatalf("got %d records; want 6", len(recs))
	}
	// With -u omitted the default is all units, interleaved by timestamp.
	want := []string{"a.service", "b.service", "a.service", "b.service", "a.service", "b.service"}
	for i, r := range recs {
		if r.Unit != want[i] {
			t.Fatalf("record %d is from %s; want %s", i, r.Unit, want[i])
		}
	}
}

func TestVacuum(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"v.service.log", "v.service.log.1", "v.service.log.2"} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, 1000), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	rd := NewReader(dir)
	if rd.DiskUsage() != 3000 {
		t.Errorf("DiskUsage = %d; want 3000", rd.DiskUsage())
	}
	removed, _ := rd.VacuumSize(1500)
	if removed == 0 {
		t.Error("vacuum should have removed at least one archived file")
	}
	// The active log is never vacuumed.
	if _, err := os.Stat(filepath.Join(dir, "v.service.log")); err != nil {
		t.Error("the active log must not be removed by a vacuum")
	}
}

func TestParsePriority(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{{"err", 3}, {"3", 3}, {"warning", 4}, {"debug", 7}} {
		got, err := ParsePriority(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParsePriority(%q) = %d, %v", c.in, got, err)
		}
	}
	if _, err := ParsePriority("nope"); err == nil {
		t.Error("an unknown priority should be rejected")
	}
}

func FuzzDecodeRecord(f *testing.F) {
	f.Add("2026-08-11T09:14:02.117384Z <6> nginx.service[431]: hello")
	f.Add("legacy line")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		_ = Decode(s, "u.service")
	})
}
