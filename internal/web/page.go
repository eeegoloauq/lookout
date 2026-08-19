package web

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/eeegoloauq/lookout/internal/state"
)

//go:embed page.html
var pageHTML string

//go:embed page.css
var pageCSS string

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

type pageView struct {
	Title    string
	CSS      template.CSS
	Tally    string
	When     string
	Degraded string
	Checks   []pageRow
}

type pageRow struct {
	Name     string
	Group    string
	Label    string
	RowClass string
	Checked  string
	Latency  string
	Uptime   string
	Expiry   string
	Incident string
}

func (s *server) page(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	doc := s.snapshot(now)
	health, _ := s.health(now)

	up, down, unknown, unstable := 0, 0, 0, 0
	rows := make([]pageRow, 0, len(doc.Checks))
	for _, c := range doc.Checks {
		row := pageRow{
			Name:     c.Name,
			Group:    c.Group,
			Checked:  "—",
			Latency:  "—",
			Uptime:   "—",
			Expiry:   "—",
			Incident: "—",
		}
		switch {
		case c.Status == string(state.StatusDown):
			row.Label, row.RowClass = "DOWN", "down"
			down++
		case c.Unstable:
			row.Label, row.RowClass = "UNSTABLE", "unstable"
			unstable++
		case c.Status == string(state.StatusUp):
			row.Label, row.RowClass = "UP", "up"
			up++
		default:
			row.Label, row.RowClass = "UNKNOWN", "unknown"
			unknown++
		}
		if c.LastProbe != nil {
			row.Checked = formatAgo(now, c.LastProbe.At)
			row.Latency = formatLatency(time.Duration(c.LastProbe.DurationMS) * time.Millisecond)
		}
		if c.Uptime24h != nil {
			row.Uptime = fmt.Sprintf("%.1f%%", c.Uptime24h.Ratio*100)
		}
		if c.Incident != nil {
			row.Incident = formatSpan(time.Duration(c.Incident.DurationMS) * time.Millisecond)
		}
		row.Expiry = formatExpiry(c)
		rows = append(rows, row)
	}

	title := "lookout"
	if down > 0 {
		title = fmt.Sprintf("lookout — %s down", plural(down, "check"))
	}

	view := pageView{
		Title:  title,
		CSS:    template.CSS(pageCSS),
		Tally:  tally(up, down, unknown, unstable),
		When:   now.UTC().Format("15:04:05 UTC"),
		Checks: rows,
	}
	if health.Status == "degraded" {
		view.Degraded = health.Reason
	}

	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, view); err != nil {
		http.Error(w, "status page failed to render", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func tally(up, down, unknown, unstable int) string {
	parts := make([]string, 0, 4)
	if down > 0 {
		parts = append(parts, fmt.Sprintf("%d down", down))
	}
	if unstable > 0 {
		parts = append(parts, fmt.Sprintf("%d unstable", unstable))
	}
	if unknown > 0 {
		parts = append(parts, fmt.Sprintf("%d unknown", unknown))
	}
	if up > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%d up", up))
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += " · " + p
	}
	return out
}

func formatAgo(now, at time.Time) string {
	if at.IsZero() {
		return "—"
	}
	d := now.Sub(at)
	if d < 0 {
		d = 0
	}
	switch {
	case d < 2*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return at.UTC().Format("2006-01-02 15:04")
	}
}

func formatLatency(d time.Duration) string {
	if d <= 0 {
		return "0ms"
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func formatExpiry(c CheckStatus) string {
	switch {
	case c.CertDaysLeft != nil && c.DomainDaysLeft != nil:
		return fmt.Sprintf("cert %s · domain %s", formatDays(*c.CertDaysLeft), formatDays(*c.DomainDaysLeft))
	case c.CertDaysLeft != nil:
		return "cert " + formatDays(*c.CertDaysLeft)
	case c.DomainDaysLeft != nil:
		return "domain " + formatDays(*c.DomainDaysLeft)
	case c.DomainLookupUnknown:
		return "registry ?"
	default:
		return "—"
	}
}

func formatDays(n int) string {
	switch {
	case n < 0:
		return fmt.Sprintf("%dd ago", -n)
	case n == 0:
		return "<1d"
	default:
		return fmt.Sprintf("%dd", n)
	}
}

func formatSpan(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return formatLatency(d)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dm", m)
	}
}
