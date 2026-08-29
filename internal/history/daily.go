package history

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/state"
)

// Daily is one JSON Lines record: one check, one UTC day.
type Daily struct {
	Date      string  `json:"date"`
	Check     string  `json:"check"`
	Group     string  `json:"group,omitempty"`
	Uptime    float64 `json:"uptime"`
	Incidents int     `json:"incidents"`
	Samples   int     `json:"samples"`
	P50MS     int64   `json:"p50_ms,omitempty"`
	P95MS     int64   `json:"p95_ms,omitempty"`
	// Reason is the last failure recorded that day. A month of squares
	// that can only say "something happened here" is worse than no month
	// at all: it raises the question and then refuses to answer it.
	Reason string `json:"reason,omitempty"`
}

// Log is the append-only JSONL file. Restarts must neither duplicate a
// day (the seen set is rebuilt from the file) nor lose one (the in-progress
// accumulator lives in durable state and is flushed on the next start if
// its date is in the past).
type Log struct {
	path string
	mu   sync.Mutex
	seen map[string]bool // date+"\t"+check
	all  []Daily
}

// NewLog returns a log backed by path. It does not read the file yet.
func NewLog(path string) *Log {
	return &Log{path: path, seen: map[string]bool{}}
}

// Path is the file this log appends to.
func (l *Log) Path() string { return l.path }

// Load reads existing records. A missing file is empty history, not an
// error. A truncated last line (crash mid-write) is skipped.
func (l *Log) Load() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = map[string]bool{}
	l.all = nil
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// A year of 60s checks is ~8k short lines; 1MiB is still plenty
	// if a check name is long.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Daily
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Date == "" || rec.Check == "" {
			continue
		}
		key := rec.Date + "\t" + rec.Check
		if l.seen[key] {
			continue
		}
		l.seen[key] = true
		l.all = append(l.all, rec)
	}
	return sc.Err()
}

// Append writes rec if that check+date is not already in the file.
func (l *Log) Append(rec Daily) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := rec.Date + "\t" + rec.Check
	if l.seen[key] {
		return nil
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(data, '\n'))
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if serr != nil {
		return serr
	}
	if cerr != nil {
		return cerr
	}
	l.seen[key] = true
	l.all = append(l.all, rec)
	return nil
}

// Has reports whether that check already has a line for date.
func (l *Log) Has(date, check string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen[date+"\t"+check]
}

// Records returns every stored day, oldest first.
func (l *Log) Records() []Daily {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Daily, len(l.all))
	copy(out, l.all)
	return out
}

// Uptime is the sample-weighted ratio over records with Date >= since
// (YYYY-MM-DD, UTC) for one check, plus an optional in-progress today.
func (l *Log) Uptime(name, since string, today *state.DayAcc) (ratio float64, samples int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	up := 0
	seenToday := false
	for _, rec := range l.all {
		if rec.Check != name || rec.Date < since {
			continue
		}
		if today != nil && rec.Date == today.Date {
			seenToday = true
		}
		samples += rec.Samples
		up += int(math.Round(rec.Uptime * float64(rec.Samples)))
	}
	if today != nil && today.Date >= since && !seenToday {
		samples += today.Samples
		up += today.Up
	}
	if samples == 0 {
		return 0, 0
	}
	return float64(up) / float64(samples), samples
}

// Window returns one record per UTC day for the last n days ending on
// today, oldest first. A day the file has nothing for comes back as a
// zero record with its date set: the caller must be able to tell a day
// that was quiet from a day lookout was not running, and a gap silently
// closed up would shift every square in the strip onto the wrong date.
// today is the in-progress accumulator, folded in as the last day.
func (l *Log) Window(name string, n int, now time.Time, today *state.DayAcc) []Daily {
	if n < 1 {
		return nil
	}
	l.mu.Lock()
	have := map[string]Daily{}
	for _, rec := range l.all {
		if rec.Check == name {
			have[rec.Date] = rec
		}
	}
	l.mu.Unlock()
	if today != nil && today.Date != "" {
		if _, ok := have[today.Date]; !ok {
			have[today.Date] = ToDaily(name, "", *today)
		}
	}
	end := now.UTC()
	out := make([]Daily, 0, n)
	for i := n - 1; i >= 0; i-- {
		date := end.AddDate(0, 0, -i).Format("2006-01-02")
		if rec, ok := have[date]; ok {
			out = append(out, rec)
			continue
		}
		out = append(out, Daily{Date: date, Check: name})
	}
	return out
}

// RecordDay folds one probe into the in-progress UTC day. dateRolled
// is the previous accumulator when the clock crossed midnight, so the
// caller can flush it.
func RecordDay(acc state.DayAcc, res check.Result, incidents int) (next, rolled state.DayAcc, rolledOK bool) {
	day := utcDate(res.At)
	if acc.Date != "" && acc.Date != day {
		rolled, rolledOK = acc, true
		acc = state.DayAcc{}
	}
	if acc.Date == "" {
		acc.Date = day
	}
	if res.Outcome != check.OutcomeUnknown {
		acc.Samples++
		if res.Outcome.Succeeded() {
			acc.Up++
		} else if r := res.Reason(); r != "" {
			acc.Reason = r
		}
		if res.Duration > 0 && len(acc.Durations) < state.MaxDayDurations {
			acc.Durations = append(acc.Durations, res.Duration.Milliseconds())
		}
	}
	acc.Incidents += incidents
	return acc, rolled, rolledOK
}

// ToDaily converts an accumulator into a JSONL record.
func ToDaily(name, group string, acc state.DayAcc) Daily {
	rec := Daily{
		Date:      acc.Date,
		Check:     name,
		Group:     group,
		Incidents: acc.Incidents,
		Samples:   acc.Samples,
		Reason:    acc.Reason,
	}
	if acc.Samples > 0 {
		rec.Uptime = float64(acc.Up) / float64(acc.Samples)
	}
	if n := len(acc.Durations); n > 0 {
		rec.P50MS = percentile(acc.Durations, 0.50)
		rec.P95MS = percentile(acc.Durations, 0.95)
	}
	return rec
}

func utcDate(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// NextUTCMidnight is the next 00:00:00 UTC strictly after now.
func NextUTCMidnight(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
}

func percentile(ms []int64, p float64) int64 {
	if len(ms) == 0 {
		return 0
	}
	cp := append([]int64(nil), ms...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(math.Ceil(p*float64(len(cp)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}
