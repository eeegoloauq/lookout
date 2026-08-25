package history

import (
	"sync"
	"testing"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
)

var epoch = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

func point(i int, outcome check.Outcome) Point {
	return Point{At: epoch.Add(time.Duration(i) * time.Minute), Outcome: outcome, StatusCode: 200}
}

func TestCapacityFor(t *testing.T) {
	tests := []struct {
		interval time.Duration
		want     int
	}{
		// Retention at the interval, plus a tenth of it as headroom for
		// probes that land off the beat (a restart runs its checks at once).
		{time.Minute, 1585},
		{5 * time.Minute, 317},
		{time.Hour, 27},
		{25 * time.Hour, 2},
		{time.Second, MaxPoints}, // capped: 86400 points per check is not a homelab
		{0, MaxPoints},
	}
	for _, tc := range tests {
		if got := CapacityFor(tc.interval); got != tc.want {
			t.Errorf("CapacityFor(%s) = %d, want %d", tc.interval, got, tc.want)
		}
	}
}

func TestRingKeepsTheMostRecentPointsInOrder(t *testing.T) {
	r := NewRing(3)
	for i := range 5 {
		r.Add(point(i, check.OutcomeUp))
	}
	got := r.Points()
	if len(got) != 3 {
		t.Fatalf("held %d points, want 3", len(got))
	}
	for i, p := range got {
		want := point(i+2, check.OutcomeUp).At
		if !p.At.Equal(want) {
			t.Errorf("point %d is at %s, want %s", i, p.At, want)
		}
	}
	last, ok := r.Last()
	if !ok || !last.At.Equal(point(4, check.OutcomeUp).At) {
		t.Errorf("Last() = %v, %v", last, ok)
	}
}

func TestRingDropsPointsOlderThanRetention(t *testing.T) {
	r := NewRing(MaxPoints)
	r.Add(Point{At: epoch, Outcome: check.OutcomeUp})
	r.Add(Point{At: epoch.Add(Retention - time.Minute), Outcome: check.OutcomeUp})
	if r.Len() != 2 {
		t.Fatalf("held %d points, want 2", r.Len())
	}
	r.Add(Point{At: epoch.Add(Retention + time.Minute), Outcome: check.OutcomeUp})
	if r.Len() != 2 {
		t.Fatalf("held %d points, want 2 after the first aged out", r.Len())
	}
	if first := r.Points()[0]; first.At.Equal(epoch) {
		t.Error("the point older than the retention window is still held")
	}
}

func TestUptime(t *testing.T) {
	r := NewRing(10)
	pattern := []check.Outcome{
		check.OutcomeUp, check.OutcomeDown, check.OutcomeUp, check.OutcomeUp,
		check.OutcomeMalformed, check.OutcomeUp,
	}
	for i, o := range pattern {
		r.Add(point(i, o))
	}

	// Malformed counts against uptime: it is a probe we could not call good.
	ratio, samples := r.Uptime(epoch)
	if samples != 6 || ratio != 4.0/6.0 {
		t.Errorf("Uptime = %v over %d samples, want %v over 6", ratio, samples, 4.0/6.0)
	}

	// A narrower window only counts what falls inside it.
	ratio, samples = r.Uptime(epoch.Add(4 * time.Minute))
	if samples != 2 || ratio != 0.5 {
		t.Errorf("Uptime = %v over %d samples, want 0.5 over 2", ratio, samples)
	}

	// No samples must never read as 100%.
	ratio, samples = r.Uptime(epoch.Add(time.Hour))
	if samples != 0 || ratio != 0 {
		t.Errorf("Uptime = %v over %d samples, want 0 over 0", ratio, samples)
	}
}

func TestHistoryRecordsOnlyTrackedChecks(t *testing.T) {
	h := New()
	h.Track("Example", time.Minute)
	h.Record(check.Result{Name: "Example", At: epoch, Outcome: check.OutcomeUp, StatusCode: 200})
	h.Record(check.Result{Name: "Unknown Check", At: epoch, Outcome: check.OutcomeUp})

	ring, ok := h.Ring("Example")
	if !ok || ring.Len() != 1 {
		t.Fatalf("tracked check holds %v points", ring)
	}
	if _, ok := h.Ring("Unknown Check"); ok {
		t.Error("an untracked check must not create a ring")
	}
}

func TestRingIsSafeForConcurrentUse(t *testing.T) {
	r := NewRing(64)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				r.Add(point(w*100+i, check.OutcomeUp))
				r.Uptime(epoch)
				r.Points()
			}
		}()
	}
	wg.Wait()
	if r.Len() != 64 {
		t.Errorf("held %d points, want a full ring of 64", r.Len())
	}
}
