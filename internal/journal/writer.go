package journal

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"docker-systemd/internal/paths"
)

// RotateConfig mirrors the directives of 07 §4.
type RotateConfig struct {
	SizeMax   int64 // rotate when the active file exceeds this
	FileCount int   // keep <unit>.log.1 .. .N
	TotalMax  int64 // across all units; oldest rotated files pruned first
}

// DefaultRotateConfig is the shipped default.
func DefaultRotateConfig() RotateConfig {
	return RotateConfig{SizeMax: 16 << 20, FileCount: 3, TotalMax: 128 << 20}
}

// Broker owns one open file handle per unit for the unit's whole lifetime.
//
// Exactly one writer per file is what makes rotation race-free, and opening
// once rather than once per start and once per stop is what fixes the fd leak
// proportional to restart count (defects B14/B15).
type Broker struct {
	mu        sync.Mutex
	cfg       RotateConfig
	dir       string
	sinks     map[string]*sink
	mirror    io.Writer
	mirrorAll bool
	mirrorSet map[string]bool
	disabled  bool
	// sinkErrAt rate-limits the LOG-SINK-ERROR message to one per unit per
	// minute so a full disk cannot itself become a log flood.
	sinkErrAt map[string]time.Time
}

type sink struct {
	f    *os.File
	path string
	size int64
}

// NewBroker returns a broker writing into dir.
func NewBroker(dir string, cfg RotateConfig) *Broker {
	return &Broker{
		cfg:       cfg,
		dir:       dir,
		sinks:     map[string]*sink{},
		mirrorSet: map[string]bool{},
		sinkErrAt: map[string]time.Time{},
	}
}

// SetMirror configures the --log-to-stderr mirror. units==nil mirrors
// everything; a non-empty list selects specific units, because mirroring a
// chatty service into `docker logs` is often exactly what one does not want.
func (b *Broker) SetMirror(w io.Writer, units []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mirror = w
	b.mirrorAll = len(units) == 0
	b.mirrorSet = map[string]bool{}
	for _, u := range units {
		b.mirrorSet[u] = true
	}
}

// Disable turns off file writing (--no-logfile).
func (b *Broker) Disable() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.disabled = true
}

// Write appends one record to its unit's sink and, when selected, mirrors it.
func (b *Broker) Write(r Record) {
	line := r.Encode()
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.mirror != nil && (b.mirrorAll || b.mirrorSet[r.Unit]) {
		_, _ = io.WriteString(b.mirror, line)
	}
	if b.disabled {
		return
	}
	s, err := b.sinkFor(r.Unit)
	if err != nil {
		b.reportSinkError(r.Unit, err)
		return
	}
	n, err := s.f.WriteString(line)
	if err != nil {
		b.reportSinkError(r.Unit, err)
		return
	}
	s.size += int64(n)
	if b.cfg.SizeMax > 0 && s.size >= b.cfg.SizeMax {
		b.rotate(r.Unit, s)
	}
}

// reportSinkError emits at most one message per unit per minute. A failing
// sink must never take the unit down with it.
func (b *Broker) reportSinkError(unit string, err error) {
	if last, ok := b.sinkErrAt[unit]; ok && time.Since(last) < time.Minute {
		return
	}
	b.sinkErrAt[unit] = time.Now()
	fmt.Fprintf(os.Stderr, "%s <3> systemd[%s]: LOG-SINK-ERROR: %v\n",
		time.Now().UTC().Format(TimeFormat), unit, err)
}

func (b *Broker) sinkFor(unit string) (*sink, error) {
	if s, ok := b.sinks[unit]; ok {
		return s, nil
	}
	if err := os.MkdirAll(b.dir, paths.ModeLogDir); err != nil {
		return nil, err
	}
	path := filepath.Join(b.dir, unit+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, paths.ModeLogFile)
	if err != nil {
		return nil, err
	}
	// Enforce the mode even if the file predates us with 0644: services
	// frequently echo credentials on startup and there is no reason for every
	// uid in the container to read them (defect E5).
	_ = f.Chmod(paths.ModeLogFile)
	size := int64(0)
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	s := &sink{f: f, path: path, size: size}
	b.sinks[unit] = s
	return s, nil
}

// rotate renames the generations and reopens the active file. Called with the
// lock held by the single writer, so no reader can observe a torn record.
func (b *Broker) rotate(unit string, s *sink) {
	if b.cfg.FileCount <= 0 {
		if err := s.f.Truncate(0); err == nil {
			_, _ = s.f.Seek(0, io.SeekStart)
			s.size = 0
		}
		return
	}
	_ = s.f.Close()
	_ = os.Remove(fmt.Sprintf("%s.%d", s.path, b.cfg.FileCount))
	for i := b.cfg.FileCount - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", s.path, i), fmt.Sprintf("%s.%d", s.path, i+1))
	}
	_ = os.Rename(s.path, s.path+".1")
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, paths.ModeLogFile)
	if err != nil {
		delete(b.sinks, unit)
		b.reportSinkError(unit, err)
		return
	}
	s.f = f
	s.size = 0
	b.enforceTotalMax()
}

// enforceTotalMax prunes the oldest rotated files until the directory fits
// within TotalMax. Active files are never removed.
func (b *Broker) enforceTotalMax() {
	if b.cfg.TotalMax <= 0 {
		return
	}
	type entry struct {
		path string
		size int64
		mod  time.Time
	}
	var rotated []entry
	var total int64
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || fi.IsDir() {
			continue
		}
		total += fi.Size()
		if isRotatedName(e.Name()) {
			rotated = append(rotated, entry{filepath.Join(b.dir, e.Name()), fi.Size(), fi.ModTime()})
		}
	}
	if total <= b.cfg.TotalMax {
		return
	}
	sort.Slice(rotated, func(i, j int) bool { return rotated[i].mod.Before(rotated[j].mod) })
	for _, r := range rotated {
		if total <= b.cfg.TotalMax {
			return
		}
		if os.Remove(r.path) == nil {
			total -= r.size
		}
	}
}

func isRotatedName(name string) bool {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return false
	}
	for _, c := range name[i+1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return i > 0 && strings.Contains(name[:i], ".log")
}

// CloseUnit releases a unit's sink. Called when a unit is removed from the
// registry, not on every stop.
func (b *Broker) CloseUnit(unit string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.sinks[unit]; ok {
		_ = s.f.Close()
		delete(b.sinks, unit)
	}
}

// Close releases every sink.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for unit, s := range b.sinks {
		_ = s.f.Close()
		delete(b.sinks, unit)
	}
}
