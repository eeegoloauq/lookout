package web

import (
	"net/http"
	"sort"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
	"github.com/eeegoloauq/lookout/internal/history"
	"github.com/eeegoloauq/lookout/internal/state"
)

// StatusDocument is GET /api/status. Field names and nesting are the
// contract; do not rename them to match an internal struct.
type StatusDocument struct {
	Version     int           `json:"version"`
	Build       string        `json:"build,omitempty"`
	GeneratedAt time.Time     `json:"generated_at"`
	Mutes       []MuteView    `json:"mutes,omitempty"`
	Checks      []CheckStatus `json:"checks"`
}

// MuteView is one currently active quiet window.
type MuteView struct {
	Until  time.Time `json:"until"`
	Group  string    `json:"group,omitempty"`
	Check  string    `json:"check,omitempty"`
	Source string    `json:"source"`
	Held   int       `json:"held,omitempty"`
}

// CheckStatus is one check as a foreign consumer should see it.
type CheckStatus struct {
	Name  string `json:"name"`
	Group string `json:"group,omitempty"`
	Type  string `json:"type"`
	URL   string `json:"url,omitempty"`
	// Host and QueryType describe a dns or domain check, which has no URL:
	// without them the page could not say what it is watching.
	Host      string `json:"host,omitempty"`
	QueryType string `json:"query_type,omitempty"`
	Status    string `json:"status"`
	Unstable  bool   `json:"unstable"`

	// LastProbe is null until the first result lands. A restart starts
	// from empty history, which must read as "no data", not as failure.
	LastProbe *ProbeView `json:"last_probe"`
	// Uptime24h is null when the ring has no samples: 0 would look like
	// a total outage and 1 would look like perfect health (SPEC §9.2).
	Uptime24h *UptimeView `json:"uptime_24h"`
	// Incident is null unless the check is in a confirmed outage.
	Incident *IncidentView `json:"incident"`

	// CertDaysLeft is null when no certificate has been observed.
	// Zero means "expires within 24h", not "unknown" — emitting 0 for
	// an HTTP check would page as expired.
	CertDaysLeft *int       `json:"cert_days_left"`
	CertNotAfter *time.Time `json:"cert_not_after"`
	// DomainDaysLeft is null until a registry lookup has succeeded.
	DomainDaysLeft *int       `json:"domain_days_left"`
	DomainExpires  *time.Time `json:"domain_expires_at"`
	DomainState    string     `json:"domain_state,omitempty"`
	DomainSource   string     `json:"domain_source,omitempty"`
	// DomainLookupUnknown is true while the registry has not answered
	// since DomainUnknownSince — not the same as a down domain.
	DomainLookupUnknown bool `json:"domain_lookup_unknown,omitempty"`
	// DomainDelegated is null when the registry vocabulary has no
	// DELEGATED flag (gTLDs). False is "we saw it and it is gone".
	DomainDelegated *bool `json:"domain_delegated,omitempty"`

	// Muted is true while a quiet window covers this check. Probes
	// still run; only delivery is suppressed.
	Muted bool `json:"muted"`

	// Latency24h is the response-time shape over the in-memory window.
	// The median says what normal feels like, p95 and the worst sample
	// say what the slow tail costs — one number could not do both.
	Latency24h *LatencyView `json:"latency_24h"`
	// LastFailure is the most recent failing probe and why, even after
	// the check recovered. Null until something has failed once.
	LastFailure *FailureView `json:"last_failure"`
	// Incidents is the closed-outage log, newest first: when it broke,
	// how long it stayed broken and what it said while it was.
	Incidents []IncidentLogView `json:"incidents,omitempty"`

	// Uptime7d / Uptime30d come from the JSONL history plus today.
	// Null when there are no samples, never 100% by omission.
	Uptime7d  *UptimeView `json:"uptime_7d"`
	Uptime30d *UptimeView `json:"uptime_30d"`
}

// ProbeView is the most recent probe of a check.
type ProbeView struct {
	At         time.Time `json:"at"`
	DurationMS int64     `json:"duration_ms"`
	Outcome    string    `json:"outcome"`
	StatusCode int       `json:"status_code,omitempty"`
}

// UptimeView is availability over the in-memory window.
type UptimeView struct {
	Ratio   float64 `json:"ratio"`
	Samples int     `json:"samples"`
}

// LatencyView is the response-time distribution over the last 24 hours.
type LatencyView struct {
	P50MS   int64 `json:"p50_ms"`
	P95MS   int64 `json:"p95_ms"`
	MaxMS   int64 `json:"max_ms"`
	Samples int   `json:"samples"`
}

// FailureView is the last failing probe of a check.
type FailureView struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

// IncidentLogView is one closed outage.
type IncidentLogView struct {
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
	DurationMS int64     `json:"duration_ms"`
	Reason     string    `json:"reason,omitempty"`
}

// IncidentView is the current confirmed outage, if any.
type IncidentView struct {
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
}

// HistoryDocument is GET /api/checks/{name}.
type HistoryDocument struct {
	Version int            `json:"version"`
	Name    string         `json:"name"`
	Group   string         `json:"group,omitempty"`
	Status  string         `json:"status"`
	Points  []HistoryPoint `json:"points"`
}

// HistoryPoint is one sample from the ring buffer (SPEC §9.2).
type HistoryPoint struct {
	At         time.Time `json:"at"`
	OK         bool      `json:"ok"`
	Outcome    string    `json:"outcome"`
	DurationMS int64     `json:"duration_ms"`
	StatusCode int       `json:"status_code,omitempty"`
}

func (s *server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.snapshot(time.Now()))
}

func (s *server) checkHistory(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, ok := checkByName(s.mon.Config(), name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "check not found"})
		return
	}
	now := time.Now()
	st := s.checkStatus(c, now)
	doc := HistoryDocument{
		Version: APIVersion,
		Name:    c.Name,
		Group:   c.Group,
		Status:  st.Status,
		Points:  historyPoints(s.mon.History(), c.Name),
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *server) snapshot(now time.Time) StatusDocument {
	cfg := s.mon.Config()
	checks := make([]CheckStatus, 0, len(cfg.Checks))
	for _, c := range cfg.Checks {
		checks = append(checks, s.checkStatus(c, now))
	}
	var mutes []MuteView
	for _, v := range s.mon.Mutes(now) {
		mutes = append(mutes, MuteView{
			Until:  v.Until.UTC(),
			Group:  v.Group,
			Check:  v.Check,
			Source: v.Source,
			Held:   v.Held,
		})
	}
	return StatusDocument{
		Version:     APIVersion,
		Build:       s.version,
		GeneratedAt: now.UTC(),
		Mutes:       mutes,
		Checks:      checks,
	}
}

// latency summarises a ring of points. Probes that never got a response
// are left out: their duration is the timeout, which would describe the
// configuration rather than the service.
func latency(points []history.Point) *LatencyView {
	ms := make([]int64, 0, len(points))
	for _, p := range points {
		if p.Outcome == check.OutcomeUnknown || p.Duration <= 0 {
			continue
		}
		ms = append(ms, p.Duration.Milliseconds())
	}
	if len(ms) == 0 {
		return nil
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i] < ms[j] })
	return &LatencyView{
		P50MS:   percentile(ms, 50),
		P95MS:   percentile(ms, 95),
		MaxMS:   ms[len(ms)-1],
		Samples: len(ms),
	}
}

// percentile is the nearest-rank percentile of a sorted slice.
func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := (p*len(sorted) + 99) / 100
	return sorted[min(max(i, 1), len(sorted))-1]
}

func (s *server) checkStatus(c config.Check, now time.Time) CheckStatus {
	cs, _ := s.mon.Machine().State(c.Name)
	status := cs.Status
	if status == "" {
		status = state.StatusUnknown
	}
	out := CheckStatus{
		Name:      c.Name,
		Group:     c.Group,
		Type:      string(c.Type),
		URL:       check.MaskURL(c.URL),
		Host:      c.Host,
		QueryType: string(c.QueryType),
		Status:    string(status),
		Unstable:  cs.Unstable,
	}
	if ring, ok := s.mon.History().Ring(c.Name); ok {
		if last, ok := ring.Last(); ok {
			out.LastProbe = &ProbeView{
				At:         last.At.UTC(),
				DurationMS: last.Duration.Milliseconds(),
				Outcome:    string(last.Outcome),
				StatusCode: last.StatusCode,
			}
		}
		ratio, samples := ring.Uptime(now.Add(-history.Retention))
		if samples > 0 {
			out.Uptime24h = &UptimeView{Ratio: ratio, Samples: samples}
		}
		if lat := latency(ring.Points()); lat != nil {
			out.Latency24h = lat
		}
	}
	for _, inc := range cs.Incidents {
		out.Incidents = append(out.Incidents, IncidentLogView{
			StartedAt:  inc.Start.UTC(),
			EndedAt:    inc.End.UTC(),
			DurationMS: inc.Duration().Milliseconds(),
			Reason:     inc.Reason,
		})
	}
	if !cs.LastFailureAt.IsZero() {
		out.LastFailure = &FailureView{At: cs.LastFailureAt.UTC(), Reason: cs.LastFailureReason}
	}
	if ratio, samples := s.mon.UptimeDays(c.Name, 7, now); samples > 0 {
		out.Uptime7d = &UptimeView{Ratio: ratio, Samples: samples}
	}
	if ratio, samples := s.mon.UptimeDays(c.Name, 30, now); samples > 0 {
		out.Uptime30d = &UptimeView{Ratio: ratio, Samples: samples}
	}
	out.Muted = s.mon.CheckMuted(c.Group, c.Name, now)
	if status == state.StatusDown && !cs.IncidentStart.IsZero() {
		out.Incident = &IncidentView{
			StartedAt:  cs.IncidentStart.UTC(),
			DurationMS: now.Sub(cs.IncidentStart).Milliseconds(),
		}
		if out.Incident.DurationMS < 0 {
			out.Incident.DurationMS = 0
		}
	}
	if !cs.CertNotAfter.IsZero() {
		d := state.DaysLeft(cs.CertNotAfter, now)
		t := cs.CertNotAfter.UTC()
		out.CertDaysLeft = &d
		out.CertNotAfter = &t
	}
	if !cs.DomainExpiresAt.IsZero() {
		d := state.DaysLeft(cs.DomainExpiresAt, now)
		t := cs.DomainExpiresAt.UTC()
		out.DomainDaysLeft = &d
		out.DomainExpires = &t
		out.DomainState = cs.DomainState
		out.DomainSource = cs.DomainSource
	}
	out.DomainLookupUnknown = !cs.DomainUnknownSince.IsZero()
	if cs.DomainDelegationKnown {
		d := cs.DomainDelegated
		out.DomainDelegated = &d
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

func historyPoints(h *history.History, name string) []HistoryPoint {
	ring, ok := h.Ring(name)
	if !ok {
		return []HistoryPoint{}
	}
	src := ring.Points()
	out := make([]HistoryPoint, len(src))
	for i, p := range src {
		out[i] = HistoryPoint{
			At:         p.At.UTC(),
			OK:         p.Outcome.Succeeded(),
			Outcome:    string(p.Outcome),
			DurationMS: p.Duration.Milliseconds(),
			StatusCode: p.StatusCode,
		}
	}
	return out
}
