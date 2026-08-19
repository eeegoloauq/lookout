package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
)

// SampleFlushEvery is how often the seed file is flushed to the OS. A
// hard reset may lose this much; the durable state file is what must
// survive that, not the graph.
const SampleFlushEvery = 30 * time.Second

// MaxSampleLines is a hard cap on the seed file. Two days of MaxPoints
// for 128 checks is a generous homelab; past that the oldest lines go
// first so a 1s interval cannot fill the disk waiting for midnight.
const MaxSampleLines = MaxPoints * 128 * 2

// A probe result is a few fields; 4KiB would already be a check name
// that should never have been configured.
const maxSampleLine = 4096

// sampleLine is one JSONL object. Short keys keep 25×60s×24h around 2MiB
// instead of a database.
type sampleLine struct {
	T       int64         `json:"t"`
	Check   string        `json:"c"`
	Outcome check.Outcome `json:"o"`
	MS      int64         `json:"ms,omitempty"`
	Status  int           `json:"s,omitempty"`
}

// Samples is the JSONL seed for the in-memory rings. The rings stay the
// source of truth while we run; this file exists only so a restart can
// put the same points back.
type Samples struct {
	path     string
	maxLines int // 0 means MaxSampleLines; tests lower it

	mu    sync.Mutex
	f     *os.File
	w     *bufio.Writer
	lines int // on-disk plus buffered, for the cap
}

// NewSamples returns a seed log backed by path. It does not read the file yet.
func NewSamples(path string) *Samples {
	return &Samples{path: path}
}

// Path is the file this log appends to.
func (s *Samples) Path() string { return s.path }

func (s *Samples) cap() int {
	if s.maxLines > 0 {
		return s.maxLines
	}
	return MaxSampleLines
}

// Load reads the seed file, drops anything outside Retention, and
// rewrites the file when that (or a truncated tail) changed it. A
// missing file is empty history, not an error. Returned results are
// oldest first so the caller can feed the rings in order.
func (s *Samples) Load(now time.Time) ([]check.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept, err := s.compactLocked(now)
	out := make([]check.Result, len(kept))
	for i, rec := range kept {
		out[i] = rec.result()
	}
	return out, err
}

// Prune rewrites the file with only the lines still inside Retention.
func (s *Samples) Prune(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.compactLocked(now)
	return err
}

// Append buffers one probe result. It does not fsync: losing the last
// few seconds of a graph is cheaper than a fsync on every check.
func (s *Samples) Append(res check.Result) error {
	rec := lineFrom(res)
	if rec.T <= 0 || rec.Check == "" || !knownOutcome(rec.Outcome) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lines >= s.cap() {
		if _, err := s.compactLocked(time.Now()); err != nil {
			return err
		}
		if s.lines >= s.cap() {
			return nil
		}
	}
	if err := s.ensureWriterLocked(); err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := s.w.Write(b); err != nil {
		return err
	}
	if err := s.w.WriteByte('\n'); err != nil {
		return err
	}
	s.lines++
	return nil
}

// Flush writes the buffer to the OS. It does not fsync.
func (s *Samples) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w == nil {
		return nil
	}
	return s.w.Flush()
}

// Close flushes and closes the file handle.
func (s *Samples) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeWriterLocked()
}

func (s *Samples) compactLocked(now time.Time) ([]sampleLine, error) {
	if err := s.closeWriterLocked(); err != nil {
		return nil, err
	}
	valid, raw, err := readSampleLines(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.lines = 0
			return nil, nil
		}
		return nil, err
	}
	kept := compactSamples(valid, now, s.cap())
	s.lines = raw
	if len(kept) == raw && raw == len(valid) {
		s.lines = len(kept)
		return kept, nil
	}
	if err := s.rewriteLocked(kept); err != nil {
		return kept, err
	}
	s.lines = len(kept)
	return kept, nil
}

func (s *Samples) ensureWriterLocked() error {
	if s.w != nil {
		return nil
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	s.f = f
	s.w = bufio.NewWriterSize(f, 32*1024)
	return nil
}

func (s *Samples) closeWriterLocked() error {
	var ferr, cerr error
	if s.w != nil {
		ferr = s.w.Flush()
		s.w = nil
	}
	if s.f != nil {
		cerr = s.f.Close()
		s.f = nil
	}
	if ferr != nil {
		return ferr
	}
	return cerr
}

func (s *Samples) rewriteLocked(recs []sampleLine) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".samples-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	w := bufio.NewWriter(tmp)
	for _, rec := range recs {
		b, err := json.Marshal(rec)
		if err != nil {
			tmp.Close()
			return err
		}
		if _, err := w.Write(b); err != nil {
			tmp.Close()
			return err
		}
		if err := w.WriteByte('\n'); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	// The rewrite is the one write that replaces the whole seed; fsync
	// so a crash cannot leave us with an empty name and no old file.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

func readSampleLines(path string) (valid []sampleLine, raw int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				if err == io.EOF {
					break
				}
				if err != nil {
					return valid, raw, err
				}
				continue
			}
			raw++
			if rec, ok := parseSampleLine(line); ok {
				valid = append(valid, rec)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return valid, raw, err
		}
	}
	return valid, raw, nil
}

func parseSampleLine(line []byte) (sampleLine, bool) {
	if len(line) > maxSampleLine {
		return sampleLine{}, false
	}
	var rec sampleLine
	if json.Unmarshal(line, &rec) != nil {
		return sampleLine{}, false
	}
	if rec.T <= 0 || rec.Check == "" || !knownOutcome(rec.Outcome) {
		return sampleLine{}, false
	}
	return rec, true
}

func compactSamples(recs []sampleLine, now time.Time, capLines int) []sampleLine {
	kept := make([]sampleLine, 0, len(recs))
	for _, rec := range recs {
		at := time.Unix(rec.T, 0)
		if now.Sub(at) > Retention {
			continue
		}
		kept = append(kept, rec)
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].T < kept[j].T })
	kept = keepNewestPerCheck(kept, MaxPoints)
	return trimToCap(kept, capLines)
}

func keepNewestPerCheck(recs []sampleLine, perCheck int) []sampleLine {
	if perCheck <= 0 || len(recs) == 0 {
		return recs
	}
	n := make(map[string]int)
	keep := make([]sampleLine, 0, len(recs))
	for i := len(recs) - 1; i >= 0; i-- {
		rec := recs[i]
		if n[rec.Check] >= perCheck {
			continue
		}
		n[rec.Check]++
		keep = append(keep, rec)
	}
	for l, r := 0, len(keep)-1; l < r; l, r = l+1, r-1 {
		keep[l], keep[r] = keep[r], keep[l]
	}
	return keep
}

func trimToCap(recs []sampleLine, capLines int) []sampleLine {
	if capLines <= 0 || len(recs) < capLines {
		return recs
	}
	keep := capLines * 3 / 4
	if keep < 1 {
		keep = 1
	}
	if keep >= len(recs) {
		return recs
	}
	return recs[len(recs)-keep:]
}

func lineFrom(res check.Result) sampleLine {
	return sampleLine{
		T:       res.At.Unix(),
		Check:   res.Name,
		Outcome: res.Outcome,
		MS:      res.Duration.Milliseconds(),
		Status:  res.StatusCode,
	}
}

func (r sampleLine) result() check.Result {
	return check.Result{
		Name:       r.Check,
		At:         time.Unix(r.T, 0).UTC(),
		Outcome:    r.Outcome,
		Duration:   time.Duration(r.MS) * time.Millisecond,
		StatusCode: r.Status,
	}
}

func knownOutcome(o check.Outcome) bool {
	switch o {
	case check.OutcomeUp, check.OutcomeDown, check.OutcomeMalformed, check.OutcomeUnknown:
		return true
	}
	return false
}
