package alert

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/state"
)

// telegramMaxRunes is sendMessage's documented limit. Crossing it makes
// Telegram reject the request, and a rejected request is retried forever,
// so the formatter must stay under it rather than hope.
const telegramMaxRunes = 4096

// Format renders a batch of events as one plain-text message.
//
// The shape is fixed (SPEC §6). One emoji, at the very start of the message,
// for the worst thing in it. A single event is a headline plus at most two
// supporting lines. A batch is a counting headline, a blank line, and one
// line per event, worst first. Group names, per-event glyphs and field
// labels are deliberately absent: the check name is already unique, and an
// alert is read at a glance on a phone, not studied.
//
// No Markdown is used: parse_mode would break on an arbitrary response body
// that happens to contain * or `.
func Format(events []state.Event) string {
	switch len(events) {
	case 0:
		return ""
	case 1:
		return clip(glyph(severity(events[0]))+" "+detail(events[0]), telegramMaxRunes)
	}
	ordered := worstFirst(events)
	header := glyph(severity(ordered[0])) + " " + batchHeader(ordered)
	lines := make([]string, len(ordered))
	for i, ev := range ordered {
		lines[i] = line(ev)
	}
	return fit(header, lines)
}

// severity ranks an event so the message can lead with the worst of them.
type severityRank int

const (
	sevQuiet severityRank = iota // bookkeeping: mute digests, overflow digests
	sevGood                      // recoveries
	sevSoon                      // something runs out later
	sevWarn                      // degraded, changed, or not yet an outage
	sevBad                       // it is down right now
)

func severity(ev state.Event) severityRank {
	switch ev.Kind {
	case state.EventUp, state.EventDelegated:
		return sevGood
	case state.EventUnstable, state.EventDrift:
		return sevWarn
	case state.EventStale:
		return sevSoon
	case state.EventExpiry:
		if ev.Expiry != nil && ev.Expiry.DaysLeft < 0 {
			return sevBad
		}
		return sevSoon
	case state.EventHeld, state.EventSummary:
		return sevQuiet
	case state.EventDown:
		// A response we can no longer read is a changed API, not an outage.
		if ev.Result.Outcome == check.OutcomeMalformed {
			return sevWarn
		}
		return sevBad
	}
	return sevBad
}

// glyph is the single emoji a message opens with. Colour registers in a
// crowded chat list before words do — but once per message, not once per line.
func glyph(s severityRank) string {
	switch s {
	case sevQuiet:
		return "\U0001F515" // 🔕
	case sevGood:
		return "✅" // ✅
	case sevSoon:
		return "\U0001F7E1" // 🟡
	case sevWarn:
		return "\U0001F7E0" // 🟠
	}
	return "\U0001F534" // 🔴
}

func worstFirst(events []state.Event) []state.Event {
	out := append([]state.Event(nil), events...)
	sort.SliceStable(out, func(i, j int) bool {
		return severity(out[i]) > severity(out[j])
	})
	return out
}

// batchHeader counts what happened, in words, worst kind first:
// "3 down, 1 back up".
func batchHeader(events []state.Event) string {
	counts := map[string]int{}
	order := []string{}
	rank := map[string]severityRank{}
	for _, ev := range events {
		k := headerKey(ev)
		if counts[k] == 0 {
			order = append(order, k)
			rank[k] = severity(ev)
		}
		counts[k]++
	}
	sort.SliceStable(order, func(i, j int) bool { return rank[order[i]] > rank[order[j]] })
	parts := make([]string, 0, len(order))
	for _, k := range order {
		parts = append(parts, headerPhrase(k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

func headerKey(ev state.Event) string {
	if ev.Kind == state.EventExpiry {
		if ev.Expiry != nil && ev.Expiry.Kind == state.ExpiryDomain {
			return "expiry.domain"
		}
		return "expiry.cert"
	}
	return string(ev.Kind)
}

func headerPhrase(key string, n int) string {
	switch key {
	case string(state.EventDown):
		return fmt.Sprintf("%d down", n)
	case string(state.EventStillDown):
		return fmt.Sprintf("%d still down", n)
	case string(state.EventUp):
		return fmt.Sprintf("%d back up", n)
	case string(state.EventUnstable):
		return fmt.Sprintf("%d flapping", n)
	case string(state.EventUndelegated):
		return fmt.Sprintf("%d not delegated", n)
	case string(state.EventDelegated):
		return fmt.Sprintf("%d delegated again", n)
	case string(state.EventDrift):
		return plural(n, "DNS change")
	case "expiry.domain":
		return plural(n, "domain") + " expiring"
	case "expiry.cert":
		return plural(n, "certificate") + " expiring"
	case string(state.EventStale):
		return fmt.Sprintf("%d registry lookups stale", n)
	case string(state.EventHeld):
		return "mute ended"
	case string(state.EventSummary):
		return "folded alerts"
	}
	return fmt.Sprintf("%d %s", n, key)
}

// detail renders one event as the whole message: a headline that reads as a
// sentence, then only the facts that say what to do about it.
func detail(ev state.Event) string {
	head := headline(ev)
	var extra []string
	switch ev.Kind {
	case state.EventDown, state.EventStillDown, state.EventUnstable:
		if c := cause(ev.Result); c != "" {
			extra = append(extra, c)
		}
		if b := body(ev.Result); b != "" {
			extra = append(extra, b)
		}
	case state.EventDrift:
		if ev.Drift != nil {
			extra = append(extra,
				"was: "+oneLine(clip(ev.Drift.Before, 200)),
				"now: "+oneLine(clip(ev.Drift.After, 200)))
		}
	case state.EventUndelegated:
		// The registry state is the whole content of this alert: it says
		// whether the domain was switched off for non-payment or by a court.
		if ev.Result.DomainState != "" {
			extra = append(extra, "registry: "+ev.Result.DomainState)
		}
		if !ev.Result.DomainFreeDate.IsZero() {
			extra = append(extra, "released "+date(ev.Result.DomainFreeDate, ev.At))
		}
	case state.EventStale:
		if !ev.Result.DomainExpiresAt.IsZero() {
			extra = append(extra, "last known expiry "+date(ev.Result.DomainExpiresAt, ev.At))
		}
		if c := cause(ev.Result); c != "" {
			extra = append(extra, c)
		}
	case state.EventSummary, state.EventHeld:
		extra = append(extra, digest(ev.Summary)...)
	}
	if len(extra) == 0 {
		return head
	}
	return head + "\n" + strings.Join(extra, "\n")
}

// headline is the one sentence that has to survive on its own.
func headline(ev state.Event) string {
	name := ev.Check
	switch ev.Kind {
	case state.EventUp:
		if ev.Downtime > 0 {
			return name + " is back, down for " + fmtDuration(ev.Downtime)
		}
		return name + " is back"
	case state.EventStillDown:
		if ev.Downtime > 0 {
			return name + " still down, " + fmtDuration(ev.Downtime)
		}
		return name + " still down"
	case state.EventUnstable:
		if ev.Failures > 0 && ev.Window > 0 {
			return fmt.Sprintf("%s is flapping, %d of the last %d checks failed", name, ev.Failures, ev.Window)
		}
		return name + " is flapping"
	case state.EventDrift:
		return name + ": DNS answers changed"
	case state.EventExpiry:
		return name + " " + expiryPhrase(ev)
	case state.EventStale:
		s := name + ": registry has not answered"
		if ev.StaleFor > 0 {
			s += " for " + fmtDuration(ev.StaleFor)
		}
		return s
	case state.EventUndelegated:
		return name + " is no longer delegated"
	case state.EventDelegated:
		return name + " is delegated again"
	case state.EventSummary:
		if ev.Summary != nil {
			return plural(ev.Summary.Count, "alert") + " folded while delivery was down"
		}
		return "alerts folded while delivery was down"
	case state.EventHeld:
		n := 0
		if ev.Summary != nil {
			n = ev.Summary.Count
		}
		return fmt.Sprintf("mute ended, %s held for %s", plural(n, "alert"), muteScope(ev))
	}
	if ev.Result.Outcome == check.OutcomeMalformed {
		return name + " no longer answers as expected"
	}
	return name + " is down"
}

// line renders one event inside a batch: never more than one line, because
// twenty of them are read as a list.
func line(ev state.Event) string {
	name := ev.Check
	switch ev.Kind {
	case state.EventUp:
		if ev.Downtime > 0 {
			return name + " — back after " + fmtDuration(ev.Downtime)
		}
		return name + " — back"
	case state.EventStillDown:
		s := name + " — still down"
		if ev.Downtime > 0 {
			s += " " + fmtDuration(ev.Downtime)
		}
		return s
	case state.EventUnstable:
		if ev.Failures > 0 && ev.Window > 0 {
			return fmt.Sprintf("%s — flapping, %d of %d failed", name, ev.Failures, ev.Window)
		}
		return name + " — flapping"
	case state.EventDrift:
		return name + " — DNS answers changed"
	case state.EventExpiry:
		return name + " — " + expiryShort(ev)
	case state.EventStale:
		s := name + " — registry silent"
		if ev.StaleFor > 0 {
			s += " for " + fmtDuration(ev.StaleFor)
		}
		return s
	case state.EventUndelegated:
		return name + " — no longer delegated"
	case state.EventDelegated:
		return name + " — delegated again"
	case state.EventSummary, state.EventHeld:
		return headline(ev)
	}
	if c := cause(ev.Result); c != "" {
		return name + " — " + clip(oneLine(c), 120)
	}
	if ev.Result.Outcome == check.OutcomeMalformed {
		return name + " — response no longer matches"
	}
	return name + " — down"
}

// expiryPhrase says how long is left and until when, on one line, in that
// order: the number is what decides whether to act, the date is what goes in
// the calendar. Registry status codes and .ru release dates are left out —
// they are jargon that never changed anyone's next move.
func expiryPhrase(ev state.Event) string {
	x := ev.Expiry
	what := "certificate"
	if x != nil && x.Kind == state.ExpiryDomain {
		what = "registration"
	}
	if x == nil {
		return what + " is expiring"
	}
	var when string
	if !x.NotAfter.IsZero() {
		when = " (" + date(x.NotAfter, ev.At) + ")"
	}
	switch {
	case x.DaysLeft < 0:
		return what + " expired " + plural(-x.DaysLeft, "day") + " ago" + when
	case x.DaysLeft == 0:
		return what + " expires today" + when
	}
	return what + " expires in " + plural(x.DaysLeft, "day") + when
}

// expiryShort is the batch form: the headline already said what is expiring,
// so the line only carries how long is left and until when.
func expiryShort(ev state.Event) string {
	x := ev.Expiry
	if x == nil {
		return "expiring"
	}
	prefix := ""
	if x.Kind != state.ExpiryDomain {
		// A certificate and a registration expire on different clocks and
		// are renewed by different people; in a mixed list they must not
		// look alike.
		prefix = "cert: "
	}
	var when string
	if !x.NotAfter.IsZero() {
		when = " (" + date(x.NotAfter, ev.At) + ")"
	}
	switch {
	case x.DaysLeft < 0:
		return prefix + "expired " + plural(-x.DaysLeft, "day") + " ago" + when
	case x.DaysLeft == 0:
		return prefix + "expires today" + when
	}
	return prefix + plural(x.DaysLeft, "day") + " left" + when
}

// cause is the one line that says why a check failed, with how long the
// attempt took. Everything the old format spread over three labelled lines
// fits here, because the labels never carried information.
func cause(r check.Result) string {
	reason := oneLine(check.RedactSecrets(r.Reason()))
	dur := ""
	if d := r.Duration.Round(time.Millisecond); d > 0 {
		dur = " (" + fmtDuration(d) + ")"
	}
	switch {
	case reason != "":
		return clip(reason, 300) + dur
	case r.StatusCode > 0:
		return fmt.Sprintf("HTTP %d%s", r.StatusCode, dur)
	case r.Rcode != "":
		return "DNS " + r.Rcode + dur
	case dur != "":
		return "no response" + dur
	}
	return ""
}

func body(r check.Result) string {
	sample := oneLine(check.RedactSecrets(r.BodySample))
	if sample == "" || isMarkup(sample) {
		// A failing page answers with its whole HTML shell, and 200
		// characters of doctype and inline script say nothing about why
		// the check failed. The line above already carries the diagnosis;
		// an unreadable alert is one that gets ignored.
		return ""
	}
	return clip(sample, 300)
}

// digest lists what a mute or an overflow folded together.
func digest(s *state.Summary) []string {
	if s == nil {
		return nil
	}
	var out []string
	if line := countLine(s.ByKind); line != "" {
		out = append(out, line)
	}
	if len(s.Checks) > 0 {
		names := s.Checks
		const shown = 20
		if len(names) > shown {
			names = append(append([]string(nil), names[:shown]...), "…")
		}
		out = append(out, strings.Join(names, ", "))
	}
	return out
}

func muteScope(ev state.Event) string {
	switch {
	case ev.Check != "":
		return ev.Check
	case ev.Group != "":
		return "group " + ev.Group
	}
	return "all checks"
}

// isMarkup reports whether a body sample is a web page rather than an API
// answer. Only the opening bytes matter: that is all we keep.
func isMarkup(sample string) bool {
	head := strings.ToLower(strings.TrimSpace(sample))
	return strings.HasPrefix(head, "<!doctype") || strings.HasPrefix(head, "<html") ||
		strings.HasPrefix(head, "<?xml") || strings.HasPrefix(head, "<head")
}

// date is a human date: "30 Aug" for this year, "30 Aug 2027" for another.
// The clock time of an expiry has never mattered to anyone renewing one.
func date(t, now time.Time) string {
	t = t.UTC()
	if !now.IsZero() && t.Year() == now.UTC().Year() {
		return t.Format("2 Jan")
	}
	return t.Format("2 Jan 2006")
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// fmtDuration is a duration as someone would say it out loud: "900ms",
// "1.2s", "39m", "4h12m", "3d 4h". Go's own String() would print "4h12m0s",
// and the trailing zero is three characters of noise on every alert.
func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < 10*time.Second:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	case d < 24*time.Hour:
		d = d.Round(time.Minute)
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	d = d.Round(time.Hour)
	days, h := int(d.Hours())/24, int(d.Hours())%24
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd %dh", days, h)
}

func countLine(m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// fit puts as many event lines as will go under telegramMaxRunes. The header
// is kept even when lines are dropped, so a huge batch still reports how many
// checks moved.
func fit(header string, lines []string) string {
	var b strings.Builder
	b.WriteString(header)
	used := utf8.RuneCountInString(header)
	shown := 0
	for _, l := range lines {
		sep := "\n"
		if shown == 0 {
			sep = "\n\n"
		}
		need := utf8.RuneCountInString(sep) + utf8.RuneCountInString(l)
		omitted := len(lines) - shown
		note := ""
		if omitted > 1 {
			note = fmt.Sprintf("\n\n(%s not shown)", plural(omitted-1, "more event"))
		}
		if need+utf8.RuneCountInString(note) > telegramMaxRunes-used {
			b.WriteString(note)
			return b.String()
		}
		b.WriteString(sep)
		b.WriteString(l)
		used += need
		shown++
	}
	return b.String()
}

func clip(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	const suffix = "…"
	keep := max - utf8.RuneCountInString(suffix)
	if keep < 1 {
		return suffix
	}
	n := 0
	for i := range s {
		if n == keep {
			return s[:i] + suffix
		}
		n++
	}
	return s
}
