package alert

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/state"
)

func TestFormatDownNamesTheFailingCondition(t *testing.T) {
	ev := state.Event{
		Kind:  state.EventDown,
		Check: "Photos",
		Group: "Services",
		At:    time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
		Alert: true,
		Result: check.Result{
			Outcome:    check.OutcomeDown,
			StatusCode: 503,
			Duration:   1200 * time.Millisecond,
			Failures:   []check.Failure{{Condition: "status", Want: "200-299", Got: "503"}},
			BodySample: `{"error":"backend unavailable"}`,
		},
	}
	got := Format([]state.Event{ev})
	want := "\U0001F534 Photos is down\n" +
		"status: want 200-299, got 503 (1.2s)\n" +
		`body sample`
	_ = want
	for _, w := range []string{
		"\U0001F534 Photos is down",
		"status: want 200-299, got 503 (1.2s)",
		`{"error":"backend unavailable"}`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("format missing %q\n%s", w, got)
		}
	}
	if strings.Contains(got, "*") || strings.Contains(got, "`") {
		t.Errorf("message uses markdown: %q", got)
	}
}

// The group is dropped from alerts on purpose: check names are unique, and
// "DOWN Photos (Services)" under a "Services 1" header said the same thing
// three times.
func TestAlertsDoNotRepeatTheGroup(t *testing.T) {
	got := Format([]state.Event{{
		Kind: state.EventDown, Check: "Photos", Group: "Services",
		Result: check.Result{StatusCode: 503, Failures: []check.Failure{{Condition: "status", Want: "200", Got: "503"}}},
	}})
	if strings.Contains(got, "Services") {
		t.Errorf("group leaked into the alert: %q", got)
	}
}

// One emoji, at the very start. Not one per line, and never two.
func TestMessageCarriesExactlyOneEmoji(t *testing.T) {
	events := []state.Event{
		{Kind: state.EventDown, Check: "Photos", Result: check.Result{StatusCode: 503}},
		{Kind: state.EventUp, Check: "Router", Downtime: time.Minute},
		{Kind: state.EventExpiry, Check: "example.com", Expiry: &state.Expiry{Kind: state.ExpiryDomain, DaysLeft: 11}},
	}
	for _, msg := range []string{Format(events[:1]), Format(events)} {
		if n := countEmoji(msg); n != 1 {
			t.Errorf("message carries %d emoji, want 1:\n%s", n, msg)
		}
		if r, _ := utf8.DecodeRuneInString(msg); !isEmoji(r) {
			t.Errorf("message does not open with the emoji:\n%s", msg)
		}
	}
}

func countEmoji(s string) int {
	n := 0
	for _, r := range s {
		if isEmoji(r) {
			n++
		}
	}
	return n
}

func isEmoji(r rune) bool {
	return r == '✅' || (r >= 0x1F300 && r <= 0x1FAFF) || (r >= 0x2600 && r <= 0x27BF)
}

func TestFormatUpReportsDowntime(t *testing.T) {
	got := Format([]state.Event{{
		Kind:     state.EventUp,
		Check:    "Photos",
		Downtime: 12*time.Minute + 34*time.Second,
		Result:   check.Result{Outcome: check.OutcomeUp, StatusCode: 200},
	}})
	if !strings.Contains(got, "Photos is back, down for 13m") {
		t.Errorf("got %q", got)
	}
}

func TestFormatStillDownRepeatsTheOutage(t *testing.T) {
	got := Format([]state.Event{{
		Kind:     state.EventStillDown,
		Check:    "Photos",
		Downtime: 4*time.Hour + 12*time.Minute,
		Result:   check.Result{Outcome: check.OutcomeDown, Err: "dial tcp: connection refused"},
	}})
	if !strings.Contains(got, "Photos still down, 4h12m") {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("reminder dropped the cause: %q", got)
	}
}

func TestFormatUnstableNamesTheWindow(t *testing.T) {
	got := Format([]state.Event{{
		Kind:     state.EventUnstable,
		Check:    "Photos",
		Failures: 5,
		Window:   20,
		Result: check.Result{
			Outcome:    check.OutcomeDown,
			StatusCode: 500,
			Duration:   80 * time.Millisecond,
			Failures:   []check.Failure{{Condition: "status", Want: "200-299", Got: "500"}},
		},
	}})
	if !strings.Contains(got, "Photos is flapping, 5 of the last 20 checks failed") {
		t.Errorf("got %q", got)
	}
}

func TestFormatMalformedReadsDifferentlyFromDown(t *testing.T) {
	got := Format([]state.Event{{
		Kind:  state.EventDown,
		Check: "Photos",
		Result: check.Result{
			Outcome:    check.OutcomeMalformed,
			StatusCode: 200,
			Duration:   50 * time.Millisecond,
			Failures: []check.Failure{{
				Condition: "body .res",
				Want:      `"pong"`,
				Got:       "field .res is missing from the response",
				Malformed: true,
			}},
		},
	}})
	if !strings.Contains(got, "no longer answers as expected") {
		t.Errorf("malformed outage must not read as a plain down: %q", got)
	}
	if !strings.Contains(got, "field .res is missing") {
		t.Errorf("got %q", got)
	}
	if strings.HasPrefix(got, "\U0001F534") {
		t.Errorf("a changed API is not an outage-red message: %q", got)
	}
}

// A batch is a list: one counting headline, then one line per event, worst
// first. Twenty paragraphs are not read; twenty lines are.
func TestFormatBatchIsOneLinePerEvent(t *testing.T) {
	events := []state.Event{
		{Kind: state.EventUp, Check: "DNS", Downtime: time.Minute},
		{Kind: state.EventDown, Check: "Router", Result: check.Result{StatusCode: 500, Failures: []check.Failure{{Condition: "status", Want: "200", Got: "500"}}}},
		{Kind: state.EventDown, Check: "Photos", Result: check.Result{StatusCode: 503, Failures: []check.Failure{{Condition: "status", Want: "200", Got: "503"}}}},
		{Kind: state.EventDown, Check: "API", Result: check.Result{StatusCode: 502, Failures: []check.Failure{{Condition: "status", Want: "200", Got: "502"}}}},
	}
	got := Format(events)
	lines := strings.Split(got, "\n")
	if lines[0] != "\U0001F534 3 down, 1 back up" {
		t.Errorf("headline = %q\n%s", lines[0], got)
	}
	if lines[1] != "" {
		t.Errorf("headline is not followed by a blank line:\n%s", got)
	}
	if n := len(lines); n != 6 {
		t.Errorf("4 events took %d lines, want 6 (headline, blank, four):\n%s", n, got)
	}
	if !strings.Contains(got, "DNS — back after 1m") {
		t.Errorf("recovery line missing:\n%s", got)
	}
	if strings.Index(got, "DNS —") < strings.Index(got, "Router —") {
		t.Errorf("recovery must sort below the outages:\n%s", got)
	}
}

func TestFormatRedactsAuthorizationInTheBody(t *testing.T) {
	got := Format([]state.Event{{
		Kind:  state.EventDown,
		Check: "API",
		Result: check.Result{
			StatusCode: 401,
			BodySample: "Authorization: Bearer super-secret-token\nnope",
			Failures:   []check.Failure{{Condition: "status", Want: "200", Got: "401"}},
		},
	}})
	if strings.Contains(got, "super-secret-token") {
		t.Errorf("alert leaked a bearer token:\n%s", got)
	}
}

func TestFormatStaysUnderTelegramLimit(t *testing.T) {
	var events []state.Event
	for i := range 200 {
		events = append(events, state.Event{
			Kind:  state.EventDown,
			Check: fmt.Sprintf("C%03d%s", i, strings.Repeat("x", 18)),
			Result: check.Result{
				StatusCode: 500,
				BodySample: strings.Repeat("b", 200),
				Failures:   []check.Failure{{Condition: "status", Want: "200", Got: "500"}},
			},
		})
	}
	got := Format(events)
	if n := utf8.RuneCountInString(got); n > telegramMaxRunes {
		t.Errorf("message is %d runes, over the %d Telegram limit", n, telegramMaxRunes)
	}
	if !strings.HasPrefix(got, "\U0001F534 200 down") {
		t.Errorf("truncated message lost the headline count:\n%s", got[:min(len(got), 200)])
	}
	if !strings.Contains(got, "not shown") {
		t.Errorf("truncation must say how much was dropped:\n%s", got[len(got)-200:])
	}
}

func TestFormatSummaryCountsWhatOverflowFolded(t *testing.T) {
	got := Format([]state.Event{{
		Kind: state.EventSummary,
		Summary: &state.Summary{
			Count:   12,
			From:    time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
			To:      time.Date(2026, 8, 19, 13, 0, 0, 0, time.UTC),
			ByKind:  map[string]int{"down": 7, "up": 5},
			ByGroup: map[string]int{"Core": 4, "Services": 8},
			Checks:  []string{"Photos", "Router"},
		},
	}})
	for _, want := range []string{
		"12 alerts folded while delivery was down",
		"down 7, up 5",
		"Photos, Router",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

// Days left decides whether to act; the date is what goes in the calendar.
// Registry status codes and .ru release dates are jargon and stay out.
func TestFormatExpiryIsOneReadableLine(t *testing.T) {
	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	got := Format([]state.Event{{
		Kind:  state.EventExpiry,
		Check: "example.ru",
		Group: "Domains",
		At:    at,
		Expiry: &state.Expiry{
			Kind:      state.ExpiryDomain,
			DaysLeft:  11,
			Threshold: 14,
			NotAfter:  time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC),
			FreeDate:  time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
			State:     "REGISTERED, DELEGATED, VERIFIED",
		},
	}})
	if got != "\U0001F7E1 example.ru registration expires in 11 days (30 Aug)" {
		t.Errorf("got %q", got)
	}
}

func TestFormatCertExpiryKeepsTheYearWhenItDiffers(t *testing.T) {
	got := Format([]state.Event{{
		Kind:  state.EventExpiry,
		Check: "Vaultwarden",
		At:    time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC),
		Expiry: &state.Expiry{
			Kind:     state.ExpiryCertificate,
			DaysLeft: 21,
			NotAfter: time.Date(2027, 1, 10, 6, 0, 0, 0, time.UTC),
		},
	}})
	if !strings.Contains(got, "certificate expires in 21 days (10 Jan 2027)") {
		t.Errorf("got %q", got)
	}
}

func TestFormatExpiredReadsAsAnOutage(t *testing.T) {
	got := Format([]state.Event{{
		Kind:   state.EventExpiry,
		Check:  "example.com",
		Expiry: &state.Expiry{Kind: state.ExpiryDomain, DaysLeft: -2},
	}})
	if !strings.HasPrefix(got, "\U0001F534") {
		t.Errorf("an expired domain is not a yellow notice: %q", got)
	}
	if !strings.Contains(got, "expired 2 days ago") {
		t.Errorf("got %q", got)
	}
}

func TestFormatDriftShowsBeforeAndAfter(t *testing.T) {
	got := Format([]state.Event{{
		Kind:  state.EventDrift,
		Check: "MX",
		Drift: &state.Drift{Before: "NOERROR\nMX 10 mail.service.example.", After: "NXDOMAIN"},
	}})
	if !strings.Contains(got, "was:") || !strings.Contains(got, "now: NXDOMAIN") {
		t.Errorf("got %q", got)
	}
}

func TestFormatHeldNamesTheWindow(t *testing.T) {
	got := Format([]state.Event{{
		Kind:  state.EventHeld,
		Group: "Public",
		Summary: &state.Summary{
			Count:  3,
			ByKind: map[string]int{"down": 2, "up": 1},
			Checks: []string{"MX", "NS"},
		},
	}})
	for _, want := range []string{
		"mute ended, 3 alerts held for group Public",
		"down 2, up 1",
		"MX, NS",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

// The registry state is the whole content of an undelegated alert: it says
// whether the domain was switched off for non-payment or by someone else.
func TestFormatUndelegatedKeepsTheState(t *testing.T) {
	got := Format([]state.Event{{
		Kind:  state.EventUndelegated,
		Check: "service.example",
		At:    time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC),
		Result: check.Result{
			DomainState:     "REGISTERED, VERIFIED",
			DomainExpiresAt: time.Date(2027, 4, 30, 21, 0, 0, 0, time.UTC),
		},
	}})
	for _, want := range []string{
		"service.example is no longer delegated",
		"registry: REGISTERED, VERIFIED",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestMessagesOpenWithTheirSeverity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event state.Event
		want  string
	}{
		{"down", state.Event{Kind: state.EventDown, Check: "Photos"}, "\U0001F534 Photos is down"},
		{"still down", state.Event{Kind: state.EventStillDown, Check: "Photos", Downtime: time.Hour}, "\U0001F534 Photos still down"},
		{"up", state.Event{Kind: state.EventUp, Check: "Photos"}, "✅ Photos is back"},
		{"unstable", state.Event{Kind: state.EventUnstable, Check: "Photos"}, "\U0001F7E0 Photos is flapping"},
		{"drift", state.Event{Kind: state.EventDrift, Check: "MX"}, "\U0001F7E0 MX: DNS answers changed"},
		{"cert", state.Event{Kind: state.EventExpiry, Check: "API", Expiry: &state.Expiry{Kind: state.ExpiryCertificate, DaysLeft: 14}}, "\U0001F7E1 API certificate expires in 14 days"},
		{"domain", state.Event{Kind: state.EventExpiry, Check: "Reg", Expiry: &state.Expiry{Kind: state.ExpiryDomain, DaysLeft: 30}}, "\U0001F7E1 Reg registration expires in 30 days"},
		{"stale", state.Event{Kind: state.EventStale, Check: "Reg", StaleFor: 72 * time.Hour}, "\U0001F7E1 Reg: registry has not answered for 3d"},
		{"undelegated", state.Event{Kind: state.EventUndelegated, Check: "Reg"}, "\U0001F534 Reg is no longer delegated"},
		{"delegated", state.Event{Kind: state.EventDelegated, Check: "Reg"}, "✅ Reg is delegated again"},
		{"held", state.Event{Kind: state.EventHeld, Group: "Public", Summary: &state.Summary{Count: 3}}, "\U0001F515 mute ended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Format([]state.Event{tc.event})
			if !strings.HasPrefix(got, tc.want) {
				t.Fatalf("message does not start with %q:\n%s", tc.want, got)
			}
		})
	}
}

// A failing web page answers with its HTML shell. Two hundred characters of
// doctype and inline script crowd out the line that says what went wrong.
func TestHTMLBodiesAreLeftOutOfAlerts(t *testing.T) {
	got := Format([]state.Event{{Kind: state.EventDown, Check: "Site", Alert: true,
		Result: check.Result{Name: "Site", StatusCode: 404,
			BodySample: `<!DOCTYPE html><html lang="ru"><head><meta charset="utf-8"><script>var a=1;</script>`}}})
	if strings.Contains(got, "DOCTYPE") {
		t.Fatalf("HTML leaked into the alert:\n%s", got)
	}
	if !strings.Contains(got, "404") {
		t.Fatalf("the status code must survive:\n%s", got)
	}
}
