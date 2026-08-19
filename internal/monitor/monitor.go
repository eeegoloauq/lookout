// Package monitor runs the configured checks on schedule and folds their
// results into state and history.
package monitor

import (
	"context"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
	"github.com/eeegoloauq/lookout/internal/history"
	"github.com/eeegoloauq/lookout/internal/state"
)

// maxConcurrentProbes bounds how many probes may be in flight at once. A
// network stall must not leave every check's goroutine waiting on a socket at
// the same time.
const maxConcurrentProbes = 8

// Prober executes one check.
type Prober interface {
	Probe(ctx context.Context, c config.Check) check.Result
}

// Monitor owns the scheduling loop and the state that results feed into.
type Monitor struct {
	cfg     *config.Config
	prober  Prober
	store   *state.Store
	machine *state.Machine
	hist    *history.History
	log     *slog.Logger
	onEvent func(state.Event)

	sem    chan struct{}
	saveMu sync.Mutex
}

// Option configures a Monitor.
type Option func(*Monitor)

// WithLogger sets the logger. The default discards nothing and writes nothing:
// callers are expected to pass one.
func WithLogger(l *slog.Logger) Option { return func(m *Monitor) { m.log = l } }

// WithEventFunc installs a sink for state changes. Delivering them to a
// notifier is the next release's job; until then they are logged.
func WithEventFunc(f func(state.Event)) Option { return func(m *Monitor) { m.onEvent = f } }

// New wires a monitor. It does not touch the disk or the network yet.
func New(cfg *config.Config, prober Prober, opts ...Option) *Monitor {
	m := &Monitor{
		cfg:     cfg,
		prober:  prober,
		store:   state.NewStore(cfg.StateFile),
		machine: state.NewMachine(),
		hist:    history.New(),
		log:     slog.Default(),
		sem:     make(chan struct{}, maxConcurrentProbes),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Machine exposes the state machine, for the API and for tests.
func (m *Monitor) Machine() *state.Machine { return m.machine }

// History exposes the recent history, for the API and for tests.
func (m *Monitor) History() *history.History { return m.hist }

// Run probes every check until ctx is cancelled, then persists the final state.
// It returns nil on a clean shutdown: a cancelled context is how lookout stops.
func (m *Monitor) Run(ctx context.Context) error {
	m.restore()

	names := make([]string, 0, len(m.cfg.Checks))
	for _, c := range m.cfg.Checks {
		names = append(names, c.Name)
		m.hist.Track(c.Name, c.Interval)
	}
	m.machine.Prune(names)

	start := time.Now()
	var wg sync.WaitGroup
	for _, c := range m.cfg.Checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.loop(ctx, c, start)
		}()
	}
	wg.Wait()

	// The last results are worth as much as the first: persist before exiting.
	m.save()
	return nil
}

// restore loads durable state. Every failure mode here is survivable, and none
// of them may stop the monitor from starting: a monitor that refuses to start
// is a monitor that is silent, which is the failure this project exists to fix.
func (m *Monitor) restore() {
	snap, err := m.store.Load()
	if err != nil {
		m.log.Warn("starting with empty state", "path", m.store.Path(), "err", err)
	}
	m.machine.Restore(snap)
}

// loop probes one check on its own schedule.
//
// Ticks are computed from a fixed origin rather than by sleeping after each
// probe, so a slow target cannot make a check drift: the period stays the
// interval instead of becoming interval plus probe duration.
func (m *Monitor) loop(ctx context.Context, c config.Check, start time.Time) {
	// The first probe runs immediately — at startup, knowing the current state
	// matters more than spreading load. Subsequent ticks are offset by a hash
	// of the check name, which spreads the herd and, unlike random jitter,
	// spreads it the same way after every restart.
	m.probe(ctx, c)

	next := start.Add(phase(c.Name, c.Interval))
	for !next.After(time.Now()) {
		next = next.Add(c.Interval)
	}

	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		m.probe(ctx, c)
		// Skip ticks missed while the process was stalled instead of firing a
		// burst of catch-up probes.
		for !next.After(time.Now()) {
			next = next.Add(c.Interval)
		}
		timer.Reset(time.Until(next))
	}
}

// phase spreads checks across their interval by hashing the check name.
func phase(name string, interval time.Duration) time.Duration {
	h := fnv.New32a()
	h.Write([]byte(name))
	return time.Duration(uint64(h.Sum32()) % uint64(interval))
}

func (m *Monitor) probe(ctx context.Context, c config.Check) {
	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	res := m.prober.Probe(ctx, c)
	<-m.sem

	if ctx.Err() != nil {
		// A probe aborted by shutdown says nothing about the target.
		return
	}

	m.hist.Record(res)
	m.log.Debug("probe",
		"check", c.Name,
		"outcome", string(res.Outcome),
		"status", res.StatusCode,
		"duration", res.Duration,
		"reason", res.Reason())

	for _, ev := range m.machine.Observe(c, res) {
		m.emit(ev)
	}
	if m.machine.Dirty() {
		m.save()
	}
}

func (m *Monitor) emit(ev state.Event) {
	if m.onEvent != nil {
		m.onEvent(ev)
		return
	}
	m.logEvent(ev)
}

func (m *Monitor) logEvent(ev state.Event) {
	attrs := []any{"check", ev.Check, "group", ev.Group, "alert", ev.Alert}
	switch ev.Kind {
	case state.EventDown:
		attrs = append(attrs, "status", ev.Result.StatusCode, "reason", ev.Result.Reason())
		m.log.Warn("check is down", attrs...)
	case state.EventUp:
		attrs = append(attrs, "downtime", ev.Downtime)
		m.log.Info("check recovered", attrs...)
	case state.EventUnstable:
		attrs = append(attrs, "failures", ev.Failures, "window", ev.Window, "reason", ev.Result.Reason())
		m.log.Warn("check is unstable", attrs...)
	}
}

// save writes durable state. Clearing the dirty flag before taking the snapshot
// means a change racing with this write is either included here or leaves the
// flag set for the next one — never dropped.
func (m *Monitor) save() {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	if !m.machine.Dirty() {
		return
	}
	m.machine.ClearDirty()
	if err := m.store.Save(m.machine.Snapshot()); err != nil {
		// Losing the write is survivable — it costs incident continuity across
		// a restart, not a missed alert — so it is logged, not fatal.
		m.log.Error("could not write state", "path", m.store.Path(), "err", err)
	}
}
