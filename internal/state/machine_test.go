package state

import (
	"strings"
	"testing"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
)

var epoch = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

func testCheck() config.Check {
	cfg, err := config.Load("config.yaml", []byte(`
checks:
  - name: Example
    group: Services
    type: http
    url: http://example.invalid
    interval: 60s
    timeout: 5s
`))
	if err != nil {
		panic(err)
	}
	return cfg.Checks[0]
}

// feed applies a pattern of results, one per interval: 'U' up, 'D' down,
// 'M' malformed. It returns every event produced, in order.
func feed(m *Machine, c config.Check, pattern string) []Event {
	return feedFrom(m, c, pattern, epoch)
}

// feedFrom is feed starting at an explicit time, for tests where the gap
// between two batches of results is what is being asserted.
func feedFrom(m *Machine, c config.Check, pattern string, start time.Time) []Event {
	var events []Event
	for i, ch := range pattern {
		var outcome check.Outcome
		switch ch {
		case 'U':
			outcome = check.OutcomeUp
		case 'D':
			outcome = check.OutcomeDown
		case 'M':
			outcome = check.OutcomeMalformed
		default:
			panic("unknown result " + string(ch))
		}
		at := start.Add(time.Duration(i) * c.Interval)
		events = append(events, m.Observe(c, check.Result{Name: c.Name, At: at, Outcome: outcome})...)
	}
	return events
}

func kinds(events []Event) string {
	var b strings.Builder
	for _, e := range events {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(string(e.Kind))
	}
	return b.String()
}

func TestThresholds(t *testing.T) {
	c := testCheck()
	tests := []struct {
		name       string
		pattern    string
		wantEvents string
		wantStatus Status
	}{
		{"nothing yet", "U", "", StatusUnknown},
		{"confirmed up is silent", "UU", "", StatusUp},
		{"two failures are not enough", "UUDD", "", StatusUp},
		{"three failures confirm an outage", "UUDDD", "down", StatusDown},
		{"one success does not confirm recovery", "UUDDDU", "down", StatusDown},
		{"two successes confirm recovery", "UUDDDUU", "down up", StatusUp},
		{"a success resets the failure streak", "UUDDUDD", "", StatusUp},
		{"malformed results count as failures", "UUMMM", "down", StatusDown},
		{"an outage found before any success still alerts", "DDD", "down", StatusDown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMachine()
			got := feed(m, c, tc.pattern)
			if kinds(got) != tc.wantEvents {
				t.Errorf("events = %q, want %q", kinds(got), tc.wantEvents)
			}
			if s := m.Status(c.Name); s != tc.wantStatus {
				t.Errorf("status = %q, want %q", s, tc.wantStatus)
			}
		})
	}
}

func TestDownEventCarriesTheFailingResult(t *testing.T) {
	c := testCheck()
	m := NewMachine()
	res := check.Result{
		Name:       c.Name,
		At:         epoch,
		Outcome:    check.OutcomeDown,
		StatusCode: 503,
		Failures:   []check.Failure{{Condition: "status", Want: "200-299", Got: "503"}},
	}
	var events []Event
	for range c.FailureThreshold {
		events = append(events, m.Observe(c, res)...)
	}
	if len(events) != 1 || events[0].Kind != EventDown {
		t.Fatalf("events = %v, want one down event", kinds(events))
	}
	e := events[0]
	if e.Check != "Example" || e.Group != "Services" {
		t.Errorf("event identifies %q/%q, want Example/Services", e.Check, e.Group)
	}
	// A check that says nothing about alerting must alert (SPEC §1.1).
	if !e.Alert {
		t.Error("event.Alert = false for a check with no explicit alert:")
	}
	if e.Result.StatusCode != 503 || e.Result.Reason() == "" {
		t.Errorf("event does not carry what failed: %+v", e.Result)
	}
}

func TestRecoveryReportsDowntime(t *testing.T) {
	c := testCheck()
	m := NewMachine()
	events := feed(m, c, "UUDDDDUU")
	if kinds(events) != "down up" {
		t.Fatalf("events = %q, want %q", kinds(events), "down up")
	}
	// The incident starts at the first failure (index 2), the recovery is
	// confirmed at index 7.
	if want := 5 * c.Interval; events[1].Downtime != want {
		t.Errorf("downtime = %s, want %s", events[1].Downtime, want)
	}
}

// The signature of one bad backend behind a load balancer: never three
// failures in a row, so thresholds alone stay silent forever at 50% uptime.
func TestAlternatingFailuresAreCaught(t *testing.T) {
	c := testCheck()
	m := NewMachine()
	events := feed(m, c, strings.Repeat("UD", 10))

	if kinds(events) != "unstable" {
		t.Fatalf("events = %q, want exactly one unstable event", kinds(events))
	}
	e := events[0]
	if e.Failures != c.Instability.Failures {
		t.Errorf("failures = %d, want %d (the detector must fire at the threshold, not later)", e.Failures, c.Instability.Failures)
	}
	if e.Window < c.Instability.Failures || e.Window > c.Instability.Window {
		t.Errorf("window = %d, want it between %d and %d", e.Window, c.Instability.Failures, c.Instability.Window)
	}
	if m.Status(c.Name) == StatusDown {
		t.Error("an unstable check must not be reported as a confirmed outage")
	}
}

func TestInstabilityRespectsCooldown(t *testing.T) {
	c := testCheck()
	c.Instability.Cooldown = 10 * c.Interval
	m := NewMachine()

	// 30 alternating results: without a cooldown this would notify on every
	// failure once the window is full.
	events := feed(m, c, strings.Repeat("UD", 15))
	if len(events) < 2 {
		t.Fatalf("events = %q, want a repeat notice after the cooldown", kinds(events))
	}
	for i, e := range events {
		if e.Kind != EventUnstable {
			t.Fatalf("event %d = %q, want only unstable events", i, e.Kind)
		}
		if i > 0 {
			if gap := e.At.Sub(events[i-1].At); gap < c.Instability.Cooldown {
				t.Errorf("notices %d and %d are %s apart, cooldown is %s", i-1, i, gap, c.Instability.Cooldown)
			}
		}
	}
}

// A sustained outage is already reported as an incident; it must not also be
// reported as instability, before or after it recovers.
func TestSustainedOutageIsNotReportedAsInstability(t *testing.T) {
	c := testCheck()
	m := NewMachine()
	events := feed(m, c, "UU"+strings.Repeat("D", 15)+"UUUUU")
	if kinds(events) != "down up" {
		t.Errorf("events = %q, want %q", kinds(events), "down up")
	}
}

func TestInstabilityIsSuppressedWhileDown(t *testing.T) {
	c := testCheck()
	m := NewMachine()
	// Five failures, one success, five failures: the window holds ten
	// failures, but the check is confirmed down, so the outage is the story.
	events := feed(m, c, strings.Repeat("D", 5)+"U"+strings.Repeat("D", 5))
	if kinds(events) != "down" {
		t.Errorf("events = %q, want %q", kinds(events), "down")
	}
}

func TestAlertFalseIsCarriedOnEvents(t *testing.T) {
	c := testCheck()
	c.Alert = false
	m := NewMachine()
	events := feed(m, c, "DDD")
	if len(events) != 1 {
		t.Fatalf("events = %q, want one", kinds(events))
	}
	if events[0].Alert {
		t.Error("event.Alert = true for a check with alert: false")
	}
}

func TestPruneDropsCheckssThatLeftTheConfig(t *testing.T) {
	c := testCheck()
	m := NewMachine()
	feed(m, c, "DDD")
	m.Prune([]string{"Something Else"})
	if _, ok := m.State(c.Name); ok {
		t.Error("state survived a check leaving the configuration")
	}
	if !m.Dirty() {
		t.Error("pruning must mark the state dirty so the file shrinks too")
	}
}

func TestDirtyOnlyOnChange(t *testing.T) {
	c := testCheck()
	m := NewMachine()
	feed(m, c, "UU")
	m.ClearDirty()
	// A second confirmed success changes nothing but the streak counter,
	// which is itself durable, so this is expected to be dirty.
	feed(m, c, "U")
	if !m.Dirty() {
		t.Error("a changed success streak must be persisted")
	}
	m.ClearDirty()
	if m.Dirty() {
		t.Error("Dirty stayed set after ClearDirty")
	}
}

// A long outage has to keep saying it is an outage. The gaps escalate so a
// night-time failure is mentioned again in the morning without turning the
// chat into a ticker.
func TestStillDownRemindersFollowTheSchedule(t *testing.T) {
	c := testCheck()
	m := NewMachine()
	m.SetReminders([]time.Duration{time.Hour, 4 * time.Hour, 24 * time.Hour})

	if got := kinds(feed(m, c, "UUDDD")); got != "down" {
		t.Fatalf("confirming the outage produced %q", got)
	}
	// Probes keep landing every interval; only the schedule may notify.
	// Gaps are measured from the DOWN alert, which is the fifth result.
	notice := epoch.Add(4 * c.Interval)
	at := epoch.Add(5 * c.Interval)
	var fired []time.Duration
	for elapsed := time.Duration(0); elapsed <= 40*time.Hour; elapsed += c.Interval {
		evs := m.Observe(c, check.Result{Name: c.Name, At: at.Add(elapsed), Outcome: check.OutcomeDown})
		for _, ev := range evs {
			if ev.Kind == EventStillDown {
				fired = append(fired, ev.At.Sub(notice).Round(time.Minute))
			}
		}
	}
	want := []time.Duration{time.Hour, 5 * time.Hour, 29 * time.Hour}
	if len(fired) != len(want) {
		t.Fatalf("reminders fired at %v, want %v", fired, want)
	}
	for i := range want {
		if fired[i] != want[i] {
			t.Fatalf("reminders fired at %v, want %v", fired, want)
		}
	}
}

func TestNoRemindersWithoutASchedule(t *testing.T) {
	c := testCheck()
	m := NewMachine()
	feed(m, c, "UUDDD")
	for i := range 200 {
		at := epoch.Add(time.Duration(5+i) * c.Interval)
		for _, ev := range m.Observe(c, check.Result{Name: c.Name, At: at, Outcome: check.OutcomeDown}) {
			if ev.Kind == EventStillDown {
				t.Fatalf("reminder fired with no schedule configured")
			}
		}
	}
}

// "Still down" about a check that just answered would be a lie, and the
// recovery clears the incident's reminder state so the next outage starts
// its schedule from scratch.
func TestReminderStopsAtRecovery(t *testing.T) {
	c := testCheck()
	m := NewMachine()
	m.SetReminders([]time.Duration{time.Hour})
	feed(m, c, "UUDDD")

	at := epoch.Add(5 * c.Interval)
	if evs := m.Observe(c, check.Result{Name: c.Name, At: at.Add(2 * time.Hour), Outcome: check.OutcomeUp}); kinds(evs) != "" {
		t.Fatalf("a single success produced %q", kinds(evs))
	}
	evs := m.Observe(c, check.Result{Name: c.Name, At: at.Add(2*time.Hour + c.Interval), Outcome: check.OutcomeUp})
	if kinds(evs) != "up" {
		t.Fatalf("recovery produced %q", kinds(evs))
	}
	st, _ := m.State(c.Name)
	if !st.DownNoticeAt.IsZero() || st.DownReminders != 0 {
		t.Fatalf("recovery left reminder state behind: %+v", st)
	}
}

// State that was lost must not page: an unknown check has never been
// reported down, so it has nothing to remind anyone about.
func TestReminderNeedsAReportedOutage(t *testing.T) {
	c := testCheck()
	m := NewMachine()
	m.SetReminders([]time.Duration{time.Hour})
	m.Restore(Snapshot{Checks: map[string]CheckState{c.Name: {Status: StatusDown}}})
	for i := range 300 {
		at := epoch.Add(time.Duration(i) * c.Interval)
		for _, ev := range m.Observe(c, check.Result{Name: c.Name, At: at, Outcome: check.OutcomeDown}) {
			if ev.Kind == EventStillDown {
				t.Fatalf("restored state produced a reminder for an outage nobody was told about")
			}
		}
	}
}
