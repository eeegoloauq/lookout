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

// Format renders a batch of events as one plain-text message, grouped by
// check group (SPEC §6, §7). No Markdown is used: parse_mode would break on
// an arbitrary response body that happens to contain * or `.
func Format(events []state.Event) string {
	if len(events) == 0 {
		return ""
	}
	header := ""
	if len(events) > 1 {
		header = batchHeader(events)
	}
	bodies := make([]string, len(events))
	for i, ev := range events {
		bodies[i] = formatEvent(ev)
	}
	return fit(header, bodies)
}

func batchHeader(events []state.Event) string {
	type key struct{ group, check string }
	seen := map[key]bool{}
	groups := map[string]int{}
	var extra int
	for _, ev := range events {
		if ev.Kind == state.EventSummary {
			if ev.Summary != nil {
				extra += ev.Summary.Count
			} else {
				extra++
			}
			continue
		}
		k := key{ev.Group, ev.Check}
		if seen[k] {
			continue
		}
		seen[k] = true
		g := ev.Group
		if g == "" {
			g = "other"
		}
		groups[g]++
	}
	names := make([]string, 0, len(groups))
	for g := range groups {
		names = append(names, g)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, g := range names {
		parts = append(parts, fmt.Sprintf("%s %d", g, groups[g]))
	}
	n := len(seen)
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%s: %s", plural(n, "check"), strings.Join(parts, ", ")))
	if extra > 0 {
		b.WriteString(fmt.Sprintf("\nplus %s collapsed while delivery was down", plural(extra, "earlier alert")))
	}
	return b.String()
}

func formatEvent(ev state.Event) string {
	switch ev.Kind {
	case state.EventUp:
		return formatUp(ev)
	case state.EventUnstable:
		return formatUnstable(ev)
	case state.EventSummary:
		return formatSummary(ev)
	default:
		return formatDown(ev)
	}
}

func formatDown(ev state.Event) string {
	var b strings.Builder
	b.WriteString(title("DOWN", ev.Check, ev.Group))
	r := ev.Result
	if r.Outcome == check.OutcomeMalformed {
		b.WriteString("\nresponse no longer matches")
	}
	writeCause(&b, r)
	writeHTTP(&b, r)
	writeBody(&b, r)
	return b.String()
}

func formatUp(ev state.Event) string {
	var b strings.Builder
	b.WriteString(title("UP", ev.Check, ev.Group))
	if ev.Downtime > 0 {
		b.WriteString(" after ")
		b.WriteString(fmtDuration(ev.Downtime))
	}
	return b.String()
}

func formatUnstable(ev state.Event) string {
	var b strings.Builder
	b.WriteString(title("UNSTABLE", ev.Check, ev.Group))
	if ev.Failures > 0 && ev.Window > 0 {
		fmt.Fprintf(&b, "\n%d failures in the last %s", ev.Failures, plural(ev.Window, "check"))
	}
	writeCause(&b, ev.Result)
	writeHTTP(&b, ev.Result)
	writeBody(&b, ev.Result)
	return b.String()
}

func formatSummary(ev state.Event) string {
	s := ev.Summary
	if s == nil {
		return "SUMMARY (empty)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "SUMMARY %s folded after the delivery queue filled", plural(s.Count, "alert"))
	if !s.From.IsZero() && !s.To.IsZero() {
		fmt.Fprintf(&b, "\nfrom %s to %s", s.From.UTC().Format("2006-01-02 15:04:05 UTC"), s.To.UTC().Format("2006-01-02 15:04:05 UTC"))
	}
	if line := countLine(s.ByKind); line != "" {
		b.WriteString("\n")
		b.WriteString(line)
	}
	if line := countLine(s.ByGroup); line != "" {
		b.WriteString("\n")
		b.WriteString(line)
	}
	if len(s.Checks) > 0 {
		names := s.Checks
		const shown = 20
		if len(names) > shown {
			names = append(append([]string(nil), names[:shown]...), "...")
		}
		b.WriteString("\nchecks: ")
		b.WriteString(strings.Join(names, ", "))
	}
	return b.String()
}

func writeCause(b *strings.Builder, r check.Result) {
	if r.Err != "" {
		b.WriteByte('\n')
		b.WriteString(check.RedactSecrets(r.Err))
	}
	reason := r.Reason()
	if reason == "" || reason == r.Err {
		return
	}
	b.WriteByte('\n')
	b.WriteString(check.RedactSecrets(reason))
}

func writeHTTP(b *strings.Builder, r check.Result) {
	dur := r.Duration.Round(time.Millisecond)
	b.WriteByte('\n')
	switch {
	case r.StatusCode > 0 && dur > 0:
		fmt.Fprintf(b, "HTTP %d in %s", r.StatusCode, dur)
	case r.StatusCode > 0:
		fmt.Fprintf(b, "HTTP %d", r.StatusCode)
	case dur > 0:
		fmt.Fprintf(b, "no HTTP response in %s", dur)
	default:
		b.WriteString("no HTTP response")
	}
}

func writeBody(b *strings.Builder, r check.Result) {
	sample := oneLine(check.RedactSecrets(r.BodySample))
	if sample == "" {
		return
	}
	b.WriteString("\nbody: ")
	b.WriteString(sample)
}

func title(kind, name, group string) string {
	if group != "" {
		return kind + " " + name + " (" + group + ")"
	}
	return kind + " " + name
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func fmtDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
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

// fit puts as many event bodies as will go under telegramMaxRunes. The
// header is kept even when details are dropped, so a huge batch still
// reports how many checks moved.
func fit(header string, bodies []string) string {
	if header == "" && len(bodies) == 1 {
		return clip(bodies[0], telegramMaxRunes)
	}
	var b strings.Builder
	used := 0
	if header != "" {
		b.WriteString(header)
		used = utf8.RuneCountInString(header)
	}
	shown := 0
	for _, body := range bodies {
		sep := "\n\n"
		if used == 0 {
			sep = ""
		}
		need := utf8.RuneCountInString(sep) + utf8.RuneCountInString(body)
		remain := telegramMaxRunes - used
		omitted := len(bodies) - shown
		note := ""
		if omitted > 1 {
			note = fmt.Sprintf("\n\n(%s not shown)", plural(omitted-1, "more event"))
		}
		noteN := utf8.RuneCountInString(note)
		if shown > 0 && need+noteN > remain {
			if note != "" {
				b.WriteString(note)
			}
			return b.String()
		}
		if need > remain {
			// First body is already too big: clip it rather than send nothing.
			avail := remain - utf8.RuneCountInString(sep)
			if avail < 16 {
				break
			}
			b.WriteString(sep)
			b.WriteString(clip(body, avail))
			shown++
			break
		}
		b.WriteString(sep)
		b.WriteString(body)
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
