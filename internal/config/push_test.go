package config

import (
	"strings"
	"testing"
	"time"
)

const pushMinimal = `
checks:
  - name: ZFS tank
    type: push
    expect_every: 10m
    token: 0123456789abcdef
`

// loadErr returns the errors a configuration produced, failing the test when
// it produced none: a validation test that passes because nothing was checked
// is worse than no test.
func loadErr(t *testing.T, src string) string {
	t.Helper()
	_, err := Load("config.yaml", []byte(src))
	if err == nil {
		t.Fatal("configuration was accepted, expected it to be refused")
	}
	return err.Error()
}

func TestPushCheckResolves(t *testing.T) {
	cfg := mustLoad(t, pushMinimal+"    grace: 2m\n")
	c := cfg.Checks[0]
	if c.Type != TypePush {
		t.Fatalf("type = %q, want %q", c.Type, TypePush)
	}
	if c.ExpectEvery != 10*time.Minute || c.Grace != 2*time.Minute {
		t.Errorf("deadline = %s + %s, want 10m + 2m", c.ExpectEvery, c.Grace)
	}
	if c.PushToken != "0123456789abcdef" {
		t.Errorf("token = %q, want the one in the config", c.PushToken)
	}
	// The deadline is read on a tick, so the tick has to exist and be
	// shorter than the deadline it watches.
	if c.Interval != PushTick || c.Interval >= c.ExpectEvery {
		t.Errorf("interval = %s, want %s and under the deadline", c.Interval, PushTick)
	}
	if !c.Alert {
		t.Error("a push check that says nothing about alerting must alert")
	}
}

// A deadline shorter than the tick would be judged by a clock coarser than
// itself, so the tick follows it down.
func TestPushTickFollowsAShortDeadline(t *testing.T) {
	cfg := mustLoad(t, `
checks:
  - name: Ticker
    type: push
    expect_every: 15s
    token: 0123456789abcdef
`)
	if got := cfg.Checks[0].Interval; got != 15*time.Second {
		t.Fatalf("interval = %s, want 15s", got)
	}
}

func TestPushGraceDefaultsToZeroAndAcceptsZero(t *testing.T) {
	cfg := mustLoad(t, pushMinimal)
	if cfg.Checks[0].Grace != 0 {
		t.Fatalf("grace = %s, want 0", cfg.Checks[0].Grace)
	}
	cfg = mustLoad(t, pushMinimal+"    grace: 0s\n")
	if cfg.Checks[0].Grace != 0 {
		t.Fatalf("explicit grace: 0s = %s, want 0", cfg.Checks[0].Grace)
	}
}

func TestPushTokenComesFromTheEnvironment(t *testing.T) {
	t.Setenv("LOOKOUT_PUSH_TOKEN_TEST", "sw0rdf1sh-sw0rdf1sh")
	cfg := mustLoad(t, `
checks:
  - name: ZFS tank
    type: push
    expect_every: 10m
    token: ${LOOKOUT_PUSH_TOKEN_TEST}
`)
	if cfg.Checks[0].PushToken != "sw0rdf1sh-sw0rdf1sh" {
		t.Fatalf("token = %q, want the expanded environment value", cfg.Checks[0].PushToken)
	}
}

func TestPushTokenIsRequired(t *testing.T) {
	msg := loadErr(t, `
checks:
  - name: ZFS tank
    type: push
    expect_every: 10m
`)
	if !strings.Contains(msg, "checks[0].token") || !strings.Contains(msg, "required") {
		t.Fatalf("missing token reported as %q", msg)
	}
}

func TestPushTokenMustBeUsable(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{"empty", `""`, "empty"},
		{"short", "abc123", "shortest accepted"},
		{"path separator", "aaaaaaaa/bbbbbbbb", "unreserved URL characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := loadErr(t, `
checks:
  - name: ZFS tank
    type: push
    expect_every: 10m
    token: `+tt.token+"\n")
			if !strings.Contains(msg, tt.want) {
				t.Fatalf("token %q reported as %q, want a message about %q", tt.token, msg, tt.want)
			}
			if strings.Contains(msg, "aaaaaaaa") {
				t.Fatalf("the error echoed the token: %q", msg)
			}
		})
	}
}

// Two checks behind one token cannot be told apart at the endpoint, so the
// second would silently answer for the first.
func TestPushTokensMustBeUnique(t *testing.T) {
	msg := loadErr(t, `
checks:
  - name: ZFS tank
    type: push
    expect_every: 10m
    token: 0123456789abcdef
  - name: Offsite copy
    type: push
    expect_every: 24h
    token: 0123456789abcdef
`)
	if !strings.Contains(msg, "checks[1].token") || !strings.Contains(msg, `"ZFS tank"`) {
		t.Fatalf("duplicate token reported as %q", msg)
	}
	if strings.Contains(msg, "0123456789abcdef") {
		t.Fatalf("the error echoed the token: %q", msg)
	}
}

func TestPushExpectEveryIsRequiredAndSane(t *testing.T) {
	msg := loadErr(t, `
checks:
  - name: ZFS tank
    type: push
    token: 0123456789abcdef
`)
	if !strings.Contains(msg, "checks[0].expect_every") || !strings.Contains(msg, "required") {
		t.Fatalf("missing expect_every reported as %q", msg)
	}
	msg = loadErr(t, `
checks:
  - name: ZFS tank
    type: push
    expect_every: 1s
    token: 0123456789abcdef
`)
	if !strings.Contains(msg, "expect_every") || !strings.Contains(msg, "liveness probe") {
		t.Fatalf("1s deadline reported as %q", msg)
	}
}

// Every field that belongs to a check lookout performs is meaningless on one
// it waits for, and a field that is silently ignored is a check nobody is
// running the way they think they are.
func TestPushRejectsFieldsItCannotHave(t *testing.T) {
	tests := []struct {
		field string
		line  string
		want  string
	}{
		{"interval", "    interval: 60s", "expect_every is the schedule"},
		{"timeout", "    timeout: 5s", "nothing is dialled"},
		{"url", "    url: http://example.invalid/", "it is pinged"},
		{"address", "    address: db.example:5432", `only valid on "tcp" checks`},
		{"host", "    host: service.example", "it is pinged"},
		{"query_type", "    query_type: A", `only valid on "dns" checks`},
		{"headers", "    headers: {Authorization: none}", `only valid on "http" checks`},
		{"expect", "    expect:\n      response_time: \"<1s\"", "the whole condition"},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			msg := loadErr(t, pushMinimal+tt.line+"\n")
			if !strings.Contains(msg, "checks[0]."+tt.field) || !strings.Contains(msg, tt.want) {
				t.Fatalf("%s on a push check reported as %q", tt.field, msg)
			}
		})
	}
}

// The push fields are as wrong on the other four types as their fields are
// on a push check.
func TestPushFieldsRejectedOnOtherTypes(t *testing.T) {
	msg := loadErr(t, `
checks:
  - name: Example
    type: http
    url: http://example.invalid
    expect_every: 10m
    grace: 1m
    token: 0123456789abcdef
`)
	for _, want := range []string{"checks[0].expect_every", "checks[0].grace", "checks[0].token"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%s not reported on an http check: %q", want, msg)
		}
	}
}

// A type nobody recognises must name the ones that exist, push included.
func TestUnknownTypeNamesPush(t *testing.T) {
	msg := loadErr(t, `
checks:
  - name: Example
    type: ping
    url: http://example.invalid
`)
	if !strings.Contains(msg, `"push"`) {
		t.Fatalf("unknown type message does not offer push: %q", msg)
	}
}
