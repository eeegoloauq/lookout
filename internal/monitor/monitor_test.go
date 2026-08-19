package monitor

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
	"github.com/eeegoloauq/lookout/internal/state"
)

// fakeProber records when it was called and returns scripted outcomes, so the
// scheduler can be tested without a network — which a synctest bubble forbids
// anyway.
type fakeProber struct {
	mu       sync.Mutex
	calls    map[string][]time.Time
	inFlight map[string]bool
	overlap  bool
	duration time.Duration
	outcome  func(name string, n int) check.Outcome
}

func newFakeProber() *fakeProber {
	return &fakeProber{calls: map[string][]time.Time{}, inFlight: map[string]bool{}}
}

func (f *fakeProber) Probe(ctx context.Context, c config.Check) check.Result {
	f.mu.Lock()
	n := len(f.calls[c.Name])
	f.calls[c.Name] = append(f.calls[c.Name], time.Now())
	if f.inFlight[c.Name] {
		f.overlap = true
	}
	f.inFlight[c.Name] = true
	outcome := check.OutcomeUp
	if f.outcome != nil {
		outcome = f.outcome(c.Name, n)
	}
	f.mu.Unlock()

	if f.duration > 0 {
		select {
		case <-time.After(f.duration):
		case <-ctx.Done():
		}
	}

	f.mu.Lock()
	f.inFlight[c.Name] = false
	f.mu.Unlock()

	return check.Result{Name: c.Name, At: time.Now(), Outcome: outcome, StatusCode: 200, Duration: f.duration}
}

func (f *fakeProber) times(name string) []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.calls[name]...)
}

func testConfig(t *testing.T, checks string) *config.Config {
	t.Helper()
	cfg, err := config.Load("config.yaml", []byte(checks))
	if err != nil {
		t.Fatalf("test config: %v", err)
	}
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	return cfg
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// runFor runs the monitor for a stretch of simulated time and returns once it
// has stopped. The clock inside the bubble is fake: this takes no real time.
func runFor(t *testing.T, m *Monitor, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	time.Sleep(d)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestScheduleIsRegularAndOffsetByCheckName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t, `
checks:
  - name: Alpha
    type: http
    url: http://alpha.invalid
    interval: 60s
    timeout: 5s
  - name: Beta
    type: http
    url: http://beta.invalid
    interval: 5m
    timeout: 5s
`)
		p := newFakeProber()
		m := New(cfg, p, WithLogger(quietLogger()))
		start := time.Now()
		runFor(t, m, time.Hour)

		for _, tc := range []struct {
			name     string
			interval time.Duration
		}{{"Alpha", time.Minute}, {"Beta", 5 * time.Minute}} {
			times := p.times(tc.name)
			offset := phase(tc.name, tc.interval)
			want := 1 + int((time.Hour-offset)/tc.interval) + 1
			if len(times) != want {
				t.Errorf("%s probed %d times in an hour, want %d", tc.name, len(times), want)
			}
			// The first probe is immediate: at startup, knowing beats spreading.
			if !times[0].Equal(start) {
				t.Errorf("%s first probe at %s, want it immediately at startup", tc.name, times[0].Sub(start))
			}
			// Every later probe sits on the offset grid, with no drift.
			for i, at := range times[1:] {
				want := start.Add(offset + time.Duration(i)*tc.interval)
				if !at.Equal(want) {
					t.Fatalf("%s probe %d at %s, want %s", tc.name, i+1, at.Sub(start), want.Sub(start))
				}
			}
		}

		// The offset comes from the name, so it is the same after a restart and
		// two checks do not fire in lockstep.
		if phase("Alpha", time.Minute) == phase("Beta", time.Minute) {
			t.Error("two check names hashed to the same phase; the herd is not spread")
		}
	})
}

// A slow probe must not push the following ones out: the period stays the
// interval instead of becoming interval plus probe duration.
func TestSlowProbesDoNotShiftTheSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t, `
checks:
  - name: Slow
    type: http
    url: http://slow.invalid
    interval: 60s
    timeout: 30s
`)
		p := newFakeProber()
		p.duration = 25 * time.Second
		m := New(cfg, p, WithLogger(quietLogger()))
		start := time.Now()
		runFor(t, m, 10*time.Minute)

		times := p.times("Slow")
		offset := phase("Slow", time.Minute)
		// A probe that runs into its own tick makes that tick be skipped, not
		// queued, so the grid itself never moves.
		for i, at := range times[1:] {
			if off := (at.Sub(start) - offset) % time.Minute; off != 0 {
				t.Fatalf("probe %d at %s is %s off the %s grid: the schedule drifted", i+1, at.Sub(start), off, time.Minute)
			}
			if i > 0 {
				if gap := at.Sub(times[i]); gap != time.Minute {
					t.Fatalf("probes %d and %d are %s apart, want %s", i, i+1, gap, time.Minute)
				}
			}
		}
		if p.overlap {
			t.Error("two probes of the same check overlapped")
		}
	})
}

func TestOutageProducesEventsAndPersistsState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t, `
checks:
  - name: Flaky
    type: http
    url: http://flaky.invalid
    interval: 60s
    timeout: 5s
`)
		p := newFakeProber()
		// Up for five probes, then down for the rest.
		p.outcome = func(_ string, n int) check.Outcome {
			if n < 5 {
				return check.OutcomeUp
			}
			return check.OutcomeDown
		}

		var mu sync.Mutex
		var events []state.Event
		m := New(cfg, p, WithLogger(quietLogger()), WithEventFunc(func(ev state.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		}))
		runFor(t, m, 15*time.Minute)

		mu.Lock()
		defer mu.Unlock()
		if len(events) != 1 || events[0].Kind != state.EventDown {
			t.Fatalf("events = %+v, want a single down event", events)
		}
		if !events[0].Alert {
			t.Error("a check with no explicit alert: must alert")
		}

		// The outage is on disk, so a restart does not re-announce it.
		snap, err := state.NewStore(cfg.StateFile).Load()
		if err != nil {
			t.Fatalf("loading state: %v", err)
		}
		if got := snap.Checks["Flaky"].Status; got != state.StatusDown {
			t.Errorf("persisted status = %q, want %q", got, state.StatusDown)
		}
		if snap.Checks["Flaky"].IncidentStart.IsZero() {
			t.Error("the incident start was not persisted")
		}
	})
}

func TestHistoryRecordsEveryProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t, `
checks:
  - name: Alpha
    type: http
    url: http://alpha.invalid
    interval: 60s
    timeout: 5s
`)
		p := newFakeProber()
		p.outcome = func(_ string, n int) check.Outcome {
			if n%2 == 0 {
				return check.OutcomeUp
			}
			return check.OutcomeDown
		}
		m := New(cfg, p, WithLogger(quietLogger()))
		start := time.Now()
		runFor(t, m, 10*time.Minute)

		ring, ok := m.History().Ring("Alpha")
		if !ok {
			t.Fatal("no history ring for a configured check")
		}
		if ring.Len() != len(p.times("Alpha")) {
			t.Errorf("history holds %d points for %d probes", ring.Len(), len(p.times("Alpha")))
		}
		ratio, samples := ring.Uptime(start)
		if samples == 0 || ratio < 0.4 || ratio > 0.6 {
			t.Errorf("uptime = %v over %d samples, want about half", ratio, samples)
		}
	})
}

// State from a previous process is picked up, and checks that left the
// configuration do not linger in the file.
func TestRestartRestoresStateAndPrunes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t, `
checks:
  - name: Alpha
    type: http
    url: http://alpha.invalid
    interval: 60s
    timeout: 5s
`)
		store := state.NewStore(cfg.StateFile)
		if err := store.Save(state.Snapshot{Checks: map[string]state.CheckState{
			"Alpha": {Status: state.StatusDown, ConsecutiveFailures: 3, IncidentStart: time.Now().Add(-time.Hour)},
			"Gone":  {Status: state.StatusUp},
		}}); err != nil {
			t.Fatal(err)
		}

		p := newFakeProber()
		var events []state.Event
		var mu sync.Mutex
		m := New(cfg, p, WithLogger(quietLogger()), WithEventFunc(func(ev state.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		}))
		runFor(t, m, 5*time.Minute)

		mu.Lock()
		defer mu.Unlock()
		// The check was down when we stopped and is up now: that is a genuine
		// recovery and must be reported exactly once.
		if len(events) != 1 || events[0].Kind != state.EventUp {
			t.Fatalf("events = %+v, want a single up event", events)
		}
		snap, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := snap.Checks["Gone"]; ok {
			t.Error("state for a check that left the configuration was kept")
		}
	})
}

// Losing the state file must not produce a recovery for an outage this process
// never announced (SPEC §9).
func TestLostStateFileProducesNoEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t, `
checks:
  - name: Alpha
    type: http
    url: http://alpha.invalid
    interval: 60s
    timeout: 5s
`)
		var events []state.Event
		var mu sync.Mutex
		record := WithEventFunc(func(ev state.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		})

		// A first process sees an outage and records it.
		down := newFakeProber()
		down.outcome = func(string, int) check.Outcome { return check.OutcomeDown }
		runFor(t, New(cfg, down, WithLogger(quietLogger()), record), 10*time.Minute)

		mu.Lock()
		first := len(events)
		mu.Unlock()
		if first != 1 {
			t.Fatalf("first process emitted %d events, want 1", first)
		}

		// The state file is lost, and the service is healthy again.
		if err := os.Remove(cfg.StateFile); err != nil {
			t.Fatal(err)
		}
		runFor(t, New(cfg, newFakeProber(), WithLogger(quietLogger()), record), 10*time.Minute)

		mu.Lock()
		defer mu.Unlock()
		if len(events) != first {
			t.Fatalf("events after losing the state file = %+v, want none", events[first:])
		}
	})
}
