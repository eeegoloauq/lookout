// Package monitor runs the configured checks on schedule and folds their
// results into state and history.
package monitor

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/eeegoloauq/lookout/internal/alert"
	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
	"github.com/eeegoloauq/lookout/internal/history"
	"github.com/eeegoloauq/lookout/internal/mute"
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

	book      *mute.Book
	histLog   *history.Log
	samples   *history.Samples
	days      map[string]state.DayAcc
	daysMu    sync.Mutex
	daysDirty bool
	wakeHolds chan struct{}

	beatMu     sync.Mutex
	lastBeat   time.Time
	closedBeat int
	beatDirty  bool

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
	if cfg.HistoryFile == "" {
		dir := filepath.Dir(cfg.StateFile)
		if dir == "." || dir == "" {
			cfg.HistoryFile = "history.jsonl"
		} else {
			cfg.HistoryFile = filepath.Join(dir, "history.jsonl")
		}
	}
	if cfg.SamplesFile == "" {
		dir := filepath.Dir(cfg.StateFile)
		if dir == "." || dir == "" {
			cfg.SamplesFile = "samples.jsonl"
		} else {
			cfg.SamplesFile = filepath.Join(dir, "samples.jsonl")
		}
	}
	m := &Monitor{
		cfg:       cfg,
		prober:    prober,
		store:     state.NewStore(cfg.StateFile),
		machine:   state.NewMachine(),
		hist:      history.New(),
		histLog:   history.NewLog(cfg.HistoryFile),
		samples:   history.NewSamples(cfg.SamplesFile),
		book:      mute.NewBook(cfg.Mute),
		days:      map[string]state.DayAcc{},
		wakeHolds: make(chan struct{}, 1),
		log:       slog.Default(),
		sem:       make(chan struct{}, maxConcurrentProbes),
	}
	for _, opt := range opts {
		opt(m)
	}
	m.machine.SetReminders(cfg.Alerting.Reminders)
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
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.watchHolds(ctx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.watchDays(ctx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.watchSamples(ctx)
	}()
	if m.pipeline != nil && m.cfg.Alerting.Heartbeat > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.watchHeartbeat(ctx)
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
	if err := m.samples.Close(); err != nil {
		m.log.Error("could not flush samples", "path", m.samples.Path(), "err", err)
	}
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
	m.book.Restore(snap.Holds)
	m.daysMu.Lock()
	m.days = snap.Days
	if m.days == nil {
		m.days = map[string]state.DayAcc{}
	}
	m.daysMu.Unlock()
	m.beatMu.Lock()
	m.lastBeat = snap.LastHeartbeat
	m.closedBeat = snap.ClosedSinceHeartbeat
	m.beatMu.Unlock()
	if err := m.histLog.Load(); err != nil {
		m.log.Warn("starting with empty history file", "path", m.histLog.Path(), "err", err)
	}
	recs, err := m.samples.Load(time.Now())
	if err != nil {
		m.log.Warn("starting with empty samples file", "path", m.samples.Path(), "err", err)
	}
	for _, rec := range recs {
		m.hist.Record(rec)
	}
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
	// A mute that expired while we were down, or a UTC day that crossed
	// midnight, must be resolved before the first request or probe.
	for _, ev := range m.book.Expire(time.Now()) {
		m.emit(ev)
	}
	m.rollDays(time.Now())
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
	if err := m.samples.Append(res); err != nil {
		m.log.Error("could not append sample", "path", m.samples.Path(), "err", err)
	}
	m.log.Debug("probe",
		"check", c.Name,
		"outcome", string(res.Outcome),
		"status", res.StatusCode,
		"duration", res.Duration,
		"reason", res.Reason())

	events := m.machine.Observe(c, res)
	incidents := 0
	for _, ev := range events {
		if ev.Kind == state.EventDown {
			incidents++
		}
		if ev.Kind == state.EventUp {
			m.noteClosed()
		}
		m.emit(ev)
	}
	m.recordDay(c, res, incidents)
	if m.machine.Dirty() || (m.pipeline != nil && m.pipeline.Dirty()) || registryDirty(m.prober) || m.book.Dirty() || m.daysAreDirty() || m.heartbeatDirty() {
		m.save()
	}
}

func registryDirty(p Prober) bool {
	rv, ok := p.(registryView)
	return ok && rv.RegistryDirty()
}

func (m *Monitor) emit(ev state.Event) {
	if m.pipeline != nil && ev.Alert {
		if m.book.Catch(ev, time.Now()) {
			m.log.Info("alert held while muted",
				"check", ev.Check, "group", ev.Group, "kind", string(ev.Kind))
		} else {
			m.pipeline.Enqueue(ev)
		}
	} else if ev.Alert && m.book.Catch(ev, time.Now()) {
		// mode: none still records the digest so a later process
		// with a notifier does not inherit a silent hole.
		m.log.Info("alert held while muted",
			"check", ev.Check, "group", ev.Group, "kind", string(ev.Kind))
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
	case state.EventStillDown:
		attrs = append(attrs, "downtime", ev.Downtime, "reason", ev.Result.Reason())
		m.log.Warn("check is still down", attrs...)
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
	case state.EventUndelegated:
		m.log.Error("domain is no longer delegated", attrs...)
	case state.EventDelegated:
		m.log.Info("domain is delegated again", attrs...)
	case state.EventHeld:
		n := 0
		if ev.Summary != nil {
			n = ev.Summary.Count
		}
		attrs = append(attrs, "held", n)
		m.log.Info("mute ended", attrs...)
	case state.EventHeartbeat:
		checks, down, closed := 0, 0, 0
		if ev.Heartbeat != nil {
			checks, down, closed = ev.Heartbeat.Checks, ev.Heartbeat.Down, ev.Heartbeat.Closed
		}
		m.log.Info("lookout is alive", "checks", checks, "down", down, "closed", closed)
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
	holdsDirty := m.book.Dirty()
	daysDirty := m.daysAreDirty()
	beatDirty := m.heartbeatDirty()
	if !machineDirty && !outboxDirty && !regDirty && !holdsDirty && !daysDirty && !beatDirty {
		return
	}
	m.machine.ClearDirty()
	if m.pipeline != nil {
		m.pipeline.ClearDirty()
	}
	m.book.ClearDirty()
	m.clearDaysDirty()
	m.clearHeartbeatDirty()
	if rv, ok := m.prober.(registryView); ok {
		rv.ClearRegistryDirty()
	}
	snap := m.machine.Snapshot()
	if rv, ok := m.prober.(registryView); ok {
		snap.Registry = rv.Registry()
	}
	snap.Holds = m.book.Snapshot()
	m.daysMu.Lock()
	snap.Days = cloneDays(m.days)
	m.daysMu.Unlock()
	m.beatMu.Lock()
	snap.LastHeartbeat = m.lastBeat
	snap.ClosedSinceHeartbeat = m.closedBeat
	m.beatMu.Unlock()
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

// Mute starts an ad-hoc quiet window. Probes keep running.
func (m *Monitor) Mute(d time.Duration, group, check string) (state.Hold, error) {
	if check != "" {
		if _, ok := checkByName(m.cfg, check); !ok {
			return state.Hold{}, fmt.Errorf("no check named %q", check)
		}
	}
	if group != "" && !groupExists(m.cfg, group) {
		return state.Hold{}, fmt.Errorf("no group named %q", group)
	}
	h, err := m.book.Mute(time.Now(), d, group, check)
	if err != nil {
		return state.Hold{}, err
	}
	m.save()
	m.nudgeHolds()
	m.log.Info("muted", "for", d, "until", h.Until.UTC().Format(time.RFC3339), "group", group, "check", check)
	return h, nil
}

// Unmute lifts matching ad-hoc holds and delivers their digest.
func (m *Monitor) Unmute(group, check string) int {
	events := m.book.Unmute(time.Now(), group, check)
	for _, ev := range events {
		m.emit(ev)
	}
	m.save()
	m.nudgeHolds()
	m.log.Info("unmuted", "group", group, "check", check, "digests", len(events))
	return len(events)
}

// Mutes is the currently active quiet windows, for the status page.
func (m *Monitor) Mutes(now time.Time) []mute.View {
	return m.book.Views(now)
}

// CheckMuted reports whether a check is in a quiet window right now.
func (m *Monitor) CheckMuted(group, name string, now time.Time) bool {
	return m.book.Muted(group, name, now)
}

// UptimeDays is sample-weighted availability over the last n UTC days
// plus today, from the JSONL file. No samples → (0, 0), never 100%.
func (m *Monitor) UptimeDays(name string, n int, now time.Time) (ratio float64, samples int) {
	if n < 1 {
		return 0, 0
	}
	since := now.UTC().AddDate(0, 0, -(n - 1)).Format("2006-01-02")
	m.daysMu.Lock()
	acc, ok := m.days[name]
	m.daysMu.Unlock()
	var today *state.DayAcc
	if ok {
		today = &acc
	}
	return m.histLog.Uptime(name, since, today)
}

// Days is one record per UTC day for the last n days ending today, oldest
// first, with today's in-progress day folded in. Days with no record come
// back empty rather than missing — see history.Log.Window.
func (m *Monitor) Days(name string, n int, now time.Time) []history.Daily {
	m.daysMu.Lock()
	acc, ok := m.days[name]
	m.daysMu.Unlock()
	var today *state.DayAcc
	if ok {
		today = &acc
	}
	return m.histLog.Window(name, n, now, today)
}

func (m *Monitor) recordDay(c config.Check, res check.Result, incidents int) {
	m.daysMu.Lock()
	acc, rolled, ok := history.RecordDay(m.days[c.Name], res, incidents)
	m.days[c.Name] = acc
	m.daysDirty = true
	m.daysMu.Unlock()
	if ok {
		m.flushDay(c.Name, c.Group, rolled)
	}
}

func (m *Monitor) rollDays(now time.Time) {
	today := now.UTC().Format("2006-01-02")
	type pair struct {
		name, group string
		acc         state.DayAcc
	}
	var flushed []pair
	m.daysMu.Lock()
	for name, acc := range m.days {
		if acc.Date == "" || acc.Date >= today {
			continue
		}
		flushed = append(flushed, pair{name, groupOf(m.cfg, name), acc})
		delete(m.days, name)
		m.daysDirty = true
	}
	m.daysMu.Unlock()
	for _, p := range flushed {
		m.flushDay(p.name, p.group, p.acc)
	}
	if len(flushed) > 0 {
		m.save()
	}
}

func (m *Monitor) flushDay(name, group string, acc state.DayAcc) {
	if acc.Samples == 0 && acc.Incidents == 0 {
		return
	}
	rec := history.ToDaily(name, group, acc)
	if err := m.histLog.Append(rec); err != nil {
		m.log.Error("could not append history", "path", m.histLog.Path(), "err", err)
		// Put it back so a later start retries rather than losing the day.
		m.daysMu.Lock()
		if _, exists := m.days[name]; !exists {
			m.days[name] = acc
			m.daysDirty = true
		}
		m.daysMu.Unlock()
	}
}

func (m *Monitor) watchHolds(ctx context.Context) {
	for {
		now := time.Now()
		events := m.book.Expire(now)
		for _, ev := range events {
			m.emit(ev)
		}
		if m.book.Dirty() {
			m.save()
		}
		deadline, ok := m.book.NextDeadline(time.Now())
		delay := time.Minute
		if ok {
			delay = time.Until(deadline)
			if delay < 0 {
				delay = 0
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.wakeHolds:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (m *Monitor) nudgeHolds() {
	select {
	case m.wakeHolds <- struct{}{}:
	default:
	}
}

func (m *Monitor) watchDays(ctx context.Context) {
	m.rollDays(time.Now())
	for {
		next := history.NextUTCMidnight(time.Now())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			m.rollDays(time.Now())
			if err := m.samples.Prune(time.Now()); err != nil {
				m.log.Error("could not prune samples", "path", m.samples.Path(), "err", err)
			}
		}
	}
}

func (m *Monitor) watchSamples(ctx context.Context) {
	ticker := time.NewTicker(history.SampleFlushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.samples.Flush(); err != nil {
				m.log.Error("could not flush samples", "path", m.samples.Path(), "err", err)
			}
		}
	}
}

func (m *Monitor) watchHeartbeat(ctx context.Context) {
	for {
		delay := m.heartbeatDelay(time.Now())
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			m.beat(time.Now())
		}
	}
}

func (m *Monitor) heartbeatDelay(now time.Time) time.Duration {
	interval := m.cfg.Alerting.Heartbeat
	m.beatMu.Lock()
	last := m.lastBeat
	m.beatMu.Unlock()
	if last.IsZero() {
		return 0
	}
	d := last.Add(interval).Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

// beat queues at most one still-alive event. A missed week is one
// message, not one per skipped interval: catching up would page the
// operator for lookout having been down, which they already know.
func (m *Monitor) beat(now time.Time) {
	if m.cfg.Alerting.Heartbeat <= 0 {
		return
	}
	down := m.downCount()
	configured := m.configuredCount()
	m.beatMu.Lock()
	if !m.lastBeat.IsZero() && now.Before(m.lastBeat.Add(m.cfg.Alerting.Heartbeat)) {
		m.beatMu.Unlock()
		return
	}
	closed := m.closedBeat
	m.closedBeat = 0
	m.lastBeat = now
	m.beatDirty = true
	m.beatMu.Unlock()

	m.emit(state.Event{
		Kind:  state.EventHeartbeat,
		At:    now,
		Alert: true,
		Heartbeat: &state.Heartbeat{
			Checks: configured,
			Down:   down,
			Closed: closed,
		},
	})
	m.save()
}

// configuredCount is what the operator wrote, not what lookout derived from
// it: a heartbeat saying 24 checks about a file listing 20 reads as a bug in
// one of them.
func (m *Monitor) configuredCount() int {
	n := 0
	for _, c := range m.cfg.Checks {
		if !c.Implicit {
			n++
		}
	}
	return n
}

func (m *Monitor) downCount() int {
	n := 0
	for _, c := range m.cfg.Checks {
		if m.machine.Status(c.Name) == state.StatusDown {
			n++
		}
	}
	return n
}

func (m *Monitor) noteClosed() {
	if m.cfg.Alerting.Heartbeat <= 0 {
		return
	}
	m.beatMu.Lock()
	m.closedBeat++
	m.beatDirty = true
	m.beatMu.Unlock()
}

func (m *Monitor) heartbeatDirty() bool {
	m.beatMu.Lock()
	defer m.beatMu.Unlock()
	return m.beatDirty
}

func (m *Monitor) clearHeartbeatDirty() {
	m.beatMu.Lock()
	defer m.beatMu.Unlock()
	m.beatDirty = false
}

func (m *Monitor) daysAreDirty() bool {
	m.daysMu.Lock()
	defer m.daysMu.Unlock()
	return m.daysDirty
}

func (m *Monitor) clearDaysDirty() {
	m.daysMu.Lock()
	defer m.daysMu.Unlock()
	m.daysDirty = false
}

func cloneDays(in map[string]state.DayAcc) map[string]state.DayAcc {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]state.DayAcc, len(in))
	for k, v := range in {
		if v.Durations != nil {
			v.Durations = append([]int64(nil), v.Durations...)
		}
		out[k] = v
	}
	return out
}

func checkByName(cfg *config.Config, name string) (config.Check, bool) {
	for _, c := range cfg.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return config.Check{}, false
}

func groupExists(cfg *config.Config, group string) bool {
	for _, c := range cfg.Checks {
		if c.Group == group {
			return true
		}
	}
	return false
}

func groupOf(cfg *config.Config, name string) string {
	c, _ := checkByName(cfg, name)
	return c.Group
}
