// Package monitor runs the configured checks on schedule and folds their
// results into state and history.
package monitor

import (
	"context"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/eeegoloauq/lookout/internal/alert"
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

// registryView is implemented by the production probe set. Tests that
// inject a fake prober simply don't persist a registry cache.
type registryView interface {
	LoadRegistry(state.RegistryCache)
	Registry() state.RegistryCache
	RegistryDirty() bool
	ClearRegistryDirty()
}

// Monitor owns the scheduling loop and the state that results feed into.
type Monitor struct {
	cfg      *config.Config
	prober   Prober
	store    *state.Store
	machine  *state.Machine
	hist     *history.History
	log      *slog.Logger
	onEvent  func(state.Event)
	notifier alert.Notifier
	pipeline *alert.Pipeline
	// loadedOutbox is the queue as it was on disk, kept so a process that
	// is not delivering (no notifier) cannot wipe someone else's pending
	// alerts the next time it saves check state.
	loadedOutbox state.Outbox

	sem         chan struct{}
	saveMu      sync.Mutex
	restoreOnce sync.Once
}

// Option configures a Monitor.
type Option func(*Monitor)

// WithLogger sets the logger. The default discards nothing and writes nothing:
// callers are expected to pass one.
func WithLogger(l *slog.Logger) Option { return func(m *Monitor) { m.log = l } }

// WithEventFunc installs a sink for state changes. Tests use this to
// observe events without going through the outbox; production leaves it
// unset so events are logged and, if a notifier is configured, queued.
func WithEventFunc(f func(state.Event)) Option { return func(m *Monitor) { m.onEvent = f } }

// WithNotifier enables durable delivery. Events with Alert=true are
// written to the outbox and drained by the pipeline; without a notifier
// they are only logged.
func WithNotifier(n alert.Notifier) Option { return func(m *Monitor) { m.notifier = n } }

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
	// Rings exist before Run so the status API can answer immediately
	// with "no samples" rather than pretending a check is missing.
	for _, c := range cfg.Checks {
		m.hist.Track(c.Name, c.Interval)
	}
	if m.notifier != nil {
		m.pipeline = alert.NewPipeline(m.notifier, m.cfg.Alerting.BatchWindow, m.log)
		m.pipeline.SetPersist(m.save)
	}
	return m
}

// Config is the resolved configuration this monitor is running.
func (m *Monitor) Config() *config.Config { return m.cfg }

// Outbox is a snapshot of the undelivered alert queue. /healthz uses it
// to decide whether lookout can still call for help.
func (m *Monitor) Outbox() state.Outbox {
	if m.pipeline != nil {
		return m.pipeline.Snapshot()
	}
	return m.loadedOutbox.Clone()
}

// Machine exposes the state machine, for the API and for tests.
func (m *Monitor) Machine() *state.Machine { return m.machine }

// History exposes the recent history, for the API and for tests.
func (m *Monitor) History() *history.History { return m.hist }

// Run probes every check until ctx is cancelled, then persists the final state.
// It returns nil on a clean shutdown: a cancelled context is how lookout stops.
func (m *Monitor) Run(ctx context.Context) error {
	m.Restore()

	start := time.Now()
	var wg sync.WaitGroup
	if m.pipeline != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.pipeline.Run(ctx)
		}()
	}
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

// Restore loads durable state and drops entries for checks that left
// the configuration. It is safe to call more than once: the HTTP server
// wants state loaded before the first request, and Run wants the same
// load before the first probe; doing it twice would be a race, not a
// refresh.
//
// Every failure mode here is survivable, and none of them may stop the
// monitor from starting: a monitor that refuses to start is a monitor
// that is silent, which is the failure this project exists to fix.
func (m *Monitor) Restore() {
	m.restoreOnce.Do(m.restore)
}

func (m *Monitor) restore() {
	snap, err := m.store.Load()
	if err != nil {
		m.log.Warn("starting with empty state", "path", m.store.Path(), "err", err)
	}
	m.machine.Restore(snap)
	m.loadedOutbox = snap.Outbox
	if m.pipeline != nil {
		m.pipeline.Restore(snap.Outbox)
	}
	if rv, ok := m.prober.(registryView); ok {
		rv.LoadRegistry(snap.Registry)
	}
	names := make([]string, 0, len(m.cfg.Checks))
	for _, c := range m.cfg.Checks {
		names = append(names, c.Name)
	}
	m.machine.Prune(names)
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
	if m.machine.Dirty() || (m.pipeline != nil && m.pipeline.Dirty()) || registryDirty(m.prober) {
		m.save()
	}
}

func registryDirty(p Prober) bool {
	rv, ok := p.(registryView)
	return ok && rv.RegistryDirty()
}

func (m *Monitor) emit(ev state.Event) {
	if m.pipeline != nil && ev.Alert {
		m.pipeline.Enqueue(ev)
	}
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
		// A response we can no longer read is a changed API, not an outage.
		// It reaches the threshold the same way but must not read the same.
		if ev.Result.Outcome == check.OutcomeMalformed {
			m.log.Warn("check response no longer matches", attrs...)
			return
		}
		m.log.Warn("check is down", attrs...)
	case state.EventUp:
		attrs = append(attrs, "downtime", ev.Downtime)
		m.log.Info("check recovered", attrs...)
	case state.EventUnstable:
		attrs = append(attrs, "failures", ev.Failures, "window", ev.Window, "reason", ev.Result.Reason())
		m.log.Warn("check is unstable", attrs...)
	case state.EventDrift:
		m.log.Warn("dns zone changed", attrs...)
	case state.EventExpiry:
		kind, days := "", 0
		if ev.Expiry != nil {
			kind, days = ev.Expiry.Kind, ev.Expiry.DaysLeft
		}
		attrs = append(attrs, "kind", kind, "days", days)
		m.log.Warn("expiry notice", attrs...)
	case state.EventStale:
		attrs = append(attrs, "silent_for", ev.StaleFor)
		m.log.Warn("registry lookup stale", attrs...)
	}
}

// save writes durable state. Clearing the dirty flag before taking the snapshot
// means a change racing with this write is either included here or leaves the
// flag set for the next one — never dropped.
func (m *Monitor) save() {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	machineDirty := m.machine.Dirty()
	outboxDirty := m.pipeline != nil && m.pipeline.Dirty()
	regDirty := registryDirty(m.prober)
	if !machineDirty && !outboxDirty && !regDirty {
		return
	}
	m.machine.ClearDirty()
	if m.pipeline != nil {
		m.pipeline.ClearDirty()
	}
	if rv, ok := m.prober.(registryView); ok {
		rv.ClearRegistryDirty()
	}
	snap := m.machine.Snapshot()
	if rv, ok := m.prober.(registryView); ok {
		snap.Registry = rv.Registry()
	}
	if m.pipeline != nil {
		snap.Outbox = m.pipeline.Snapshot()
	} else {
		// A process that is not delivering must leave the on-disk queue
		// alone: overwriting it with empty would drop someone else's
		// undelivered alerts.
		snap.Outbox = m.loadedOutbox
	}
	if err := m.store.Save(snap); err != nil {
		// Losing the write is survivable — it costs incident continuity
		// across a restart — but an unwritten outbox is a missed alert, so
		// this is the one save failure that is not "just history".
		m.log.Error("could not write state", "path", m.store.Path(), "err", err)
	}
}
