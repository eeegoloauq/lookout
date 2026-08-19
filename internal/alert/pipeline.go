package alert

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/eeegoloauq/lookout/internal/state"
)

const (
	backoffBase = time.Second
	backoffCap  = 5 * time.Minute
	// shutdownFlushBudget is how long a dying process spends trying to
	// push whatever is already queued. The items stay on disk either way;
	// this is a courtesy so a planned stop does not wait for the next start.
	shutdownFlushBudget = 5 * time.Second
)

// Pipeline is the batching window and retry worker in front of a Notifier.
//
// The first queued event opens a window of BatchWindow; everything that
// arrives during that window is sent as one message. A failed send keeps
// every item in the outbox and retries with exponential backoff. The
// outbox itself is a value the caller persists — this type only mutates
// it and says when it has changed.
type Pipeline struct {
	mu       sync.Mutex
	outbox   state.Outbox
	dirty    bool
	notifier Notifier
	window   time.Duration
	log      *slog.Logger
	persist  func()
	wake     chan struct{}
}

// NewPipeline returns a pipeline that has not been restored yet.
func NewPipeline(n Notifier, window time.Duration, log *slog.Logger) *Pipeline {
	if window <= 0 {
		window = 45 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Pipeline{
		notifier: n,
		window:   window,
		log:      log,
		wake:     make(chan struct{}, 1),
	}
}

// SetPersist registers the function called after a flush changes the
// outbox. The callback must not run under Pipeline's lock (Snapshot
// takes that lock) and is typically monitor.save.
func (p *Pipeline) SetPersist(fn func()) { p.persist = fn }

// Restore replaces the in-memory queue with a snapshot from disk.
func (p *Pipeline) Restore(o state.Outbox) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outbox = o
	p.dirty = false
}

// Snapshot returns a copy of the queue, safe to write to disk.
func (p *Pipeline) Snapshot() state.Outbox {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outbox.Clone()
}

// Dirty reports whether the queue changed since the last ClearDirty.
func (p *Pipeline) Dirty() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dirty
}

// ClearDirty marks the current queue as persisted.
func (p *Pipeline) ClearDirty() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dirty = false
}

// Enqueue records an event. It is a no-op for events that the check asked
// not to alert on: those still exist as state changes, they just must not
// page anyone.
func (p *Pipeline) Enqueue(ev state.Event) {
	if !ev.Alert {
		return
	}
	p.mu.Lock()
	p.outbox.Enqueue(ev, ev.At)
	p.dirty = true
	p.mu.Unlock()
	p.nudge()
}

// Items is a copy of the queued items, for tests.
func (p *Pipeline) Items() []state.OutboxItem {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outbox.Clone().Items
}

// Run delivers queued events until ctx is cancelled, then tries one last
// flush so a planned stop does not wait for the next start.
func (p *Pipeline) Run(ctx context.Context) {
	defer p.shutdownFlush()
	for {
		var timer *time.Timer
		var timerC <-chan time.Time
		deadline, ok := p.deadline()
		if ok {
			delay := time.Until(deadline)
			if delay < 0 {
				delay = 0
			}
			timer = time.NewTimer(delay)
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return
		case <-p.wake:
			stopTimer(timer)
		case <-timerC:
			p.flush(ctx, false)
			p.callPersist()
		}
	}
}

func (p *Pipeline) shutdownFlush() {
	if p.notifier == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownFlushBudget)
	defer cancel()
	p.flush(ctx, true)
	p.callPersist()
}

func (p *Pipeline) callPersist() {
	if p.persist == nil || !p.Dirty() {
		return
	}
	p.persist()
}

func (p *Pipeline) nudge() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Pipeline) deadline() (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.outbox.Items) == 0 || p.notifier == nil {
		return time.Time{}, false
	}
	ready := p.outbox.Items[0].Enqueued.Add(p.window)
	if !p.outbox.NextTry.IsZero() && p.outbox.NextTry.After(ready) {
		ready = p.outbox.NextTry
	}
	return ready, true
}

// flush sends every currently queued item as one message. force skips the
// batch window (used on shutdown). Items are removed only after Notify
// returns nil.
func (p *Pipeline) flush(ctx context.Context, force bool) {
	p.mu.Lock()
	if p.notifier == nil || len(p.outbox.Items) == 0 {
		p.mu.Unlock()
		return
	}
	now := time.Now()
	if !force {
		if !p.outbox.NextTry.IsZero() && now.Before(p.outbox.NextTry) {
			p.mu.Unlock()
			return
		}
		if now.Before(p.outbox.Items[0].Enqueued.Add(p.window)) {
			p.mu.Unlock()
			return
		}
	}
	items := append([]state.OutboxItem(nil), p.outbox.Items...)
	p.mu.Unlock()

	events := make([]state.Event, len(items))
	ids := make([]int64, len(items))
	for i, it := range items {
		events[i] = it.Event
		ids[i] = it.ID
	}
	text := Format(events)
	err := p.notifier.Notify(ctx, text)

	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.outbox.Attempts++
		p.outbox.NextTry = time.Now().Add(backoff(p.outbox.Attempts))
		p.dirty = true
		p.log.Warn("alert delivery failed",
			"err", err,
			"pending", len(p.outbox.Items),
			"attempt", p.outbox.Attempts,
			"next", p.outbox.NextTry.UTC().Format(time.RFC3339))
		return
	}
	p.outbox.Remove(ids)
	p.outbox.Attempts = 0
	p.outbox.NextTry = time.Time{}
	p.dirty = true
	p.log.Info("alert sent", "events", len(ids), "pending", len(p.outbox.Items))
}

func stopTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	// 1s, 2s, 4s, ... capped. A shift that would overflow the duration
	// is treated as the cap: a crashed clock must not wait 200 years.
	shift := attempts - 1
	if shift > 10 {
		return backoffCap
	}
	d := backoffBase << shift
	if d > backoffCap || d <= 0 {
		return backoffCap
	}
	return d
}
