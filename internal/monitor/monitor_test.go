package monitor

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
	"github.com/eeegoloauq/lookout/internal/history"
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
	dir := t.TempDir()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.HistoryFile = filepath.Join(dir, "history.jsonl")
	cfg.SamplesFile = filepath.Join(dir, "samples.jsonl")
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
// never announced.
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

type recordingNotifier struct {
	mu       sync.Mutex
	err      error
	messages []string
}

func (r *recordingNotifier) Notify(ctx context.Context, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.messages = append(r.messages, text)
	return nil
}

func (r *recordingNotifier) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *recordingNotifier) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.messages...)
}

// A confirmed outage must become a delivered notification, not just a log
// line. This is the half of "silence must never be a bug" that "alert
// defaults to true" does not cover: the alert is configured and still has
// to arrive.
func TestOutageIsDeliveredThroughTheNotifier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t, `
alerting:
  batch_window: 45s
checks:
  - name: Photos
    group: Services
    type: http
    url: http://photos.invalid
    interval: 60s
    timeout: 5s
`)
		p := newFakeProber()
		p.outcome = func(string, int) check.Outcome { return check.OutcomeDown }
		n := &recordingNotifier{}
		m := New(cfg, p, WithLogger(quietLogger()), WithNotifier(n))
		runFor(t, m, 10*time.Minute)

		got := n.got()
		if len(got) != 1 {
			t.Fatalf("delivered %d messages, want 1: %v", len(got), got)
		}
		if !strings.Contains(got[0], "Photos is down") {
			t.Errorf("message = %q", got[0])
		}
		snap, err := state.NewStore(cfg.StateFile).Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Outbox.Items) != 0 {
			t.Errorf("outbox still holds %d items after delivery", len(snap.Outbox.Items))
		}
	})
}

func TestDeliveryFailureKeepsTheOutboxAcrossRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t, `
alerting:
  batch_window: 45s
checks:
  - name: Photos
    group: Services
    type: http
    url: http://photos.invalid
    interval: 60s
    timeout: 5s
`)
		down := newFakeProber()
		down.outcome = func(string, int) check.Outcome { return check.OutcomeDown }
		n := &recordingNotifier{}
		n.setErr(errUnavailable)
		runFor(t, New(cfg, down, WithLogger(quietLogger()), WithNotifier(n)), 10*time.Minute)

		if len(n.got()) != 0 {
			t.Fatalf("delivered while the channel was down: %v", n.got())
		}
		snap, err := state.NewStore(cfg.StateFile).Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Outbox.Items) != 1 {
			t.Fatalf("outbox = %d items, want the undelivered down event", len(snap.Outbox.Items))
		}

		n.setErr(nil)
		// The check stays down, so the state machine emits nothing new.
		// Delivery has to come from the restored outbox.
		stillDown := newFakeProber()
		stillDown.outcome = func(string, int) check.Outcome { return check.OutcomeDown }
		runFor(t, New(cfg, stillDown, WithLogger(quietLogger()), WithNotifier(n)), 2*time.Minute)

		got := n.got()
		if len(got) != 1 || !strings.Contains(got[0], "Photos is down") {
			t.Fatalf("after restart: %v", got)
		}
		snap, err = state.NewStore(cfg.StateFile).Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Outbox.Items) != 0 {
			t.Errorf("outbox still holds %d items after the retry", len(snap.Outbox.Items))
		}
	})
}

func TestMuteSuppressesDeliveryAndFlushesADigest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t, `
alerting:
  batch_window: 45s
checks:
  - name: Photos
    group: Services
    type: http
    url: http://photos.invalid
    interval: 60s
    timeout: 5s
  - name: Router
    group: Core
    type: http
    url: http://router.invalid
    interval: 60s
    timeout: 5s
`)
		p := newFakeProber()
		p.outcome = func(string, int) check.Outcome { return check.OutcomeDown }
		n := &recordingNotifier{}
		m := New(cfg, p, WithLogger(quietLogger()), WithNotifier(n))
		if _, err := m.Mute(30*time.Minute, "Services", ""); err != nil {
			t.Fatal(err)
		}
		runFor(t, m, 10*time.Minute)

		got := n.got()
		for _, msg := range got {
			if strings.Contains(msg, "Photos") && strings.Contains(msg, "DOWN") {
				t.Fatalf("Photos was delivered while muted: %v", got)
			}
		}
		// Core is not muted: the router outage must still arrive.
		foundRouter := false
		for _, msg := range got {
			if strings.Contains(msg, "Router is down") {
				foundRouter = true
			}
		}
		if !foundRouter {
			t.Fatalf("unmuted group was also silenced: %v", got)
		}

		// History must still have been recording the muted check.
		ring, _ := m.History().Ring("Photos")
		if ring.Len() == 0 {
			t.Fatal("mute must not stop probes or history")
		}

		n2 := &recordingNotifier{}
		m2 := New(cfg, newFakeProber(), WithLogger(quietLogger()), WithNotifier(n2))
		m2.Restore()
		// The mute is durable: a new process still holds it.
		if !m2.CheckMuted("Services", "Photos", time.Now()) {
			t.Fatal("mute did not survive the restart")
		}
		m2.Unmute("Services", "")
		runFor(t, m2, 2*time.Minute)
		held := false
		for _, msg := range n2.got() {
			if strings.Contains(msg, "mute ended") {
				held = true
			}
		}
		if !held {
			t.Fatalf("digest of muted events was lost: %v", n2.got())
		}
	})
}

// The 24-hour bar is why this process exists. A deploy that blanks it
// makes the board lie about the only window anyone looks at.
func TestRestartKeepsThe24HourHistory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t, `
checks:
  - name: Photos
    type: http
    url: http://photos.invalid
    interval: 60s
    timeout: 5s
`)
		p := newFakeProber()
		p.duration = 40 * time.Millisecond
		p.outcome = func(_ string, n int) check.Outcome {
			if n%4 == 0 {
				return check.OutcomeDown
			}
			return check.OutcomeUp
		}
		m := New(cfg, p, WithLogger(quietLogger()))
		runFor(t, m, 3*time.Hour)

		ring, ok := m.History().Ring("Photos")
		if !ok {
			t.Fatal("no ring")
		}
		wantPoints := ring.Points()
		wantRatio, wantSamples := ring.Uptime(time.Now().Add(-history.Retention))
		if wantSamples == 0 {
			t.Fatal("first process wrote no samples")
		}

		m2 := New(cfg, newFakeProber(), WithLogger(quietLogger()))
		m2.Restore()
		ring2, ok := m2.History().Ring("Photos")
		if !ok {
			t.Fatal("restart lost the ring")
		}
		got := ring2.Points()
		if len(got) != len(wantPoints) {
			t.Fatalf("restart held %d points, want %d", len(got), len(wantPoints))
		}
		for i := range wantPoints {
			if got[i].At.Unix() != wantPoints[i].At.Unix() ||
				got[i].Outcome != wantPoints[i].Outcome ||
				got[i].Duration.Milliseconds() != wantPoints[i].Duration.Milliseconds() ||
				got[i].StatusCode != wantPoints[i].StatusCode {
				t.Fatalf("point %d after restart = %+v, want %+v", i, got[i], wantPoints[i])
			}
		}
		ratio, samples := ring2.Uptime(time.Now().Add(-history.Retention))
		if samples != wantSamples || ratio != wantRatio {
			t.Fatalf("uptime after restart = %v over %d, want %v over %d", ratio, samples, wantRatio, wantSamples)
		}
	})
}

// A crash mid-write is a truncated last line, not a reason to stay
// down. A monitor that will not start is a silent one.
func TestCorruptSamplesFileDoesNotStopStartup(t *testing.T) {
	cfg := testConfig(t, `
checks:
  - name: Photos
    type: http
    url: http://photos.invalid
    interval: 60s
    timeout: 5s
`)
	now := time.Now().UTC().Truncate(time.Second)
	good := []byte(`{"t":` + strconv.FormatInt(now.Unix(), 10) + `,"c":"Photos","o":"up","ms":40,"s":200}` + "\n" + `{"t":1,"c":"Pho`)
	if err := os.WriteFile(cfg.SamplesFile, good, 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(cfg, newFakeProber(), WithLogger(quietLogger()))
	m.Restore()
	ring, ok := m.History().Ring("Photos")
	if !ok || ring.Len() != 1 {
		t.Fatalf("ring after corrupt tail = %v, want the one good sample", ring)
	}
}

func TestDailyHistoryFlushesAtUTCMidnightAndSurvivesRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t, `
checks:
  - name: Photos
    group: Services
    type: http
    url: http://photos.invalid
    interval: 60s
    timeout: 5s
`)
		p := newFakeProber()
		// Start at synctest's 2000-01-01 00:00 UTC; skip to 23:00 so
		// a handful of probes land on that UTC day, then cross midnight.
		m := New(cfg, p, WithLogger(quietLogger()))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- m.Run(ctx) }()
		time.Sleep(23 * time.Hour)
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}

		// Restart after the day has been accumulating, still before midnight.
		m2 := New(cfg, newFakeProber(), WithLogger(quietLogger()))
		ctx, cancel = context.WithCancel(context.Background())
		done = make(chan error, 1)
		go func() { done <- m2.Run(ctx) }()
		time.Sleep(2 * time.Hour) // crosses 00:00 UTC
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}

		log := history.NewLog(cfg.HistoryFile)
		if err := log.Load(); err != nil {
			t.Fatal(err)
		}
		recs := log.Records()
		found := 0
		for _, r := range recs {
			if r.Check == "Photos" && r.Date == "2000-01-01" {
				found++
				if r.Samples == 0 {
					t.Error("flushed a day with no samples")
				}
			}
		}
		if found != 1 {
			t.Fatalf("2000-01-01 Photos lines = %d, want 1 (no duplicate, no loss); records=%+v", found, recs)
		}

		// A third start must not rewrite the day.
		m3 := New(cfg, newFakeProber(), WithLogger(quietLogger()))
		runFor(t, m3, time.Minute)
		log = history.NewLog(cfg.HistoryFile)
		_ = log.Load()
		found = 0
		for _, r := range log.Records() {
			if r.Check == "Photos" && r.Date == "2000-01-01" {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("restart after midnight duplicated the day: %d", found)
		}
	})
}

func heartbeatCfg(t *testing.T, extra string) *config.Config {
	t.Helper()
	return testConfig(t, `
alerting:
  batch_window: 45s
  heartbeat: 1h
`+extra+`
checks:
  - name: Photos
    group: Services
    type: http
    url: http://photos.invalid
    interval: 60s
    timeout: 5s
    failure_threshold: 1
    success_threshold: 1
`)
}

// A dead lookout looks exactly like a quiet homelab. The first still-alive
// message has to leave without waiting a week, or the deadman never arms.
func TestHeartbeatFiresOnAFreshStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &recordingNotifier{}
		runFor(t, New(heartbeatCfg(t, ""), newFakeProber(), WithLogger(quietLogger()), WithNotifier(n)), time.Minute)
		got := n.got()
		if len(got) != 1 {
			t.Fatalf("delivered %d messages, want the first heartbeat: %v", len(got), got)
		}
		if !strings.Contains(got[0], "lookout is alive") {
			t.Errorf("message = %q", got[0])
		}
	})
}

// A restart is not a new week. Re-paging on every boot would train the
// operator to ignore the still-alive message.
func TestHeartbeatDoesNotRepageOnRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := heartbeatCfg(t, "")
		n := &recordingNotifier{}
		runFor(t, New(cfg, newFakeProber(), WithLogger(quietLogger()), WithNotifier(n)), time.Minute)
		if len(n.got()) != 1 {
			t.Fatalf("first run delivered %d, want 1: %v", len(n.got()), n.got())
		}
		n2 := &recordingNotifier{}
		runFor(t, New(cfg, newFakeProber(), WithLogger(quietLogger()), WithNotifier(n2)), 30*time.Minute)
		if len(n2.got()) != 0 {
			t.Fatalf("restart re-sent the heartbeat: %v", n2.got())
		}
	})
}

// A week of downtime is one missed ping, not seven. Catching up would page
// the operator for lookout having been down, which they already know.
func TestHeartbeatCatchUpIsOneMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := heartbeatCfg(t, "")
		snap := state.Snapshot{
			Version:       state.SnapshotVersion,
			Checks:        map[string]state.CheckState{},
			LastHeartbeat: time.Now().Add(-8 * time.Hour),
		}
		if err := state.NewStore(cfg.StateFile).Save(snap); err != nil {
			t.Fatal(err)
		}
		n := &recordingNotifier{}
		runFor(t, New(cfg, newFakeProber(), WithLogger(quietLogger()), WithNotifier(n)), time.Minute)
		if len(n.got()) != 1 {
			t.Fatalf("after 8h down delivered %d, want 1: %v", len(n.got()), n.got())
		}
	})
}

// The still-alive ping is an alert like any other: a dead channel must
// not drop it, and an outage that matures in the same window must share
// the message rather than steal the slot.
func TestHeartbeatBatchesWithAnOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := heartbeatCfg(t, "")
		p := newFakeProber()
		p.outcome = func(string, int) check.Outcome { return check.OutcomeDown }
		n := &recordingNotifier{}
		runFor(t, New(cfg, p, WithLogger(quietLogger()), WithNotifier(n)), time.Minute)
		got := n.got()
		if len(got) != 1 {
			t.Fatalf("got %d messages, want one batch: %v", len(got), got)
		}
		if !strings.Contains(got[0], "Photos") || !strings.Contains(got[0], "lookout is alive") {
			t.Errorf("batch missing an event:\n%s", got[0])
		}
	})
}

// The closed-incident count is why the ping exists besides "N checks":
// a week of flapping that recovered has to show up, or the message
// reads as if nothing happened.
func TestHeartbeatCountsIncidentsClosedSinceLastPing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := heartbeatCfg(t, "")
		snap := state.Snapshot{
			Version:       state.SnapshotVersion,
			Checks:        map[string]state.CheckState{},
			LastHeartbeat: time.Now(),
		}
		if err := state.NewStore(cfg.StateFile).Save(snap); err != nil {
			t.Fatal(err)
		}
		p := newFakeProber()
		p.outcome = func(_ string, n int) check.Outcome {
			if n == 0 {
				return check.OutcomeDown
			}
			return check.OutcomeUp
		}
		n := &recordingNotifier{}
		runFor(t, New(cfg, p, WithLogger(quietLogger()), WithNotifier(n)), time.Hour+45*time.Second+time.Second)
		got := n.got()
		var ping string
		for _, msg := range got {
			if strings.Contains(msg, "lookout is alive") {
				ping = msg
			}
		}
		if ping == "" {
			t.Fatalf("no heartbeat in %v", got)
		}
		if !strings.Contains(ping, "1 incident closed since the last heartbeat") {
			t.Errorf("closed count missing:\n%s", ping)
		}
		if !strings.Contains(ping, "0 currently down") {
			t.Errorf("down count missing:\n%s", ping)
		}
	})
}

// Off has to be silence, not a forgotten weekly default. An operator
// who did not ask for a still-alive message must not get one.
func TestHeartbeatOffSendsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t, `
alerting:
  batch_window: 45s
  heartbeat: 0s
checks:
  - name: Photos
    type: http
    url: http://photos.invalid
    interval: 60s
    timeout: 5s
`)
		n := &recordingNotifier{}
		runFor(t, New(cfg, newFakeProber(), WithLogger(quietLogger()), WithNotifier(n)), 2*time.Hour)
		if len(n.got()) != 0 {
			t.Fatalf("heartbeat: 0 still sent: %v", n.got())
		}
	})
}

var errUnavailable = errString("telegram unreachable")

type errString string

func (e errString) Error() string { return string(e) }
