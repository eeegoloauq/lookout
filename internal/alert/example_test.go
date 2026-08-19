package alert_test

import (
	"fmt"
	"time"

	"github.com/eeegoloauq/lookout/internal/alert"
	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/state"
)

var at = time.Date(2026, 8, 19, 16, 21, 0, 0, time.UTC)

// One event is a sentence and the fact behind it. Nothing else fits on a
// phone's notification banner anyway.
func ExampleFormat_outage() {
	fmt.Println(alert.Format([]state.Event{{
		Kind: state.EventDown, Check: "Immich", Group: "Services", At: at,
		Result: check.Result{
			Outcome: check.OutcomeDown, StatusCode: 502, Duration: 1240 * time.Millisecond,
			Failures: []check.Failure{{Condition: "status", Want: "200-299", Got: "502"}},
		},
	}}))
	// Output:
	// 🔴 Immich is down
	// status: want 200-299, got 502 (1.2s)
}

// A batch is a list, worst first, one line each — the shape a gateway drop
// produces and the one that has to stay readable at twenty checks.
func ExampleFormat_batch() {
	fmt.Println(alert.Format([]state.Event{
		{Kind: state.EventDown, Check: "Immich", Group: "Services", At: at,
			Result: check.Result{StatusCode: 502, Duration: 900 * time.Millisecond,
				Failures: []check.Failure{{Condition: "status", Want: "200-299", Got: "502"}}}},
		{Kind: state.EventDown, Check: "Gateway", Group: "Services", At: at,
			Result: check.Result{Err: "context deadline exceeded", Duration: 5 * time.Second}},
		{Kind: state.EventUnstable, Check: "RAG (chat backend)", Group: "Public Sites", At: at,
			Failures: 6, Window: 20},
		{Kind: state.EventExpiry, Check: "Vaultwarden", Group: "Services", At: at,
			Expiry: &state.Expiry{Kind: state.ExpiryCertificate, DaysLeft: 7,
				NotAfter: time.Date(2026, 8, 26, 6, 0, 0, 0, time.UTC)}},
		{Kind: state.EventUp, Check: "Router", Group: "Core", At: at, Downtime: 3 * time.Minute},
	}))
	// Output:
	// 🔴 2 down, 1 flapping, 1 certificate expiring, 1 back up
	//
	// Immich — status: want 200-299, got 502 (900ms)
	// Gateway — context deadline exceeded (5s)
	// RAG (chat backend) — flapping, 6 of 20 failed
	// Vaultwarden — cert: 7 days left (26 Aug)
	// Router — back after 3m
}

// Renewal notices carry the two things that decide what to do: how long is
// left, and the date it runs out. Registry status codes and .ru release
// dates are jargon and stay out of the message.
func ExampleFormat_renewals() {
	fmt.Println(alert.Format([]state.Event{
		{Kind: state.EventExpiry, Check: "example.ru", Group: "Domains", At: at,
			Expiry: &state.Expiry{Kind: state.ExpiryDomain, DaysLeft: 11,
				NotAfter: time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC),
				FreeDate: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
				State:    "REGISTERED, DELEGATED, VERIFIED"}},
		{Kind: state.EventExpiry, Check: "example.dev", Group: "Domains", At: at,
			Expiry: &state.Expiry{Kind: state.ExpiryDomain, DaysLeft: 54,
				NotAfter: time.Date(2026, 10, 13, 10, 8, 0, 0, time.UTC),
				State:    "client transfer prohibited"}},
	}))
	// Output:
	// 🟡 2 domains expiring
	//
	// example.ru — 11 days left (30 Aug)
	// example.dev — 54 days left (13 Oct)
}

// A reminder repeats the outage with how long it has been open.
func ExampleFormat_reminder() {
	fmt.Println(alert.Format([]state.Event{{
		Kind: state.EventStillDown, Check: "Immich", At: at, Downtime: 4*time.Hour + 12*time.Minute,
		Result: check.Result{Err: "context deadline exceeded", Duration: 5 * time.Second},
	}}))
	// Output:
	// 🔴 Immich still down, 4h12m
	// context deadline exceeded (5s)
}
