package state

import (
	"fmt"
	"math/bits"
	"strings"
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
	// EventStillDown is a reminder that a confirmed outage is still open.
	// Without it, something that breaks at 03:00 sends one message and is
	// never mentioned again; the escalating schedule (Reminders) is what
	// keeps a long outage from being read once and forgotten.
	EventStillDown EventKind = "still_down"
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
	// EventDrift is a DNS zone snapshot that no longer matches the
	// previous one. The first snapshot is a baseline and never fires this.
	EventDrift EventKind = "drift"
	// EventExpiry is a certificate or domain crossing an expiry tier.
	EventExpiry EventKind = "expiry"
	// EventStale is a domain registry that has not answered for
	// DomainStaleAfter. It is not a domain outage.
	EventStale EventKind = "stale"
	// EventUndelegated is a .ru-style state: that has lost the
	// DELEGATED flag. The domain has been switched off, which is
	// a current outage, not "expires in N days".
	EventUndelegated EventKind = "undelegated"
	// EventDelegated is the matching recovery: DELEGATED came back.
	EventDelegated EventKind = "delegated"
	// EventHeld is the digest of alerts that fired while a mute was
	// on. They are not dropped; they leave as one
	// message when the mute lifts.
	EventHeld EventKind = "held"
	// EventHeartbeat is lookout saying it is still running. Without it a
	// dead process is indistinguishable from a quiet homelab.
	EventHeartbeat EventKind = "heartbeat"
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

	// Downtime is set on EventUp and EventStillDown: how long the incident
	// has lasted so far.
	Downtime time.Duration `json:"downtime,omitempty"`
	// Failures and Window are set on EventUnstable.
	Failures int `json:"failures,omitempty"`
	Window   int `json:"window,omitempty"`

	// Summary is set on EventSummary: what the outbox folded together rather
	// than drop when the queue filled up.
	Summary *Summary `json:"summary,omitempty"`

	// Drift is set on EventDrift.
	Drift *Drift `json:"drift,omitempty"`
	// Expiry is set on EventExpiry.
	Expiry *Expiry `json:"expiry,omitempty"`
	// StaleFor is set on EventStale: how long the registry has been silent.
	StaleFor time.Duration `json:"stale_for,omitempty"`

	// Heartbeat is set on EventHeartbeat.
	Heartbeat *Heartbeat `json:"heartbeat,omitempty"`
}

// Heartbeat is what lookout reports about itself, not about any one check.
type Heartbeat struct {
	Checks int `json:"checks"`
	Down   int `json:"down"`
	Closed int `json:"closed"`
}

// Drift is a zone snapshot that changed.
type Drift struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

// ExpiryKind names what is running out.
const (
	ExpiryCertificate = "certificate"
	ExpiryDomain      = "domain"
)

// Expiry is a crossed notification tier.
type Expiry struct {
	Kind      string    `json:"kind"`
	DaysLeft  int       `json:"days_left"`
	Threshold int       `json:"threshold,omitempty"` // 0 = daily
	NotAfter  time.Time `json:"not_after"`
	State     string    `json:"state,omitempty"`
	FreeDate  time.Time `json:"free_date,omitzero"`
	Source    string    `json:"source,omitempty"`
}

// Machine turns a stream of results into confirmed states and events. It is
// safe for concurrent use: probes run in their own goroutines.
type Machine struct {
	mu sync.Mutex
	// reminders is the escalating gap between notices about one open
	// incident; the last value repeats. Empty means a state change is the
	// only thing that ever notifies.
	reminders []time.Duration
	checks    map[string]*entry
	dirty     bool
	updated   time.Time
}

// SetReminders installs the still-down schedule (config alerting.reminders).
func (m *Machine) SetReminders(d []time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reminders = append([]time.Duration(nil), d...)
}

type entry struct {
	CheckState
	// recent is a bitmask of the last results, newest in bit 0, 1 meaning
	// failure. It is deliberately not durable: losing
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

	if r.RemoteAddr != "" {
		e.RemoteAddr = r.RemoteAddr
	}
	event := Event{Check: c.Name, Group: c.Group, At: r.At, Alert: c.Alert, Result: r}
	var events []Event

	if r.Outcome == check.OutcomeUnknown {
		events = append(events, e.observeUnknown(c, event)...)
	} else {
		failed := r.Outcome.Failed()
		if failed {
			e.ConsecutiveSuccesses = 0
			e.ConsecutiveFailures++
			e.LastFailureAt = r.At
			e.LastFailureReason = failureReason(r)
			if e.ConsecutiveFailures == 1 {
				e.FirstFailureAt = r.At
			}
		} else {
			e.ConsecutiveFailures = 0
			e.ConsecutiveSuccesses++
		}
		e.observe(failed, c.Instability.Window)

		switch {
		case failed && e.Status != StatusDown && e.ConsecutiveFailures >= c.FailureThreshold:
			e.Status = StatusDown
			e.IncidentStart = e.FirstFailureAt
			e.LastChange = r.At
			e.DownNoticeAt = r.At
			e.DownReminders = 0
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
				e.recordIncident(r.At)
				events = append(events, event)
				// The incident has been reported; the failures that made it up must
				// not be reported a second time as instability.
				e.recent, e.seen = 0, 0
				e.Unstable = false
			}
			e.DownNoticeAt = time.Time{}
			e.DownReminders = 0
			// StatusUnknown -> StatusUp emits nothing. Coming back from a lost
			// state file is not a recovery: nobody was ever told about an outage.
			e.IncidentStart = time.Time{}
		}

		if ev, ok := e.instability(c, event); ok {
			events = append(events, ev)
		}
		if ev, ok := e.reminder(m.reminders, failed, event); ok {
			events = append(events, ev)
		}
	}

	events = append(events, e.observeDrift(c, event)...)
	events = append(events, e.observeCert(c, event)...)
	events = append(events, e.observeDomain(c, event)...)
	events = append(events, e.observeDelegation(c, event)...)

	if !e.CheckState.sameAs(before) {
		m.dirty = true
		m.updated = r.At
	}
	return events
}

func (e *entry) observeUnknown(c config.Check, base Event) []Event {
	if c.Type != config.TypeDomain {
		return nil
	}
	if e.DomainUnknownSince.IsZero() {
		e.DomainUnknownSince = base.At
	}
	if e.DomainStaleNotice || base.At.Sub(e.DomainUnknownSince) < DomainStaleAfter {
		return nil
	}
	e.DomainStaleNotice = true
	base.Kind = EventStale
	base.StaleFor = base.At.Sub(e.DomainUnknownSince)
	return []Event{base}
}

func (e *entry) observeDrift(c config.Check, base Event) []Event {
	if c.Type != config.TypeDNS || base.Result.ZoneSnapshot == "" {
		return nil
	}
	next := base.Result.ZoneSnapshot
	if e.ZoneSnapshot == "" {
		e.ZoneSnapshot = next
		e.ZoneSnapshotAt = base.At
		return nil
	}
	if e.ZoneSnapshot == next {
		return nil
	}
	ev := base
	ev.Kind = EventDrift
	ev.Drift = &Drift{Before: e.ZoneSnapshot, After: next}
	e.ZoneSnapshot = next
	e.ZoneSnapshotAt = base.At
	return []Event{ev}
}

func (e *entry) observeCert(_ config.Check, base Event) []Event {
	next := base.Result.CertNotAfter
	if next.IsZero() {
		return nil
	}
	if renewed(e.CertNotAfter, next) {
		e.CertTiersFired = 0
		e.CertDailyOn = ""
	}
	e.CertNotAfter = next
	days := DaysLeft(next, base.At)
	th, fired, daily, fire := nextTier(days, CertTiers, e.CertTiersFired, e.CertDailyOn, base.At)
	e.CertTiersFired = fired
	e.CertDailyOn = daily
	if !fire {
		return nil
	}
	ev := base
	ev.Kind = EventExpiry
	ev.Expiry = &Expiry{Kind: ExpiryCertificate, DaysLeft: days, Threshold: th, NotAfter: next}
	return []Event{ev}
}

func (e *entry) observeDomain(c config.Check, base Event) []Event {
	if c.Type != config.TypeDomain {
		return nil
	}
	next := base.Result.DomainExpiresAt
	if next.IsZero() {
		return nil
	}
	if renewed(e.DomainExpiresAt, next) {
		e.DomainTiersFired = 0
		e.DomainDailyOn = ""
	}
	e.DomainExpiresAt = next
	e.DomainFreeDate = base.Result.DomainFreeDate
	e.DomainState = base.Result.DomainState
	e.DomainSource = base.Result.DomainSource
	e.DomainUpdatedAt = base.At
	e.DomainUnknownSince = time.Time{}
	e.DomainStaleNotice = false

	days := DaysLeft(next, base.At)
	th, fired, daily, fire := nextTier(days, DomainTiers, e.DomainTiersFired, e.DomainDailyOn, base.At)
	e.DomainTiersFired = fired
	e.DomainDailyOn = daily
	if !fire {
		return nil
	}
	ev := base
	ev.Kind = EventExpiry
	ev.Expiry = &Expiry{
		Kind:      ExpiryDomain,
		DaysLeft:  days,
		Threshold: th,
		NotAfter:  next,
		State:     base.Result.DomainState,
		FreeDate:  base.Result.DomainFreeDate,
		Source:    base.Result.DomainSource,
	}
	return []Event{ev}
}

func (e *entry) observeDelegation(c config.Check, base Event) []Event {
	if c.Type != config.TypeDomain {
		return nil
	}
	delegated, known := tcinetDelegation(base.Result.DomainState)
	if !known {
		return nil
	}
	if delegated {
		e.DomainDelegated = true
		e.DomainDelegationKnown = true
		if !e.DomainUndelegatedNotice {
			return nil
		}
		e.DomainUndelegatedNotice = false
		ev := base
		ev.Kind = EventDelegated
		return []Event{ev}
	}
	e.DomainDelegated = false
	e.DomainDelegationKnown = true
	if e.DomainUndelegatedNotice {
		return nil
	}
	e.DomainUndelegatedNotice = true
	ev := base
	ev.Kind = EventUndelegated
	return []Event{ev}
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

// reminder repeats an open outage on the configured schedule. It fires only
// for a check this process has already confirmed down and told someone about,
// so a restart cannot turn a silent unknown into a reminder, and the first
// gap is measured from the DOWN alert rather than from the incident start.
func (e *entry) reminder(schedule []time.Duration, failed bool, base Event) (Event, bool) {
	// A probe that just succeeded is a recovery in progress, not a reminder:
	// "still down" must never be sent about a check that is answering again.
	if !failed || len(schedule) == 0 || e.Status != StatusDown || e.DownNoticeAt.IsZero() {
		return Event{}, false
	}
	i := e.DownReminders
	if i >= len(schedule) {
		i = len(schedule) - 1
	}
	gap := schedule[i]
	if gap <= 0 || base.At.Sub(e.DownNoticeAt) < gap {
		return Event{}, false
	}
	e.DownNoticeAt = base.At
	e.DownReminders++
	base.Kind = EventStillDown
	base.Downtime = base.At.Sub(e.IncidentStart)
	return base, true
}

func (e *entry) failuresInWindow(window int) int {
	mask := uint64(1)<<uint(min(window, 64)) - 1
	if window >= 64 {
		mask = ^uint64(0)
	}
	return bits.OnesCount64(e.recent & mask)
}

// recordIncident closes the current outage and files it, newest first. The
// reason kept is the last failure seen during the outage, which is what the
// operator will be looking for when they ask what happened at 03:40.
func (e *entry) recordIncident(end time.Time) {
	if e.IncidentStart.IsZero() {
		return
	}
	inc := Incident{Start: e.IncidentStart, End: end, Reason: e.LastFailureReason}
	e.Incidents = append([]Incident{inc}, e.Incidents...)
	if len(e.Incidents) > MaxIncidents {
		e.Incidents = e.Incidents[:MaxIncidents]
	}
}

// failureReason is the short, secret-free explanation kept on the check for
// the status page. It is bounded because it ends up in the durable state
// file, and an API that answers with a megabyte of HTML must not grow it.
func failureReason(r check.Result) string {
	reason := check.RedactSecrets(r.Reason())
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		// Nothing named a condition: say what did arrive, so the panel
		// never shows a failure with an empty explanation.
		switch {
		case r.StatusCode > 0:
			reason = fmt.Sprintf("HTTP %d", r.StatusCode)
		case r.Rcode != "":
			reason = "DNS " + r.Rcode
		default:
			reason = string(r.Outcome)
		}
	}
	const max = 200
	if len(reason) > max {
		reason = reason[:max-1] + "…"
	}
	return reason
}
