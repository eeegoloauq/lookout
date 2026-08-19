package state

import (
	"math/bits"
	"sync"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
)

// EventKind is what happened to a check.
type EventKind string

const (
	// EventDown is a confirmed outage: the failure threshold was reached.
	EventDown EventKind = "down"
	// EventUp is a confirmed recovery. It is only ever emitted for a check
	// that this process previously confirmed as down, so state that was lost
	// cannot produce a phantom recovery.
	EventUp EventKind = "up"
	// EventUnstable is the "N of the last M" verdict: a check that keeps
	// alternating never reaches the failure threshold, so without this it
	// would deliver 50% of requests and still look perfect.
	EventUnstable EventKind = "unstable"
	// EventSummary is produced by the outbox itself when the queue would
	// otherwise drop events: a digest of what overflow discarded, so silence
	// cannot be the overflow policy.
	EventSummary EventKind = "summary"
)

// Event is a state change worth telling someone about. It is written to the
// durable outbox before anyone tries to deliver it, so a crash or a dead
// channel cannot turn a confirmed incident into silence.
type Event struct {
	Kind  EventKind `json:"kind"`
	Check string    `json:"check,omitempty"`
	Group string    `json:"group,omitempty"`
	At    time.Time `json:"at,omitzero"`
	// Alert carries the check's alert setting, which defaults to true. It
	// travels with the event so that the decision is made from the config and
	// not from the absence of one.
	Alert  bool         `json:"alert"`
	Result check.Result `json:"result"`

	// Downtime is set on EventUp: how long the incident lasted.
	Downtime time.Duration `json:"downtime,omitempty"`
	// Failures and Window are set on EventUnstable.
	Failures int `json:"failures,omitempty"`
	Window   int `json:"window,omitempty"`

	// Summary is set on EventSummary: what the outbox folded together rather
	// than drop when the queue filled up.
	Summary *Summary `json:"summary,omitempty"`
}

// Machine turns a stream of results into confirmed states and events. It is
// safe for concurrent use: probes run in their own goroutines.
type Machine struct {
	mu      sync.Mutex
	checks  map[string]*entry
	dirty   bool
	updated time.Time
}

type entry struct {
	CheckState
	// recent is a bitmask of the last results, newest in bit 0, 1 meaning
	// failure. It is deliberately not durable (SPEC §9.1 lists what is): losing
	// it can only delay an instability notice, never invent one.
	recent uint64
	seen   int
}

// NewMachine returns an empty machine. Empty means "nothing is known yet",
// which is exactly what a lost state file must degrade to.
func NewMachine() *Machine {
	return &Machine{checks: map[string]*entry{}}
}

// Restore loads durable state from a snapshot.
func (m *Machine) Restore(snap Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, cs := range snap.Checks {
		if cs.Status == "" {
			cs.Status = StatusUnknown
		}
		m.checks[name] = &entry{CheckState: cs}
	}
	m.updated = snap.UpdatedAt
}

// Prune drops state for checks that are no longer configured, so a renamed
// check cannot resurrect an old incident.
func (m *Machine) Prune(names []string) {
	keep := make(map[string]bool, len(names))
	for _, n := range names {
		keep[n] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for name := range m.checks {
		if !keep[name] {
			delete(m.checks, name)
			m.dirty = true
		}
	}
}

// Status reports the confirmed state of a check.
func (m *Machine) Status(name string) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.checks[name]
	if !ok {
		return StatusUnknown
	}
	return e.Status
}

// State returns a copy of the durable state of a check.
func (m *Machine) State(name string) (CheckState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.checks[name]
	if !ok {
		return CheckState{}, false
	}
	return e.CheckState, true
}

// Snapshot returns the durable state, ready to be written out.
func (m *Machine) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap := Snapshot{
		Version:   SnapshotVersion,
		UpdatedAt: m.updated,
		Checks:    make(map[string]CheckState, len(m.checks)),
	}
	for name, e := range m.checks {
		snap.Checks[name] = e.CheckState
	}
	return snap
}

// Dirty reports whether the durable state changed since the last call to
// ClearDirty. State is written when it changes and not on every probe.
func (m *Machine) Dirty() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dirty
}

// ClearDirty marks the current state as persisted.
func (m *Machine) ClearDirty() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirty = false
}

// Observe folds one result into the state of its check and returns the events
// it caused, if any.
func (m *Machine) Observe(c config.Check, r check.Result) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.checks[c.Name]
	if !ok {
		e = &entry{CheckState: CheckState{Status: StatusUnknown}}
		m.checks[c.Name] = e
	}
	before := e.CheckState

	failed := r.Outcome.Failed()
	if failed {
		e.ConsecutiveSuccesses = 0
		e.ConsecutiveFailures++
		if e.ConsecutiveFailures == 1 {
			e.FirstFailureAt = r.At
		}
	} else {
		e.ConsecutiveFailures = 0
		e.ConsecutiveSuccesses++
	}
	e.observe(failed, c.Instability.Window)

	event := Event{Check: c.Name, Group: c.Group, At: r.At, Alert: c.Alert, Result: r}
	var events []Event

	switch {
	case failed && e.Status != StatusDown && e.ConsecutiveFailures >= c.FailureThreshold:
		e.Status = StatusDown
		e.IncidentStart = e.FirstFailureAt
		e.LastChange = r.At
		// A confirmed incident supersedes any instability notice: the operator
		// is being told about this check already.
		e.Unstable = false
		event.Kind = EventDown
		events = append(events, event)

	case !failed && e.Status != StatusUp && e.ConsecutiveSuccesses >= c.SuccessThreshold:
		wasDown := e.Status == StatusDown
		e.Status = StatusUp
		e.LastChange = r.At
		if wasDown {
			event.Kind = EventUp
			event.Downtime = r.At.Sub(e.IncidentStart)
			events = append(events, event)
			// The incident has been reported; the failures that made it up must
			// not be reported a second time as instability.
			e.recent, e.seen = 0, 0
			e.Unstable = false
		}
		// StatusUnknown -> StatusUp emits nothing. Coming back from a lost
		// state file is not a recovery: nobody was ever told about an outage.
		e.IncidentStart = time.Time{}
	}

	if ev, ok := e.instability(c, event); ok {
		events = append(events, ev)
	}

	if e.CheckState != before {
		m.dirty = true
		m.updated = r.At
	}
	return events
}

// observe pushes one result into the sliding window.
func (e *entry) observe(failed bool, window int) {
	e.recent <<= 1
	if failed {
		e.recent |= 1
	}
	if e.seen < window {
		e.seen++
	}
}

// instability applies the "N of the last M" rule. It is suppressed while a
// check is confirmed down, because a sustained outage is already being
// reported and is a different diagnosis from flapping.
func (e *entry) instability(c config.Check, base Event) (Event, bool) {
	failures := e.failuresInWindow(c.Instability.Window)
	if e.Status == StatusDown || failures < c.Instability.Failures {
		if e.Unstable && failures == 0 {
			// A clean window ends the instability quietly; the next episode
			// gets its own notice.
			e.Unstable = false
		}
		return Event{}, false
	}
	if e.Unstable && base.At.Sub(e.UnstableNoticeAt) < c.Instability.Cooldown {
		return Event{}, false
	}
	e.Unstable = true
	e.UnstableNoticeAt = base.At
	base.Kind = EventUnstable
	base.Failures = failures
	base.Window = min(e.seen, c.Instability.Window)
	return base, true
}

func (e *entry) failuresInWindow(window int) int {
	mask := uint64(1)<<uint(min(window, 64)) - 1
	if window >= 64 {
		mask = ^uint64(0)
	}
	return bits.OnesCount64(e.recent & mask)
}
