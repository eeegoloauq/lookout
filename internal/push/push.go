// Package push holds the heartbeats a dead-man check waits for and turns
// their absence into a result.
//
// Every other check asks a question and reads the answer. A push check
// cannot: a nightly backup that stopped running is not there to say so, and
// the machine it ran on may be the thing that is gone. So the job reports in
// when it finishes, lookout writes down when it last did, and the scheduler
// compares that against the deadline on its own tick. The report is the only
// thing that arrives from outside; the outage is something lookout works out
// for itself, which is why the store is read by the scheduler rather than by
// the endpoint.
package push

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
	"github.com/eeegoloauq/lookout/internal/state"
)

// MaxMsg is how much of a ping's text is kept. It ends up in the durable
// state file, on the page and in an alert, so it is bounded here rather than
// at each of them — the same reasoning, and the same number, as the failure
// reason the state machine keeps.
const MaxMsg = 200

// SaveEvery is how often a ping is worth a durable write. Two pings on top
// of each other say the same thing, and this endpoint is reachable from the
// network: one caller with a valid token must not be able to hold the whole
// process in fsync. Whatever this skips, the next tick persists.
const SaveEvery = time.Second

// Store is the last ping of every push check. One process owns it; the HTTP
// endpoint writes and the scheduler reads.
type Store struct {
	mu    sync.Mutex
	pings map[string]state.Ping
	dirty bool
}

// NewStore returns an empty store. Empty means "nothing has pinged yet",
// which is what a lost state file must degrade to: not up, and not an
// outage until the first deadline has passed.
func NewStore() *Store { return &Store{pings: map[string]state.Ping{}} }

// Restore loads the pings from a durable snapshot.
func (s *Store) Restore(pings map[string]state.Ping) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, p := range pings {
		s.pings[name] = p
	}
}

// Snapshot is the pings as they should be written out.
func (s *Store) Snapshot() map[string]state.Ping {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pings) == 0 {
		return nil
	}
	out := make(map[string]state.Ping, len(s.pings))
	for name, p := range s.pings {
		out[name] = p
	}
	return out
}

// Dirty reports whether a ping arrived or a deadline started since the last
// ClearDirty. Pings are written when they change, not on every tick.
func (s *Store) Dirty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dirty
}

// ClearDirty marks the current pings as persisted.
func (s *Store) ClearDirty() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirty = false
}

// Prune drops pings for checks that are no longer configured, for the same
// reason the state machine prunes: a renamed check must not inherit the
// heartbeat of the one it replaced.
func (s *Store) Prune(names []string) {
	keep := make(map[string]bool, len(names))
	for _, n := range names {
		keep[n] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.pings {
		if !keep[name] {
			delete(s.pings, name)
			s.dirty = true
		}
	}
}

// Last is the ping a check most recently received, for the status API.
func (s *Store) Last(name string) (state.Ping, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pings[name]
	if !ok || (p.At.IsZero() && p.Status == "") {
		return state.Ping{}, false
	}
	return p, ok
}

// Record stores one heartbeat and reports whether it is worth writing out
// now rather than on the next tick (see SaveEvery). The message is cleaned
// and truncated here so that every caller is bounded by construction, not by
// remembering to be.
func (s *Store) Record(name, status, msg string, at time.Time) bool {
	ping := state.Ping{At: at, Status: status, Msg: Clean(msg)}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.pings[name]
	ping.Since = prev.Since
	if ping.Since.IsZero() {
		ping.Since = at
	}
	s.pings[name] = ping
	s.dirty = true
	return prev.At.IsZero() || at.Sub(prev.At) >= SaveEvery
}

// Clean reduces a ping message to one bounded line. A ping is written by a
// shell script: it can carry a stack trace, a newline, or a megabyte of log.
func Clean(msg string) string {
	msg = strings.Join(strings.Fields(msg), " ")
	if utf8.RuneCountInString(msg) <= MaxMsg {
		return msg
	}
	// Cut on runes, not bytes: half a rune is a broken page and a broken
	// alert, and the text may well be somebody's non-ASCII error message.
	out := []rune(msg)
	return string(out[:MaxMsg-1]) + "…"
}

// Evaluate reports whether a check's heartbeat is on time. Like every probe
// it never returns an error: a deadline that has passed is a result, and
// "the ping is late" and "lookout is broken" must not share a code path.
//
// It is called from the scheduler's tick rather than when a ping arrives,
// because the event this check exists for is the ping that never came.
func (s *Store) Evaluate(c config.Check, now time.Time) check.Result {
	res := check.Result{Name: c.Name, At: now}
	ping := s.waitFrom(c.Name, now)
	since := ping.At
	if since.IsZero() {
		since = ping.Since
	}
	late := now.Sub(since)
	deadline := c.ExpectEvery + c.Grace

	switch {
	case late > deadline:
		// Silence outranks the last thing that was said. A job that
		// reported a failure and then stopped reporting at all is a
		// bigger problem than the failure it managed to send.
		res.Outcome = check.OutcomeDown
		res.Err = lateReason(ping, late, c.ExpectEvery)
	case ping.Failed():
		// The sender said it went wrong. That is a better answer than any
		// deadline, and it is the one thing a dead-man switch gets for
		// free from a job that is still alive enough to complain.
		res.Outcome = check.OutcomeDown
		res.Err = failureReason(ping)
	case ping.At.IsZero():
		// Configured, waiting for the first ping. Not up — nothing has
		// reported — and not down until the first deadline passes: a token
		// wired up this morning must not page before its cron has run.
		res.Outcome = check.OutcomeUnknown
	default:
		res.Outcome = check.OutcomeUp
	}
	return res
}

// waitFrom returns the check's ping, starting the clock on the first call
// for a check that has never pinged. Without that mark there is nothing to
// measure a first deadline from, and a check that has never been pinged
// would be excused forever — the quietest possible bug.
func (s *Store) waitFrom(name string, now time.Time) state.Ping {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pings[name]
	if !ok || p.Since.IsZero() {
		p.Since = now
		s.pings[name] = p
		s.dirty = true
	}
	return p
}

// The reason goes in Err rather than in a check.Failure: a Failure is a
// condition evaluated against a response, and a heartbeat that did not
// arrive left no response to evaluate.
func lateReason(ping state.Ping, late, expect time.Duration) string {
	if ping.At.IsZero() {
		return fmt.Sprintf("no ping yet, %s after lookout started waiting (expected every %s)", span(late), span(expect))
	}
	return fmt.Sprintf("no ping for %s (expected every %s)", span(late), span(expect))
}

func failureReason(ping state.Ping) string {
	if ping.Msg != "" {
		return ping.Msg
	}
	return "the last ping reported a failure"
}

// span renders a silence in the units the operator thinks in. A missed cron
// is minutes or hours late; nobody reads 847.3s as fourteen minutes.
func span(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
}
