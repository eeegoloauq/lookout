package web

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/eeegoloauq/lookout/internal/history"
	"github.com/eeegoloauq/lookout/internal/state"
)

// Prometheus names follow the checker vocabulary in docs/research.md
// §1.5 (Blackbox: probe_success / probe_duration_seconds as gauges of
// the last result) and §4.1 (lookout_undelivered_alert_age_seconds).
// They are prefixed so they cannot collide with a real Blackbox scrape
// of the same Prometheus. Certificate and domain gauges are omitted
// until a value has been observed — emitting 0 would page as "expired".

func (s *server) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.writeMetrics(w, time.Now())
}

func (s *server) writeMetrics(w io.Writer, now time.Time) {
	cfg := s.mon.Config()
	type series struct {
		up, unstable  bool
		haveState     bool
		haveProbe     bool
		probeOK       bool
		probeDuration float64
		haveUptime    bool
		uptime        float64
		haveIncident  bool
		incident      float64
		haveCert      bool
		certDays      float64
		haveDomain    bool
		domainDays    float64
		check, group  string
	}
	rows := make([]series, 0, len(cfg.Checks))
	for _, c := range cfg.Checks {
		row := series{check: c.Name, group: c.Group}
		if cs, ok := s.mon.Machine().State(c.Name); ok {
			switch cs.Status {
			case state.StatusUp:
				row.haveState, row.up = true, true
			case state.StatusDown:
				row.haveState, row.up = true, false
			}
			row.unstable = cs.Unstable
			if cs.Status == state.StatusDown && !cs.IncidentStart.IsZero() {
				d := now.Sub(cs.IncidentStart).Seconds()
				if d < 0 {
					d = 0
				}
				row.haveIncident, row.incident = true, d
			}
			if !cs.CertNotAfter.IsZero() {
				row.haveCert, row.certDays = true, float64(state.DaysLeft(cs.CertNotAfter, now))
			}
			if !cs.DomainExpiresAt.IsZero() {
				row.haveDomain, row.domainDays = true, float64(state.DaysLeft(cs.DomainExpiresAt, now))
			}
		}
		if ring, ok := s.mon.History().Ring(c.Name); ok {
			if last, ok := ring.Last(); ok {
				row.haveProbe = true
				row.probeOK = last.Outcome.Succeeded()
				row.probeDuration = last.Duration.Seconds()
			}
			ratio, samples := ring.Uptime(now.Add(-history.Retention))
			if samples > 0 {
				row.haveUptime, row.uptime = true, ratio
			}
		}
		rows = append(rows, row)
	}

	writeFamily(w, "lookout_up", "gauge",
		"1 if the check is in a confirmed up state, 0 if confirmed down. Absent while the state is still unknown.")
	for _, r := range rows {
		if !r.haveState {
			continue
		}
		v := 0
		if r.up {
			v = 1
		}
		writeSample(w, "lookout_up", r.check, r.group, strconv.Itoa(v))
	}

	writeFamily(w, "lookout_check_unstable", "gauge",
		"1 if the N-of-M detector currently considers the check unstable.")
	for _, r := range rows {
		v := 0
		if r.unstable {
			v = 1
		}
		writeSample(w, "lookout_check_unstable", r.check, r.group, strconv.Itoa(v))
	}

	writeFamily(w, "lookout_probe_success", "gauge",
		"1 if the most recent probe succeeded, 0 if it failed. Absent before the first probe.")
	for _, r := range rows {
		if !r.haveProbe {
			continue
		}
		v := 0
		if r.probeOK {
			v = 1
		}
		writeSample(w, "lookout_probe_success", r.check, r.group, strconv.Itoa(v))
	}

	writeFamily(w, "lookout_probe_duration_seconds", "gauge",
		"Wall time of the most recent probe.")
	for _, r := range rows {
		if !r.haveProbe {
			continue
		}
		writeSample(w, "lookout_probe_duration_seconds", r.check, r.group, formatFloat(r.probeDuration))
	}

	writeFamily(w, "lookout_uptime_ratio", "gauge",
		"Fraction of successful probes in the last 24 hours. Absent when there are no samples.")
	for _, r := range rows {
		if !r.haveUptime {
			continue
		}
		writeSample(w, "lookout_uptime_ratio", r.check, r.group, formatFloat(r.uptime))
	}

	writeFamily(w, "lookout_incident_duration_seconds", "gauge",
		"Length of the current confirmed outage. Absent when the check is not down.")
	for _, r := range rows {
		if !r.haveIncident {
			continue
		}
		writeSample(w, "lookout_incident_duration_seconds", r.check, r.group, formatFloat(r.incident))
	}

	writeFamily(w, "lookout_cert_days_left", "gauge",
		"Whole days until the leaf certificate expires. Absent when no certificate has been observed.")
	for _, r := range rows {
		if !r.haveCert {
			continue
		}
		writeSample(w, "lookout_cert_days_left", r.check, r.group, formatFloat(r.certDays))
	}

	writeFamily(w, "lookout_domain_days_left", "gauge",
		"Whole days until the domain registration expires. Absent when the registry has not been read.")
	for _, r := range rows {
		if !r.haveDomain {
			continue
		}
		writeSample(w, "lookout_domain_days_left", r.check, r.group, formatFloat(r.domainDays))
	}

	box := s.mon.Outbox()
	writeFamily(w, "lookout_undelivered_alerts", "gauge",
		"Alert events waiting in the durable outbox.")
	fmt.Fprintf(w, "lookout_undelivered_alerts %d\n", len(box.Items))

	writeFamily(w, "lookout_undelivered_alert_age_seconds", "gauge",
		"Age of the oldest undelivered alert. 0 if the outbox is empty.")
	age := 0.0
	if len(box.Items) > 0 {
		age = now.Sub(box.Items[0].Enqueued).Seconds()
		if age < 0 {
			age = 0
		}
	}
	fmt.Fprintf(w, "lookout_undelivered_alert_age_seconds %s\n", formatFloat(age))

	writeFamily(w, "lookout_alert_delivery_attempts", "gauge",
		"Consecutive failed delivery attempts for the current outbox batch.")
	fmt.Fprintf(w, "lookout_alert_delivery_attempts %d\n", box.Attempts)
}

func writeFamily(w io.Writer, name, typ, help string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func writeSample(w io.Writer, name, check, group, value string) {
	fmt.Fprintf(w, "%s{check=%q,group=%q} %s\n", name, promLabel(check), promLabel(group), value)
}

// promLabel escapes a value that will be wrapped in %q. Go's %q is close
// to Prometheus's label encoding (quoted, backslash and quote escaped)
// but emits \xHH for some bytes; check names are YAML identifiers in
// practice, so we still strip CR/LF which would break the text format.
func promLabel(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
