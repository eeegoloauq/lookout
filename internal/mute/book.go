package mute

import (
	"fmt"
	"sync"
	"time"

	"github.com/eeegoloauq/lookout/internal/config"
	"github.com/eeegoloauq/lookout/internal/state"
)

// Book is the durable set of mute holds. It is safe for concurrent use.
type Book struct {
	mu     sync.Mutex
	holds  []state.Hold
	nextID int64
	dirty  bool
	windows []config.MuteWindow
}

// NewBook returns an empty book with the static windows from the config.
func NewBook(windows []config.MuteWindow) *Book {
	return &Book{windows: windows}
}

// Restore replaces in-memory holds with a snapshot from disk.
func (b *Book) Restore(holds []state.Hold) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.holds = cloneHolds(holds)
	for _, h := range b.holds {
		if n := holdSeq(h.ID); n > b.nextID {
			b.nextID = n
		}
	}
	b.dirty = false
}

// Snapshot is a copy safe to persist.
func (b *Book) Snapshot() []state.Hold {
	b.mu.Lock()
	defer b.mu.Unlock()
	return cloneHolds(b.holds)
}

func (b *Book) Dirty() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dirty
}

func (b *Book) ClearDirty() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dirty = false
}

// Mute starts an ad-hoc hold. until is now+for. An existing ad-hoc hold
// with the same group and check is replaced so repeating `lookout mute`
// extends rather than stacks.
func (b *Book) Mute(now time.Time, d time.Duration, group, check string) (state.Hold, error) {
	if d <= 0 {
		return state.Hold{}, fmt.Errorf("mute duration must be positive")
	}
	if d > config.MaxAdhocMute {
		return state.Hold{}, fmt.Errorf("mute duration %s is longer than the %s maximum (a forgotten mute is how an outage stays silent)", d, config.MaxAdhocMute)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	until := now.Add(d)
	kept := b.holds[:0]
	var transferred *state.Summary
	for _, h := range b.holds {
		if h.Source == state.HoldAdhoc && h.Group == group && h.Check == check {
			transferred = h.Suppressed
			continue
		}
		kept = append(kept, h)
	}
	b.holds = kept
	b.nextID++
	h := state.Hold{
		ID:         fmt.Sprintf("adhoc-%d", b.nextID),
		Until:      until,
		Group:      group,
		Check:      check,
		Source:     state.HoldAdhoc,
		Created:    now,
		Suppressed: transferred,
	}
	b.holds = append(b.holds, h)
	b.dirty = true
	return h, nil
}

// Unmute drops matching ad-hoc holds and returns EventHeld for any
// digest they accumulated. group/check empty means "all". Scheduled
// windows cannot be unmuted: they live in the config.
func (b *Book) Unmute(now time.Time, group, check string) []state.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var events []state.Event
	kept := b.holds[:0]
	for _, h := range b.holds {
		if h.Source != state.HoldAdhoc {
			kept = append(kept, h)
			continue
		}
		if group != "" && h.Group != group {
			kept = append(kept, h)
			continue
		}
		if check != "" && h.Check != check {
			kept = append(kept, h)
			continue
		}
		if ev, ok := b.releaseLocked(h, now); ok {
			events = append(events, ev)
		}
		b.dirty = true
	}
	b.holds = kept
	return events
}

// Catch records an event if a mute covers it. Returns true when the
// event must not be delivered.
func (b *Book) Catch(ev state.Event, now time.Time) bool {
	if ev.Kind == state.EventHeld {
		// A digest is the lift of a mute; another overlapping mute
		// may still catch it via releaseLocked's transfer.
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.catchLocked(ev.Group, ev.Check, now)
	if h == nil {
		return false
	}
	if h.Suppressed == nil {
		h.Suppressed = &state.Summary{}
	}
	at := ev.At
	if at.IsZero() {
		at = now
	}
	h.Suppressed.Absorb(ev, at)
	b.dirty = true
	return true
}

func (b *Book) catchLocked(group, check string, now time.Time) *state.Hold {
	// Create any missing schedule holds first so a later append cannot
	// invalidate a pointer we already picked.
	for i, w := range b.windows {
		if !Covers(w, group, check) {
			continue
		}
		end, ok := Active(w, now)
		if !ok {
			continue
		}
		b.ensureScheduleLocked(i, w, end, now)
	}
	best := -1
	for i := range b.holds {
		h := b.holds[i]
		if !h.Until.After(now) {
			continue
		}
		if !holdCovers(h, group, check) {
			continue
		}
		if best < 0 || h.Until.After(b.holds[best].Until) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	return &b.holds[best]
}

func (b *Book) ensureScheduleLocked(i int, w config.MuteWindow, end, now time.Time) *state.Hold {
	id := fmt.Sprintf("schedule-%d-%s", i, end.UTC().Format(time.RFC3339))
	for j := range b.holds {
		if b.holds[j].ID == id {
			return &b.holds[j]
		}
	}
	h := state.Hold{
		ID:      id,
		Until:   end,
		Group:   w.Group,
		Check:   w.Check,
		Source:  state.HoldSchedule,
		Created: now,
	}
	b.holds = append(b.holds, h)
	b.dirty = true
	return &b.holds[len(b.holds)-1]
}

// Expire drops holds whose Until has passed and returns their digests.
func (b *Book) Expire(now time.Time) []state.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var events []state.Event
	kept := b.holds[:0]
	for _, h := range b.holds {
		if h.Until.After(now) {
			kept = append(kept, h)
			continue
		}
		if ev, ok := b.releaseLocked(h, now); ok {
			events = append(events, ev)
		}
		b.dirty = true
	}
	b.holds = kept
	return events
}

// releaseLocked folds a lifting hold into a still-covering mute if
// there is one, otherwise turns its digest into EventHeld. Caller
// holds b.mu and has already removed (or is about to remove) h.
func (b *Book) releaseLocked(h state.Hold, now time.Time) (state.Event, bool) {
	if h.Suppressed == nil || h.Suppressed.Count == 0 {
		return state.Event{}, false
	}
	if other := b.catchLocked(h.Group, h.Check, now); other != nil && other.ID != h.ID {
		if other.Suppressed == nil {
			other.Suppressed = &state.Summary{}
		}
		other.Suppressed.Merge(h.Suppressed)
		b.dirty = true
		return state.Event{}, false
	}
	return state.Event{
		Kind:    state.EventHeld,
		Check:   h.Check,
		Group:   h.Group,
		At:      now,
		Alert:   true,
		Summary: h.Suppressed.Clone(),
	}, true
}

// Muted reports whether a check is currently quiet.
func (b *Book) Muted(group, check string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.catchLocked(group, check, now) != nil
}

// Views is the currently active mutes, for the status page and API.
func (b *Book) Views(now time.Time) []View {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []View
	seen := map[string]bool{}
	for _, h := range b.holds {
		if !h.Until.After(now) {
			continue
		}
		out = append(out, View{
			Until:  h.Until,
			Group:  h.Group,
			Check:  h.Check,
			Source: h.Source,
			Held:   summaryCount(h.Suppressed),
		})
		seen[h.ID] = true
	}
	for i, w := range b.windows {
		end, ok := Active(w, now)
		if !ok {
			continue
		}
		id := fmt.Sprintf("schedule-%d-%s", i, end.UTC().Format(time.RFC3339))
		if seen[id] {
			continue
		}
		out = append(out, View{
			Until:  end,
			Group:  w.Group,
			Check:  w.Check,
			Source: state.HoldSchedule,
		})
	}
	return out
}

// NextDeadline is when the book next needs to run Expire, or a
// scheduled window starts.
func (b *Book) NextDeadline(now time.Time) (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var next time.Time
	take := func(t time.Time) {
		if t.After(now) && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	for _, h := range b.holds {
		take(h.Until)
	}
	for _, w := range b.windows {
		if t, ok := NextBoundary(w, now); ok {
			take(t)
		}
	}
	if next.IsZero() {
		return time.Time{}, false
	}
	return next, true
}

// View is one active mute as a foreign consumer should see it.
type View struct {
	Until  time.Time
	Group  string
	Check  string
	Source string
	Held   int
}

func holdCovers(h state.Hold, group, check string) bool {
	if h.Group != "" && h.Group != group {
		return false
	}
	if h.Check != "" && h.Check != check {
		return false
	}
	return true
}

func summaryCount(s *state.Summary) int {
	if s == nil {
		return 0
	}
	return s.Count
}

func cloneHolds(in []state.Hold) []state.Hold {
	if len(in) == 0 {
		return nil
	}
	out := make([]state.Hold, len(in))
	copy(out, in)
	for i, h := range out {
		if h.Suppressed != nil {
			out[i].Suppressed = h.Suppressed.Clone()
		}
	}
	return out
}

func holdSeq(id string) int64 {
	var n int64
	_, _ = fmt.Sscanf(id, "adhoc-%d", &n)
	return n
}
