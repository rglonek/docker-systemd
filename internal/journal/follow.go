package journal

import (
	"bufio"
	"context"
	"io"
	"os"
	"time"
)

// Follow streams new records for the query's units until ctx is cancelled,
// calling emit for each.
//
// It is implemented natively: v0.5.x shelled out to `tail -n N` with no
// filename, so it tailed init's stdin and printed nothing (defect B5).
func (rd *Reader) Follow(ctx context.Context, q Query, emit func(Record)) error {
	units := q.Units
	if len(units) == 0 {
		units = rd.Units()
	}
	if len(units) == 0 {
		<-ctx.Done()
		return ctx.Err()
	}

	tails := make([]*tail, 0, len(units))
	for _, u := range units {
		files := rd.filesFor(u)
		if len(files) == 0 {
			// The unit has not logged yet; watch where its file will appear.
			tails = append(tails, &tail{path: rd.Dir + "/" + u + ".log", unit: u})
			continue
		}
		t := &tail{path: files[len(files)-1], unit: u}
		t.seekEnd()
		tails = append(tails, t)
	}

	watch := newWatcher(rd.Dir)
	defer watch.close()

	for {
		for _, t := range tails {
			t.drain(q, emit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-watch.wait(500 * time.Millisecond):
		}
	}
}

// tail follows one file, reopening it when rotation replaces the inode.
type tail struct {
	path    string
	unit    string
	f       *os.File
	offset  int64
	inode   uint64
	last    time.Time
	partial string
}

func (t *tail) seekEnd() {
	if err := t.open(); err != nil {
		return
	}
	if fi, err := t.f.Stat(); err == nil {
		t.offset, _ = t.f.Seek(fi.Size(), io.SeekStart)
	}
}

func (t *tail) open() error {
	f, err := os.Open(t.path)
	if err != nil {
		return err
	}
	if t.f != nil {
		t.f.Close()
	}
	t.f = f
	t.offset = 0
	t.inode = inodeOf(f)
	return nil
}

// drain reads whatever has been appended since the last call.
func (t *tail) drain(q Query, emit func(Record)) {
	if t.f == nil {
		if err := t.open(); err != nil {
			return
		}
	}
	// Rotation replaces the file; detect it by inode and by truncation.
	if fi, err := os.Stat(t.path); err == nil {
		if statInode(fi) != t.inode {
			_ = t.open()
		} else if fi.Size() < t.offset {
			t.offset, _ = t.f.Seek(0, io.SeekStart)
		}
	}
	if _, err := t.f.Seek(t.offset, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReader(t.f)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			// A partial line means the writer is mid-record; keep it for the
			// next pass rather than emitting a torn record.
			t.partial += line
			t.offset += int64(len(line))
			return
		}
		full := t.partial + line
		t.partial = ""
		t.offset += int64(len(line))
		rec := Decode(full, t.unit)
		if rec.Legacy {
			rec.Time = t.last
		} else {
			t.last = rec.Time
		}
		if q.matches(rec) {
			emit(rec)
		}
	}
}

func inodeOf(f *os.File) uint64 {
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	return statInode(fi)
}
