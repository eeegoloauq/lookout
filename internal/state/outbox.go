package state

import (
	"sort"
	"time"
)

// DefaultOutboxLimit is how many undelivered items the queue will hold as
// individual events. Past that the oldest items are folded into a Summary
// rather than dropped: a full disk or a long Telegram outage must not become
// a silent hole in the alert history.
const DefaultOutboxLimit = 128

// Outbox is the durable queue of alerts that have been decided but not yet
// confirmed delivered. It is a value type stored inside Snapshot.
type Outbox struct {
	// NextID is the last id issued. It is persisted so a restart does not
	// reuse ids that may still be in a flush that raced the write.
	NextID int64        `json:"next_id,omitempty"`
	Items  []OutboxItem `json:"items,omitempty"`
	// Attempts and NextTry belong to the queue, not to each item: a batch
	// is sent as one message, so backoff is one decision.
	Attempts int       `json:"attempts,omitempty"`
	NextTry  time.Time `json:"next_try,omitzero"`

	// limit is the collapse threshold. Zero means DefaultOutboxLimit; tests
	// set a smaller one so overflow is exercised without 128 events.
	limit int `json:"-"`
}

// OutboxItem is one queued event.
type OutboxItem struct {
	ID       int64     `json:"id"`
	Enqueued time.Time `json:"enqueued"`
	Event    Event     `json:"event"`
}

// Summary is what overflow collapsed into. Counts are of original events,
// so folding a summary into a later summary does not under-count.
type Summary struct {
	Count   int            `json:"count"`
	From    time.Time      `json:"from,omitzero"`
	To      time.Time      `json:"to,omitzero"`
	ByKind  map[string]int `json:"by_kind,omitempty"`
	ByGroup map[string]int `json:"by_group,omitempty"`
	Checks  []string       `json:"checks,omitempty"`
}

// Limit is the collapse threshold in effect.
func (o Outbox) Limit() int {
	if o.limit > 0 {
		return o.limit
	}
	return DefaultOutboxLimit
}

// SetLimit sets the collapse threshold. It is for tests.
func (o *Outbox) SetLimit(n int) { o.limit = n }

// Enqueue appends an event. at is the event's own time so a restart in the
// middle of a batch window does not restart the wait from zero.
func (o *Outbox) Enqueue(ev Event, at time.Time) {
	o.NextID++
	if at.IsZero() {
		at = time.Now()
	}
	o.Items = append(o.Items, OutboxItem{ID: o.NextID, Enqueued: at, Event: ev})
	o.collapseIfNeeded()
}

// Remove deletes items by id. Unknown ids are ignored so a flush that raced
// a collapse cannot wipe the summary that replaced those items.
func (o *Outbox) Remove(ids []int64) {
	if len(ids) == 0 || len(o.Items) == 0 {
		return
	}
	drop := make(map[int64]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}
	kept := o.Items[:0]
	for _, it := range o.Items {
		if !drop[it.ID] {
			kept = append(kept, it)
		}
	}
	o.Items = kept
}

// Clone is a snapshot safe to persist or hand to another pipeline. Item
// slices are copied; maps inside a Summary are copied so a later collapse
// cannot mutate a snapshot already on its way to disk.
func (o Outbox) Clone() Outbox {
	out := Outbox{
		NextID:   o.NextID,
		Attempts: o.Attempts,
		NextTry:  o.NextTry,
		limit:    o.limit,
	}
	if len(o.Items) > 0 {
		out.Items = make([]OutboxItem, len(o.Items))
		copy(out.Items, o.Items)
		for i, it := range out.Items {
			if it.Event.Summary != nil {
				out.Items[i].Event.Summary = it.Event.Summary.clone()
			}
		}
	}
	return out
}

func (o *Outbox) collapseIfNeeded() {
	lim := o.Limit()
	for len(o.Items) > lim {
		keepN := lim / 2
		if keepN >= len(o.Items) {
			keepN = len(o.Items) - 1
		}
		dropN := len(o.Items) - keepN
		if dropN < 1 {
			dropN = len(o.Items)
			keepN = 0
		}
		dropped := o.Items[:dropN]
		kept := append([]OutboxItem(nil), o.Items[dropN:]...)
		sum := mergeSummary(dropped)
		o.NextID++
		summary := OutboxItem{
			ID:       o.NextID,
			Enqueued: dropped[0].Enqueued,
			Event: Event{
				Kind:    EventSummary,
				At:      dropped[len(dropped)-1].Enqueued,
				Alert:   true,
				Summary: sum,
			},
		}
		o.Items = append([]OutboxItem{summary}, kept...)
	}
}

func mergeSummary(items []OutboxItem) *Summary {
	s := &Summary{
		ByKind:  map[string]int{},
		ByGroup: map[string]int{},
	}
	checks := map[string]bool{}
	for _, it := range items {
		ev := it.Event
		if ev.Kind == EventSummary && ev.Summary != nil {
			s.add(ev.Summary)
			for _, name := range ev.Summary.Checks {
				checks[name] = true
			}
			continue
		}
		s.Count++
		s.ByKind[string(ev.Kind)]++
		group := ev.Group
		if group == "" {
			group = "other"
		}
		s.ByGroup[group]++
		if ev.Check != "" {
			checks[ev.Check] = true
		}
		enqueued := it.Enqueued
		if enqueued.IsZero() {
			enqueued = ev.At
		}
		if s.From.IsZero() || enqueued.Before(s.From) {
			s.From = enqueued
		}
		if enqueued.After(s.To) {
			s.To = enqueued
		}
	}
	s.Checks = sortedKeys(checks)
	return s
}

// Absorb records one event into a digest. Used by mute holds so a
// suppressed alert is counted rather than dropped.
func (s *Summary) Absorb(ev Event, at time.Time) {
	if s.ByKind == nil {
		s.ByKind = map[string]int{}
	}
	if s.ByGroup == nil {
		s.ByGroup = map[string]int{}
	}
	if ev.Kind == EventSummary || ev.Kind == EventHeld {
		if ev.Summary != nil {
			s.add(ev.Summary)
			for _, name := range ev.Summary.Checks {
				s.addCheck(name)
			}
		}
		return
	}
	s.Count++
	s.ByKind[string(ev.Kind)]++
	group := ev.Group
	if group == "" {
		group = "other"
	}
	s.ByGroup[group]++
	if ev.Check != "" {
		s.addCheck(ev.Check)
	}
	if at.IsZero() {
		at = ev.At
	}
	if s.From.IsZero() || (!at.IsZero() && at.Before(s.From)) {
		s.From = at
	}
	if at.After(s.To) {
		s.To = at
	}
}

// Clone is a deep copy, safe to persist or hand to another holder.
func (s *Summary) Clone() *Summary { return s.clone() }

// Merge folds another digest into s.
func (s *Summary) Merge(other *Summary) {
	if other == nil {
		return
	}
	s.add(other)
	for _, name := range other.Checks {
		s.addCheck(name)
	}
}

func (s *Summary) addCheck(name string) {
	for _, n := range s.Checks {
		if n == name {
			return
		}
	}
	s.Checks = append(s.Checks, name)
	sort.Strings(s.Checks)
}

func (s *Summary) add(other *Summary) {
	if other == nil {
		return
	}
	s.Count += other.Count
	if s.ByKind == nil {
		s.ByKind = map[string]int{}
	}
	if s.ByGroup == nil {
		s.ByGroup = map[string]int{}
	}
	for k, n := range other.ByKind {
		s.ByKind[k] += n
	}
	for k, n := range other.ByGroup {
		s.ByGroup[k] += n
	}
	if s.From.IsZero() || (!other.From.IsZero() && other.From.Before(s.From)) {
		s.From = other.From
	}
	if other.To.After(s.To) {
		s.To = other.To
	}
}

func (s *Summary) clone() *Summary {
	if s == nil {
		return nil
	}
	out := *s
	if s.ByKind != nil {
		out.ByKind = make(map[string]int, len(s.ByKind))
		for k, v := range s.ByKind {
			out.ByKind[k] = v
		}
	}
	if s.ByGroup != nil {
		out.ByGroup = make(map[string]int, len(s.ByGroup))
		for k, v := range s.ByGroup {
			out.ByGroup[k] = v
		}
	}
	if s.Checks != nil {
		out.Checks = append([]string(nil), s.Checks...)
	}
	return &out
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
