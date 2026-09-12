package push

import (
	"strings"
	"testing"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
	"github.com/eeegoloauq/lookout/internal/state"
)

var epoch = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

// testCheck is the example from the README: a nightly job that reports in
// every ten minutes with two minutes of slack.
func testCheck(t *testing.T) config.Check {
	t.Helper()
	cfg, err := config.Load("config.yaml", []byte(`
checks:
  - name: ZFS tank
    group: Jobs
    type: push
    expect_every: 10m
    grace: 2m
    token: 0123456789abcdef
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg.Checks[0]
}

func TestPingOnTimeIsUp(t *testing.T) {
	s := NewStore()
	c := testCheck(t)
	s.Record(c.Name, state.PingOK, "", epoch)

	res := s.Evaluate(c, epoch.Add(9*time.Minute))
	if res.Outcome != check.OutcomeUp {
		t.Fatalf("outcome = %q (%s), want up", res.Outcome, res.Reason())
	}
}

// The grace is slack on the deadline, not decoration: a ping eleven minutes
// into a ten-minute deadline with two minutes of grace is still on time.
func TestGraceExtendsTheDeadline(t *testing.T) {
	s := NewStore()
	c := testCheck(t)
	s.Record(c.Name, state.PingOK, "", epoch)

	if res := s.Evaluate(c, epoch.Add(11*time.Minute)); res.Outcome != check.OutcomeUp {
		t.Fatalf("at 11m outcome = %q, want up (10m + 2m grace)", res.Outcome)
	}
	if res := s.Evaluate(c, epoch.Add(13*time.Minute)); res.Outcome != check.OutcomeDown {
		t.Fatalf("at 13m outcome = %q, want down", res.Outcome)
	}
}

func TestOverdueReasonSaysHowLateAndAgainstWhat(t *testing.T) {
	s := NewStore()
	c := testCheck(t)
	s.Record(c.Name, state.PingOK, "", epoch)

	res := s.Evaluate(c, epoch.Add(14*time.Minute))
	if res.Outcome != check.OutcomeDown {
		t.Fatalf("outcome = %q, want down", res.Outcome)
	}
	if got, want := res.Reason(), "no ping for 14m (expected every 10m)"; got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
}

// ok → overdue → ok, which is the whole life of a dead-man check.
func TestOverdueRecoversOnTheNextPing(t *testing.T) {
	s := NewStore()
	c := testCheck(t)
	s.Record(c.Name, state.PingOK, "", epoch)

	late := epoch.Add(20 * time.Minute)
	if res := s.Evaluate(c, late); res.Outcome != check.OutcomeDown {
		t.Fatalf("outcome = %q, want down", res.Outcome)
	}
	s.Record(c.Name, state.PingOK, "", late)
	if res := s.Evaluate(c, late.Add(time.Minute)); res.Outcome != check.OutcomeUp {
		t.Fatalf("after the next ping outcome = %q, want up", res.Outcome)
	}
}

// A job that is still alive enough to complain is the one case a dead-man
// switch gets a real reason for free.
func TestExplicitFailIsDownWithTheMessage(t *testing.T) {
	s := NewStore()
	c := testCheck(t)
	s.Record(c.Name, state.PingFail, "pool degraded: 1 device faulted", epoch)

	res := s.Evaluate(c, epoch.Add(time.Minute))
	if res.Outcome != check.OutcomeDown {
		t.Fatalf("outcome = %q, want down", res.Outcome)
	}
	if got := res.Reason(); got != "pool degraded: 1 device faulted" {
		t.Fatalf("reason = %q, want the message the ping carried", got)
	}
}

func TestExplicitFailWithoutAMessageStillSaysSomething(t *testing.T) {
	s := NewStore()
	c := testCheck(t)
	s.Record(c.Name, state.PingFail, "", epoch)

	if got := s.Evaluate(c, epoch).Reason(); got == "" {
		t.Fatal("a failed ping with no message produced an empty reason")
	}
}

// Silence outranks the last thing that was said: a job that reported a
// failure and then stopped reporting at all is the bigger problem.
func TestOverdueOutranksAnOldFailure(t *testing.T) {
	s := NewStore()
	c := testCheck(t)
	s.Record(c.Name, state.PingFail, "pool degraded", epoch)

	res := s.Evaluate(c, epoch.Add(time.Hour))
	if !strings.HasPrefix(res.Reason(), "no ping for") {
		t.Fatalf("reason = %q, want the silence rather than the old failure", res.Reason())
	}
}

// A check that has never pinged is not up, and it is not down until its
// first deadline has passed — but it does go down, or a token that was
// never wired up would be excused forever.
func TestFirstDeadlineRunsFromWhenLookoutStartedWaiting(t *testing.T) {
	s := NewStore()
	c := testCheck(t)

	if res := s.Evaluate(c, epoch); res.Outcome != check.OutcomeUnknown {
		t.Fatalf("first tick outcome = %q, want unknown", res.Outcome)
	}
	if res := s.Evaluate(c, epoch.Add(5*time.Minute)); res.Outcome != check.OutcomeUnknown {
		t.Fatalf("inside the first deadline outcome = %q, want unknown", res.Outcome)
	}
	res := s.Evaluate(c, epoch.Add(13*time.Minute))
	if res.Outcome != check.OutcomeDown {
		t.Fatalf("past the first deadline outcome = %q, want down", res.Outcome)
	}
	if !strings.Contains(res.Reason(), "no ping yet") {
		t.Fatalf("reason = %q, want it to say nothing has ever pinged", res.Reason())
	}
}

// The durable half: a restart must not invent an outage out of its own
// downtime, and must not forget what the last ping said.
func TestRestoredPingSurvivesARestart(t *testing.T) {
	c := testCheck(t)
	first := NewStore()
	first.Record(c.Name, state.PingOK, "backup done", epoch)
	if !first.Dirty() {
		t.Fatal("a ping did not mark the store dirty, so it would never be written")
	}
	snap := first.Snapshot()

	second := NewStore()
	second.Restore(snap)
	if res := second.Evaluate(c, epoch.Add(3*time.Minute)); res.Outcome != check.OutcomeUp {
		t.Fatalf("after a restart outcome = %q, want up", res.Outcome)
	}
	last, ok := second.Last(c.Name)
	if !ok || last.Msg != "backup done" || !last.At.Equal(epoch) {
		t.Fatalf("restored ping = %+v, want the one that was written", last)
	}
}

// A restart while the deadline was already blown stays down: the outage
// belongs to the sender, and restoring must not reset its clock.
func TestRestoredPingStaysOverdue(t *testing.T) {
	c := testCheck(t)
	first := NewStore()
	first.Record(c.Name, state.PingOK, "", epoch)

	second := NewStore()
	second.Restore(first.Snapshot())
	res := second.Evaluate(c, epoch.Add(30*time.Minute))
	if res.Outcome != check.OutcomeDown {
		t.Fatalf("outcome = %q, want down", res.Outcome)
	}
	if got, want := res.Reason(), "no ping for 30m (expected every 10m)"; got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
}

// The waiting mark is durable too, or a crash loop would restart the first
// deadline on every boot and the check would never go down.
func TestWaitingSinceIsPersisted(t *testing.T) {
	c := testCheck(t)
	first := NewStore()
	first.Evaluate(c, epoch)
	if !first.Dirty() {
		t.Fatal("starting to wait did not mark the store dirty")
	}

	second := NewStore()
	second.Restore(first.Snapshot())
	if res := second.Evaluate(c, epoch.Add(13*time.Minute)); res.Outcome != check.OutcomeDown {
		t.Fatalf("outcome = %q, want down: the wait started before the restart", res.Outcome)
	}
}

func TestPruneDropsCheckThatLeftTheConfig(t *testing.T) {
	s := NewStore()
	s.Record("ZFS tank", state.PingOK, "", epoch)
	s.Record("Offsite copy", state.PingOK, "", epoch)
	s.Prune([]string{"ZFS tank"})
	if _, ok := s.Last("Offsite copy"); ok {
		t.Fatal("a check that left the config kept its ping")
	}
	if _, ok := s.Last("ZFS tank"); !ok {
		t.Fatal("a configured check lost its ping")
	}
}

// The endpoint is reachable from the network, so a burst from one sender
// must not turn into one fsync per request. The ping is still recorded;
// only the immediate write is skipped, and the next tick writes it.
func TestRepeatedPingsCoalesceTheDurableWrite(t *testing.T) {
	s := NewStore()
	if !s.Record("ZFS tank", state.PingOK, "", epoch) {
		t.Fatal("the first ping must be written out")
	}
	if s.Record("ZFS tank", state.PingOK, "", epoch.Add(SaveEvery/2)) {
		t.Fatal("a ping on top of the last one asked for its own write")
	}
	if !s.Record("ZFS tank", state.PingOK, "", epoch.Add(2*SaveEvery)) {
		t.Fatal("a ping a second later must be written out")
	}
	if !s.Dirty() {
		t.Fatal("a coalesced ping left the store clean, so it would never be written")
	}
}

func TestCleanTrimsAndTruncates(t *testing.T) {
	if got := Clean("  two   lines\nof noise \t"); got != "two lines of noise" {
		t.Errorf("Clean = %q, want the text on one line", got)
	}
	long := Clean(strings.Repeat("щ", 500))
	if n := len([]rune(long)); n != MaxMsg {
		t.Errorf("Clean kept %d runes, want %d", n, MaxMsg)
	}
	if !strings.HasSuffix(long, "…") {
		t.Errorf("a truncated message does not say it was cut: %q", long)
	}
}

// The state machine is what turns these results into events, and it must
// treat them like any other check's: thresholds, then DOWN, then UP.
func TestDeadlineDrivesTheStateMachine(t *testing.T) {
	s := NewStore()
	c := testCheck(t)
	m := state.NewMachine()

	tick := func(at time.Time) []state.Event {
		return m.Observe(c, s.Evaluate(c, at))
	}

	s.Record(c.Name, state.PingOK, "", epoch)
	now := epoch
	for range c.SuccessThreshold {
		now = now.Add(c.Interval)
		tick(now)
	}
	if got := m.Status(c.Name); got != state.StatusUp {
		t.Fatalf("status = %q, want up after %d good ticks", got, c.SuccessThreshold)
	}

	// Nothing pings again. The deadline passes, and the usual consecutive
	// failures confirm it.
	var down []state.Event
	now = epoch.Add(13 * time.Minute)
	for range c.FailureThreshold {
		down = append(down, tick(now)...)
		now = now.Add(c.Interval)
	}
	if len(down) != 1 || down[0].Kind != state.EventDown {
		t.Fatalf("events = %+v, want exactly one down", down)
	}
	if !strings.HasPrefix(down[0].Result.Reason(), "no ping for") {
		t.Fatalf("down reason = %q", down[0].Result.Reason())
	}

	s.Record(c.Name, state.PingOK, "", now)
	var up []state.Event
	for range c.SuccessThreshold {
		now = now.Add(c.Interval)
		up = append(up, tick(now)...)
	}
	if len(up) != 1 || up[0].Kind != state.EventUp {
		t.Fatalf("events = %+v, want exactly one up", up)
	}
}
