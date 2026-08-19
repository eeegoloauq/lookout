package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/state"
)

func TestAppendIsIdempotentPerCheckPerDay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	l := NewLog(path)
	rec := Daily{Date: "2026-08-19", Check: "Photos", Group: "Services", Uptime: 1, Samples: 10, P50MS: 40, P95MS: 80}
	if err := l.Append(rec); err != nil {
		t.Fatal(err)
	}
	dup := rec
	dup.Samples = 99
	if err := l.Append(dup); err != nil {
		t.Fatal(err)
	}
	if n := len(l.Records()); n != 1 {
		t.Fatalf("records = %d, the duplicate day was written", n)
	}

	reloaded := NewLog(path)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Append(rec); err != nil {
		t.Fatal(err)
	}
	if n := len(reloaded.Records()); n != 1 {
		t.Fatalf("after reload: %d records", n)
	}
}

func TestLoadSkipsATruncatedLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	good, err := json.Marshal(Daily{Date: "2026-08-18", Check: "Photos", Samples: 1, Uptime: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append(good, '\n'), []byte(`{"date":"2026-08-19","che`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	l := NewLog(path)
	if err := l.Load(); err != nil {
		t.Fatal(err)
	}
	if n := len(l.Records()); n != 1 || l.Records()[0].Date != "2026-08-18" {
		t.Fatalf("records = %+v, truncated line must not become a day", l.Records())
	}
}

func TestRecordDayRollsAtUTCMidnight(t *testing.T) {
	acc := state.DayAcc{}
	start := time.Date(2026, 8, 19, 23, 0, 0, 0, time.UTC)
	acc, _, _ = RecordDay(acc, check.Result{At: start, Outcome: check.OutcomeUp, Duration: 40 * time.Millisecond}, 0)
	next := start.Add(2 * time.Hour) // 01:00 next UTC day
	acc, rolled, ok := RecordDay(acc, check.Result{At: next, Outcome: check.OutcomeDown, Duration: 80 * time.Millisecond}, 1)
	if !ok || rolled.Date != "2026-08-19" || rolled.Samples != 1 {
		t.Fatalf("rolled = %+v ok=%v", rolled, ok)
	}
	if acc.Date != "2026-08-20" || acc.Samples != 1 || acc.Incidents != 1 {
		t.Fatalf("today = %+v", acc)
	}
}

func TestToDailyPercentiles(t *testing.T) {
	acc := state.DayAcc{Date: "2026-08-19", Samples: 5, Up: 4, Incidents: 1, Durations: []int64{10, 20, 30, 40, 100}}
	rec := ToDaily("Photos", "Services", acc)
	if rec.Uptime != 0.8 || rec.Incidents != 1 || rec.P50MS != 30 || rec.P95MS != 100 {
		t.Fatalf("daily = %+v", rec)
	}
}

func TestUnknownOutcomeIsNotASample(t *testing.T) {
	acc, _, _ := RecordDay(state.DayAcc{}, check.Result{At: epoch, Outcome: check.OutcomeUnknown}, 0)
	if acc.Samples != 0 {
		t.Fatalf("unknown must not count: %+v", acc)
	}
}

func TestUptimeWindowWeightsBySamples(t *testing.T) {
	l := NewLog(filepath.Join(t.TempDir(), "h.jsonl"))
	_ = l.Append(Daily{Date: "2026-08-17", Check: "Photos", Uptime: 1, Samples: 10})
	_ = l.Append(Daily{Date: "2026-08-18", Check: "Photos", Uptime: 0.5, Samples: 10})
	_ = l.Append(Daily{Date: "2026-08-18", Check: "Router", Uptime: 0, Samples: 10})
	ratio, samples := l.Uptime("Photos", "2026-08-17", nil)
	if samples != 20 || ratio != 0.75 {
		t.Errorf("uptime = %v over %d, want 0.75 over 20", ratio, samples)
	}
}

func TestNextUTCMidnight(t *testing.T) {
	now := time.Date(2026, 8, 19, 23, 0, 0, 0, time.UTC)
	next := NextUTCMidnight(now)
	if !next.Equal(time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("next = %s", next)
	}
}

func TestAppendDoesNotUseTheRealClock(t *testing.T) {
	// Guard: this file must stay inside synctest when it sleeps. The
	// log itself is clock-free; the test exists so a future sleep
	// cannot sneak in outside a bubble.
	synctest.Test(t, func(t *testing.T) {
		l := NewLog(filepath.Join(t.TempDir(), "h.jsonl"))
		if err := l.Append(Daily{Date: "2000-01-01", Check: "A", Samples: 1, Uptime: 1}); err != nil {
			t.Fatal(err)
		}
	})
}
