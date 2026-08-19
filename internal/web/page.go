package web

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/eeegoloauq/lookout/internal/history"
	"github.com/eeegoloauq/lookout/internal/state"
)

//go:embed page.html
var pageHTML string

//go:embed page.css
var pageCSS string

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

// timelineBuckets is how many slots the 24-hour bar is cut into: 48 half
// hours. Fewer hides a short outage inside a green block; more turns into
// a texture nobody can point at.
const timelineBuckets = 48

type pageView struct {
	Title    string
	Version  string
	Zone     string
	CSS      template.CSS
	Tally    string
	When     string
	Degraded string
	Muted    string
	Groups   []pageGroup
	Empty    bool
}

// pageGroup is one config group, rendered as a heading rather than as a
// column repeated on every row.
type pageGroup struct {
	Name  string
	Note  string
	Rows  []pageRow
	Bad   bool
	Empty bool
}

type pageRow struct {
	// ID is the fragment that opens this row. Expansion lives in the URL so
	// that the page's own reload — which closes any <details> — cannot shut
	// a panel someone is reading.
	ID       string
	Name     string
	Link     string
	Label    string
	RowClass string
	Note     string // what the status line adds: "12m", "5 of 20 failed"
	Checked  string
	Latency  string
	Uptime   string
	// Wide replaces the latency and uptime columns for a check where they
	// mean nothing — a registration is not "up 100% of the day", it runs
	// out on a date.
	Wide      string
	WideClass string
	// Badge is a short warning that rides next to the name: the site is
	// answering fine, but the name it lives on runs out next week.
	Badge      string
	BadgeClass string
	Muted      bool
	Timeline   []pageBucket
	Facts      []pageFact
	Incidents  []pageIncident
}

// pageIncident is one closed outage in the row's history.
type pageIncident struct {
	When   string
	For    string
	Reason string
}

// pageFact is one line of the expanded panel. A slice, not a struct with
// twenty optional fields: the panel shows what a check actually has.
type pageFact struct {
	Label string
	Value string
}

// pageBucket is one slot of the 24-hour bar.
type pageBucket struct {
	Class string
	Title string
}

func (s *server) page(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	loc := s.location()
	doc := s.snapshot(now)
	health, _ := s.health(now)

	up, down, unknown, unstable := 0, 0, 0, 0
	groups := map[string]*pageGroup{}
	var order []string
	for _, c := range doc.Checks {
		if c.Implicit {
			// A derived registration is not a row: the site that implied it
			// shows its expiry, and a board of names nobody wrote is noise.
			continue
		}
		row := pageRow{
			Name:    c.Name,
			Checked: "—",
			Latency: "—",
			Uptime:  "—",
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
		if c.Incident != nil {
			// How long it has been down belongs next to the word DOWN, not
			// in a column that is empty on every healthy row.
			row.Note = "for " + formatSpan(time.Duration(c.Incident.DurationMS)*time.Millisecond)
		}
		switch {
		case c.URL != "" && (strings.HasPrefix(c.URL, "http://") || strings.HasPrefix(c.URL, "https://")):
			// The target is the first thing anyone wants to open when a
			// row goes red; making them retype it is silly.
			row.Link = c.URL
		case c.Type == "domain" && c.Host != "":
			row.Link = "https://" + c.Host
		}
		for _, inc := range c.Incidents {
			row.Incidents = append(row.Incidents, pageIncident{
				When:   inc.StartedAt.In(loc).Format("2 Jan 15:04"),
				For:    formatSpan(time.Duration(inc.DurationMS) * time.Millisecond),
				Reason: inc.Reason,
			})
		}
		if c.LastProbe != nil {
			row.Checked = formatAgo(now, c.LastProbe.At)
			row.Latency = formatLatency(time.Duration(c.LastProbe.DurationMS) * time.Millisecond)
		}
		if c.Uptime24h != nil {
			row.Uptime = formatRatio(c.Uptime24h.Ratio)
		}
		row.Muted = c.Muted
		if c.Muted {
			row.RowClass += " muted"
		}
		// A bar of half-hour slots says nothing about a check that runs once
		// a day: it would be forty-seven slots of "no data" and one tick.
		if ring, ok := s.mon.History().Ring(c.Name); ok && !sparse(c) {
			row.Timeline = timeline(ring.Points(), now.In(loc))
		}
		if c.Type == "domain" {
			row.Wide = registrationSummary(c, loc)
			row.WideClass = expiryUrgency(c.DomainDaysLeft)
		}
		row.Badge, row.BadgeClass = registrationBadge(c)
		row.ID = slug(c.Name)
		row.Facts = facts(c, now, loc, row.Link != "")

		name := c.Group
		if name == "" {
			name = "Ungrouped"
		}
		g, ok := groups[name]
		if !ok {
			g = &pageGroup{Name: name}
			groups[name] = g
			order = append(order, name)
		}
		if row.RowClass != "up" && !c.Muted {
			g.Bad = true
		}
		g.Rows = append(g.Rows, row)
	}

	view := pageView{
		Title:   "lookout",
		Version: s.version,
		CSS:     template.CSS(pageCSS),
		Tally:   tally(up, down, unknown, unstable),
		When:    now.In(loc).Format("15:04:05"),
		Zone:    zoneName(now, loc),
		Empty:   len(doc.Checks) == 0,
	}
	if down > 0 {
		view.Title = fmt.Sprintf("lookout — %s down", plural(down, "check"))
	}
	for _, name := range order {
		g := groups[name]
		g.Note = groupNote(g.Rows)
		view.Groups = append(view.Groups, *g)
	}
	if health.Status == "degraded" {
		view.Degraded = health.Reason
	}
	view.Muted = formatMutes(doc.Mutes, now)

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

// sparse reports whether a check runs less often than one timeline slot, in
// which case the 24-hour bar has nothing to draw.
func sparse(c CheckStatus) bool {
	slot := history.Retention / timelineBuckets
	return c.IntervalMS > 0 && time.Duration(c.IntervalMS)*time.Millisecond >= slot
}

// registrationSummary is what a domain row shows instead of a response time
// and a 24-hour uptime: neither says anything about a name that is either
// registered until a date or not.
func registrationSummary(c CheckStatus, loc *time.Location) string {
	if c.DomainDaysLeft == nil {
		if c.DomainLookupUnknown {
			return "registry silent"
		}
		return "not looked up yet"
	}
	out := "expires " + formatDays(*c.DomainDaysLeft)
	if c.DomainExpires != nil {
		when := c.DomainExpires.In(loc)
		// The year is only worth its width when it is not this one.
		layout := "2 Jan"
		if when.Year() != time.Now().In(loc).Year() {
			layout = "2 Jan 2006"
		}
		out += " · " + when.Format(layout)
	}
	return out
}

// expiryUrgency colours the date on a row that is otherwise green: a
// registration with three days left is not an outage, but calling it plain
// "up" until the day it dies is how a domain gets lost.
func expiryUrgency(days *int) string {
	switch {
	case days == nil:
		return ""
	case *days <= 3:
		return "bad"
	case *days <= 14:
		return "soon"
	}
	return ""
}

// slug turns a check name into a URL fragment. Check names are unique, so
// the only thing to guard is the character set.
func slug(name string) string {
	var b strings.Builder
	b.WriteString("c-")
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// registrationBadge warns on a site row when the name it lives on is about
// to lapse. It stays silent the rest of the year: a badge that is always
// there is furniture, and furniture is not read.
func registrationBadge(c CheckStatus) (string, string) {
	r := c.Registration
	if r == nil {
		return "", ""
	}
	if r.Delegated != nil && !*r.Delegated {
		return "not delegated", "bad"
	}
	class := expiryUrgency(&r.DaysLeft)
	if class == "" {
		return "", ""
	}
	switch {
	case r.DaysLeft < 0:
		return "domain expired", class
	case r.DaysLeft == 0:
		return "domain expires today", class
	}
	return fmt.Sprintf("domain %dd", r.DaysLeft), class
}

// facts is the expanded panel: what the check watches, how it has behaved,
// and what runs out. Absent data produces no line at all — an empty field
// is a question the reader has to answer for themselves.
// location is the timezone clocks are shown in. Machine-readable output
// stays UTC; a person reading a board wants the time on their own wall.
func (s *server) location() *time.Location {
	if cfg := s.mon.Config(); cfg != nil && cfg.Location != nil {
		return cfg.Location
	}
	return time.Local
}

// zoneName is the abbreviation shown once in the header, so a clock on the
// page is never ambiguous about which one it is.
func zoneName(now time.Time, loc *time.Location) string {
	name, _ := now.In(loc).Zone()
	return name
}

func facts(c CheckStatus, now time.Time, loc *time.Location, linked bool) []pageFact {
	var out []pageFact
	add := func(label, value string) {
		if value != "" {
			out = append(out, pageFact{Label: label, Value: value})
		}
	}
	if !linked {
		// When the target is a link the template prints it; printing it
		// here as well is the same string twice.
		add("watching", target(c))
	}
	if c.Incident != nil {
		add("down since", c.Incident.StartedAt.In(loc).Format("2 Jan 15:04"))
	}
	if c.LastFailure != nil {
		when := formatAgo(now, c.LastFailure.At)
		if c.LastFailure.Reason != "" {
			add("last failure", when+" · "+c.LastFailure.Reason)
		} else {
			add("last failure", when)
		}
	}
	// Which box answered. After a migration this is the difference between
	// "the site is up" and "the site is up on the machine I thought I
	// turned off".
	add("connected", c.RemoteAddr)
	if c.Type == "domain" {
		// The uptime of a registry lookup is not the uptime of anything
		// anyone cares about, and neither is how fast RDAP answered.
		add("checked", "every "+formatSpan(time.Duration(c.IntervalMS)*time.Millisecond))
	} else {
		add("uptime", uptimeLine(c))
		add("response", latencyLine(c.Latency24h))
	}
	// A domain row already leads with its expiry date; repeating it inside
	// is noise. A certificate has no column of its own and belongs here.
	if e := expiryLine(c, loc, c.Type != "domain"); e != "" {
		add("expires", e)
	}
	// The site itself can be perfectly healthy and still be a week away
	// from disappearing, so its panel says when the name runs out.
	if r := c.Registration; r != nil {
		line := "registration " + formatDays(r.DaysLeft)
		if r.ExpiresAt != nil {
			line += " (" + r.ExpiresAt.In(loc).Format("2 Jan 2006") + ")"
		}
		if r.Name != "" {
			line += " · " + r.Name
		}
		if r.Delegated != nil && !*r.Delegated {
			line += " · NOT DELEGATED"
		}
		add("domain", line)
	}
	if c.DomainState != "" {
		who := c.DomainState
		if c.DomainSource != "" {
			who += " (" + c.DomainSource + ")"
		}
		add("registry", who)
	}
	return out
}

// target says what the check actually looks at, in the vocabulary of its
// type: a URL, a DNS question, or a registered name.
func target(c CheckStatus) string {
	switch {
	case c.URL != "":
		return c.URL
	case c.QueryType != "" && c.Host != "":
		q := c.QueryType + " " + c.Host
		if c.Resolver != "" {
			q += " @" + c.Resolver
		}
		return q
	case c.Host != "":
		return c.Host
	}
	return ""
}

func uptimeLine(c CheckStatus) string {
	parts := make([]string, 0, 3)
	if c.Uptime24h != nil {
		parts = append(parts, formatRatio(c.Uptime24h.Ratio)+" 24h")
	}
	if c.Uptime7d != nil {
		parts = append(parts, formatRatio(c.Uptime7d.Ratio)+" 7d")
	}
	if c.Uptime30d != nil {
		parts = append(parts, formatRatio(c.Uptime30d.Ratio)+" 30d")
	}
	return join(parts, " · ")
}

func latencyLine(l *LatencyView) string {
	if l == nil {
		return ""
	}
	// Percentiles, not an average: one slow probe drags a mean around and
	// hides it at the same time. p50 is what it usually feels like, p95 is
	// what the bad end of normal costs, max is the worst single answer of
	// the day. The worst is dropped when it is the same sample as p95 —
	// with a handful of probes they collide and the line reads as noise.
	p50 := formatLatency(time.Duration(l.P50MS) * time.Millisecond)
	p95 := formatLatency(time.Duration(l.P95MS) * time.Millisecond)
	worst := formatLatency(time.Duration(l.MaxMS) * time.Millisecond)
	if l.MaxMS == l.P95MS {
		return fmt.Sprintf("usual %s · slowest 5%% %s", p50, p95)
	}
	return fmt.Sprintf("usual %s · slowest 5%% %s · worst %s", p50, p95, worst)
}

func expiryLine(c CheckStatus, loc *time.Location, withDomain bool) string {
	var parts []string
	if c.CertDaysLeft != nil {
		s := "certificate in " + formatDays(*c.CertDaysLeft)
		if c.CertNotAfter != nil {
			s += " (" + c.CertNotAfter.In(loc).Format("2 Jan 2006") + ")"
		}
		parts = append(parts, s)
	}
	if c.DomainDaysLeft != nil && withDomain {
		s := "registration in " + formatDays(*c.DomainDaysLeft)
		if c.DomainExpires != nil {
			s += " (" + c.DomainExpires.In(loc).Format("2 Jan 2006") + ")"
		}
		parts = append(parts, s)
	}
	if c.DomainLookupUnknown && withDomain {
		parts = append(parts, "registry has not answered")
	}
	return join(parts, " · ")
}

// timeline folds the ring into fixed half-hour slots. A slot with any
// failure is drawn as failing: an outage that ended must stay visible,
// which is the whole reason to look at a bar instead of a number.
func timeline(points []history.Point, now time.Time) []pageBucket {
	slot := history.Retention / timelineBuckets
	type acc struct {
		ok, bad             int
		ms                  []int64
		worst               time.Duration
		worstAt             time.Time
		firstFail, lastFail time.Time
	}
	buckets := make([]acc, timelineBuckets)
	for _, p := range points {
		// Slots are counted back from now, not forward from a truncated
		// start: anchoring at the start left the newest half hour in a
		// bucket past the end of the bar, so a failure that just happened
		// was the one thing the bar did not show.
		back := int(now.Sub(p.At) / slot)
		i := timelineBuckets - 1 - back
		if p.Outcome == "unknown" || i < 0 || i >= timelineBuckets {
			continue
		}
		b := &buckets[i]
		if p.Outcome.Succeeded() {
			b.ok++
		} else {
			b.bad++
			if b.firstFail.IsZero() || p.At.Before(b.firstFail) {
				b.firstFail = p.At
			}
			if p.At.After(b.lastFail) {
				b.lastFail = p.At
			}
		}
		if p.Duration > 0 {
			b.ms = append(b.ms, p.Duration.Milliseconds())
			if p.Duration > b.worst {
				b.worst, b.worstAt = p.Duration, p.At
			}
		}
	}
	out := make([]pageBucket, timelineBuckets)
	for i, b := range buckets {
		from := now.Add(-time.Duration(timelineBuckets-i) * slot)
		span := from.Format("15:04") + "–" + from.Add(slot).Format("15:04")
		total := b.ok + b.bad
		if total == 0 {
			out[i] = pageBucket{Class: "n", Title: span + "\nno data"}
			continue
		}
		// The tooltip is where the detail goes that the colour cannot
		// carry: how many probes, how slow the worst one was and when,
		// and the window the failures actually fell in.
		lines := []string{span}
		if b.bad > 0 {
			lines = append(lines, fmt.Sprintf("%d of %d failed", b.bad, total))
			if !b.firstFail.IsZero() {
				fail := "failing " + b.firstFail.Format("15:04")
				if b.lastFail.After(b.firstFail) {
					fail += "–" + b.lastFail.Format("15:04")
				}
				lines = append(lines, fail)
			}
		} else {
			lines = append(lines, fmt.Sprintf("%d ok", b.ok))
		}
		if len(b.ms) > 0 {
			sort.Slice(b.ms, func(x, y int) bool { return b.ms[x] < b.ms[y] })
			typical := formatLatency(time.Duration(percentile(b.ms, 50)) * time.Millisecond)
			line := "typical " + typical
			if b.worst > 0 && b.worst.Milliseconds() != percentile(b.ms, 50) {
				line += " · worst " + formatLatency(b.worst) + " at " + b.worstAt.Format("15:04")
			}
			lines = append(lines, line)
		}
		class := "o"
		switch {
		case b.bad > 0 && b.ok == 0:
			class = "b"
		case b.bad > 0:
			class = "p"
		}
		out[i] = pageBucket{Class: class, Title: strings.Join(lines, "\n")}
	}
	return out
}

// groupNote is the count beside a group heading: what is wrong, or how
// many checks are fine.
func groupNote(rows []pageRow) string {
	counts := map[string]int{}
	for _, r := range rows {
		switch {
		case r.Label == "DOWN":
			counts["down"]++
		case r.Label == "UNSTABLE":
			counts["unstable"]++
		case r.Label == "UNKNOWN":
			counts["unknown"]++
		default:
			counts["up"]++
		}
	}
	var parts []string
	for _, k := range []string{"down", "unstable", "unknown"} {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%d up", counts["up"])
	}
	return join(parts, " · ")
}

func join(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
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
	return join(parts, " · ")
}

func formatRatio(r float64) string {
	pct := r * 100
	// 99.97% rounded to one decimal is 100.0%, which reads as "nothing
	// ever failed" on a day that had an outage.
	if pct < 100 && pct > 99.9 {
		return "99.9%"
	}
	return fmt.Sprintf("%.1f%%", pct)
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

func formatMutes(mutes []MuteView, now time.Time) string {
	if len(mutes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(mutes))
	for _, m := range mutes {
		scope := "all checks"
		switch {
		case m.Check != "":
			scope = m.Check
		case m.Group != "":
			scope = "group " + m.Group
		}
		left := m.Until.Sub(now)
		if left < 0 {
			left = 0
		}
		parts = append(parts, fmt.Sprintf("%s until %s (%s left)", scope, m.Until.UTC().Format("15:04 UTC"), formatSpan(left)))
	}
	return "Alerts muted: " + join(parts, " · ")
}

func formatDays(n int) string {
	switch {
	case n < 0:
		return fmt.Sprintf("%s ago", plural(-n, "day"))
	case n == 0:
		return "under a day"
	default:
		return plural(n, "day")
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
