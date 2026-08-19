package web

import (
	"net/http"
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
	Checks      []CheckStatus `json:"checks"`
}

// CheckStatus is one check as a foreign consumer should see it.
type CheckStatus struct {
	Name     string `json:"name"`
	Group    string `json:"group,omitempty"`
	Type     string `json:"type"`
	URL      string `json:"url,omitempty"`
	Status   string `json:"status"`
	Unstable bool   `json:"unstable"`

	// LastProbe is null until the first result lands. A restart starts
	// from empty history, which must read as "no data", not as failure.
	LastProbe *ProbeView `json:"last_probe"`
	// Uptime24h is null when the ring has no samples: 0 would look like
	// a total outage and 1 would look like perfect health (SPEC §9.2).
	Uptime24h *UptimeView `json:"uptime_24h"`
	// Incident is null unless the check is in a confirmed outage.
	Incident *IncidentView `json:"incident"`
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
	return StatusDocument{
		Version:     APIVersion,
		Build:       s.version,
		GeneratedAt: now.UTC(),
		Checks:      checks,
	}
}

func (s *server) checkStatus(c config.Check, now time.Time) CheckStatus {
	cs, _ := s.mon.Machine().State(c.Name)
	status := cs.Status
	if status == "" {
		status = state.StatusUnknown
	}
	out := CheckStatus{
		Name:     c.Name,
		Group:    c.Group,
		Type:     string(c.Type),
		URL:      check.MaskURL(c.URL),
		Status:   string(status),
		Unstable: cs.Unstable,
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
	}
	if status == state.StatusDown && !cs.IncidentStart.IsZero() {
		out.Incident = &IncidentView{
			StartedAt:  cs.IncidentStart.UTC(),
			DurationMS: now.Sub(cs.IncidentStart).Milliseconds(),
		}
		if out.Incident.DurationMS < 0 {
			out.Incident.DurationMS = 0
		}
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
			OK:         !p.Outcome.Failed(),
			Outcome:    string(p.Outcome),
			DurationMS: p.Duration.Milliseconds(),
			StatusCode: p.StatusCode,
		}
	}
	return out
}
