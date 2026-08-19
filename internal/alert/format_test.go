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
	for _, want := range []string{
		"DOWN Photos (Services)",
		"status: want 200-299, got 503",
		"HTTP 503 in 1.2s",
		`body: {"error":"backend unavailable"}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("format missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "*") || strings.Contains(got, "`") {
		t.Errorf("message uses markdown: %q", got)
	}
}

func TestFormatUpReportsDowntime(t *testing.T) {
	ev := state.Event{
		Kind:     state.EventUp,
		Check:    "Photos",
		Group:    "Services",
		Downtime: 12*time.Minute + 34*time.Second,
		Result:   check.Result{Outcome: check.OutcomeUp, StatusCode: 200},
	}
	got := Format([]state.Event{ev})
	if !strings.Contains(got, "UP Photos (Services) after 12m34s") {
		t.Errorf("got %q", got)
	}
}

func TestFormatUnstableNamesTheWindow(t *testing.T) {
	ev := state.Event{
		Kind:     state.EventUnstable,
		Check:    "Photos",
		Group:    "Services",
		Failures: 5,
		Window:   20,
		Result: check.Result{
			Outcome:    check.OutcomeDown,
			StatusCode: 500,
			Duration:   80 * time.Millisecond,
			Failures:   []check.Failure{{Condition: "status", Want: "200-299", Got: "500"}},
		},
	}
	got := Format([]state.Event{ev})
	if !strings.Contains(got, "UNSTABLE Photos (Services)") {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(got, "5 failures in the last 20 checks") {
		t.Errorf("got %q", got)
	}
}

func TestFormatMalformedReadsDifferentlyFromDown(t *testing.T) {
	ev := state.Event{
		Kind:  state.EventDown,
		Check: "Photos",
		Group: "Services",
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
	}
	got := Format([]state.Event{ev})
	if !strings.Contains(got, "response no longer matches") {
		t.Errorf("malformed outage must not read as a plain down: %q", got)
	}
	if !strings.Contains(got, "field .res is missing") {
		t.Errorf("got %q", got)
	}
}

func TestFormatBatchGroupsByCheckGroup(t *testing.T) {
	events := []state.Event{
		{Kind: state.EventDown, Check: "Router", Group: "Core", Result: check.Result{StatusCode: 500, Failures: []check.Failure{{Condition: "status", Want: "200", Got: "500"}}}},
		{Kind: state.EventDown, Check: "Photos", Group: "Services", Result: check.Result{StatusCode: 503, Failures: []check.Failure{{Condition: "status", Want: "200", Got: "503"}}}},
		{Kind: state.EventDown, Check: "API", Group: "Services", Result: check.Result{StatusCode: 502, Failures: []check.Failure{{Condition: "status", Want: "200", Got: "502"}}}},
		{Kind: state.EventUp, Check: "DNS", Group: "Core", Downtime: time.Minute},
	}
	got := Format(events)
	if !strings.HasPrefix(got, "4 checks: Core 2, Services 2") {
		t.Errorf("header = first line of %q", got)
	}
	if strings.Count(got, "DOWN ") != 3 || !strings.Contains(got, "UP DNS") {
		t.Errorf("details missing:\n%s", got)
	}
}

func TestFormatRedactsAuthorizationInTheBody(t *testing.T) {
	ev := state.Event{
		Kind:  state.EventDown,
		Check: "API",
		Result: check.Result{
			StatusCode: 401,
			BodySample: "Authorization: Bearer super-secret-token\nnope",
			Failures:   []check.Failure{{Condition: "status", Want: "200", Got: "401"}},
		},
	}
	got := Format([]state.Event{ev})
	if strings.Contains(got, "super-secret-token") {
		t.Errorf("alert leaked a bearer token:\n%s", got)
	}
}

func TestFormatStaysUnderTelegramLimit(t *testing.T) {
	var events []state.Event
	for i := range 50 {
		events = append(events, state.Event{
			Kind:  state.EventDown,
			Check: fmt.Sprintf("C%02d%s", i, strings.Repeat("x", 18)),
			Group: "G",
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
	if !strings.Contains(got, "50 checks") {
		t.Errorf("truncated message lost the header counts:\n%s", got[:min(len(got), 200)])
	}
}

func TestFormatSummaryCountsWhatOverflowFolded(t *testing.T) {
	ev := state.Event{
		Kind: state.EventSummary,
		Summary: &state.Summary{
			Count:   12,
			From:    time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
			To:      time.Date(2026, 8, 19, 13, 0, 0, 0, time.UTC),
			ByKind:  map[string]int{"down": 7, "up": 5},
			ByGroup: map[string]int{"Core": 4, "Services": 8},
			Checks:  []string{"Photos", "Router"},
		},
	}
	got := Format([]state.Event{ev})
	for _, want := range []string{
		"12 alerts folded",
		"down 7, up 5",
		"Core 4, Services 8",
		"Photos, Router",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}
