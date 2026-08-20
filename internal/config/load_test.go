package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const minimal = `
checks:
  - name: Example
    type: http
    url: http://example.invalid:8080
`

func mustLoad(t *testing.T, src string) *Config {
	t.Helper()
	cfg, err := Load("config.yaml", []byte(src))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// A check that says nothing about alerting must alert. This is the inverted
// default of SPEC §1.1 and the reason lookout exists, so it is tested first.
func TestAlertingDefaultsToOn(t *testing.T) {
	cfg := mustLoad(t, minimal)
	if !cfg.Checks[0].Alert {
		t.Fatal("a check without an explicit alert: must alert, got alert=false")
	}
}

func TestAlertingCanBeDisabledExplicitly(t *testing.T) {
	cfg := mustLoad(t, minimal+"    alert: false\n")
	if cfg.Checks[0].Alert {
		t.Fatal("alert: false must disable alerting")
	}
}

func TestDefaultsBlockAppliesAndCheckOverrides(t *testing.T) {
	cfg := mustLoad(t, `
defaults:
  interval: 30s
  timeout: 3s
  failure_threshold: 4
  instability: {failures: 2, window: 10, cooldown: 15m}
  alert: false
checks:
  - name: A
    type: http
    url: http://a.invalid
  - name: B
    type: http
    url: http://b.invalid
    interval: 10s
    alert: true
`)
	a, b := cfg.Checks[0], cfg.Checks[1]
	if a.Interval != 30*time.Second || a.Timeout != 3*time.Second {
		t.Errorf("defaults not applied: %+v", a)
	}
	if a.FailureThreshold != 4 || a.SuccessThreshold != DefaultSuccessThreshold {
		t.Errorf("threshold defaults wrong: %+v", a)
	}
	if a.Instability != (Instability{Failures: 2, Window: 10, Cooldown: 15 * time.Minute}) {
		t.Errorf("instability defaults wrong: %+v", a.Instability)
	}
	if a.Alert {
		t.Error("defaults.alert: false must apply to checks that say nothing")
	}
	if b.Interval != 10*time.Second || !b.Alert {
		t.Errorf("check overrides not applied: %+v", b)
	}
	if b.Method != DefaultMethod {
		t.Errorf("method = %q, want %q", b.Method, DefaultMethod)
	}
}

func TestStatusDefaultsToSuccessRange(t *testing.T) {
	cfg := mustLoad(t, minimal)
	m := cfg.Checks[0].Expect.Status
	if !m.Match(200) || !m.Match(299) || m.Match(500) || m.Match(199) {
		t.Fatalf("default status matcher %q behaves wrong", m)
	}
}

func TestStatusMatcherForms(t *testing.T) {
	tests := []struct {
		yaml  string
		match map[int]bool
	}{
		{"status: 204", map[int]bool{204: true, 200: false}},
		{`status: "200-299"`, map[int]bool{200: true, 250: true, 300: false}},
		{`status: "<500"`, map[int]bool{499: true, 500: false}},
		{`status: ">=200"`, map[int]bool{200: true, 199: false}},
	}
	for _, tc := range tests {
		t.Run(tc.yaml, func(t *testing.T) {
			cfg := mustLoad(t, minimal+"    expect:\n      "+tc.yaml+"\n")
			m := cfg.Checks[0].Expect.Status
			for code, want := range tc.match {
				if got := m.Match(code); got != want {
					t.Errorf("%q.Match(%d) = %v, want %v", m, code, got, want)
				}
			}
		})
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("LOOKOUT_TEST_TOKEN", "s3cret")
	cfg := mustLoad(t, minimal+"    headers:\n      Authorization: \"Basic ${LOOKOUT_TEST_TOKEN}\"\n")
	if got := cfg.Checks[0].Headers["Authorization"]; got != "Basic s3cret" {
		t.Fatalf("Authorization = %q, want %q", got, "Basic s3cret")
	}
}

func TestBodyExpectations(t *testing.T) {
	cfg := mustLoad(t, minimal+`    expect:
      body:
        ".result.source.online": true
        ".items[0].name": "primary"
        ".count": 3
`)
	body := cfg.Checks[0].Expect.Body
	if len(body) != 3 {
		t.Fatalf("got %d body conditions, want 3", len(body))
	}
	// Order follows the document, so error messages are reproducible.
	if body[0].Path.String() != ".result.source.online" || body[0].Want != true {
		t.Errorf("body[0] = %+v", body[0])
	}
	if body[2].Want != float64(3) {
		t.Errorf("numbers must normalise to float64, got %T", body[2].Want)
	}
}

// Validation must report every problem in one pass, each with the line it is on.
func TestValidateReportsAllErrorsWithPositions(t *testing.T) {
	src := `
defaults:
  interval: 60s
  timeout: 5s
checks:
  - name: ""
    type: http
    url: not-a-url
  - name: Bad
    type: ftp
    url: http://example.invalid
    timeout: 90s
    expect:
      status: "999"
      response_time: "5s"
      body:
        "result.online": true
`
	_, err := Load("config.yaml", []byte(src))
	var errs Errors
	if !errors.As(err, &errs) {
		t.Fatalf("want Errors, got %T: %v", err, err)
	}
	want := map[int]string{
		6:  "name is required",
		8:  "no scheme",
		10: "unknown check type",
		12: "must be shorter than interval",
		14: "outside the HTTP range",
		15: "must start with a comparison",
		17: `must start with "." or "["`,
	}
	got := map[int]string{}
	for _, e := range errs {
		got[e.Line] = e.Msg
	}
	for line, substr := range want {
		msg, ok := got[line]
		if !ok {
			t.Errorf("no error reported on line %d (want %q); got: %v", line, substr, errs)
			continue
		}
		if !strings.Contains(msg, substr) {
			t.Errorf("line %d: message %q does not contain %q", line, msg, substr)
		}
	}
	if len(errs) != len(want) {
		t.Errorf("got %d errors, want %d:\n%v", len(errs), len(want), errs)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"no checks", "checks: []\n", "no checks defined"},
		{"missing type", "checks:\n  - name: A\n    url: http://a.invalid\n", "type is required"},
		{"missing url", "checks:\n  - name: A\n    type: http\n", "url is required"},
		{"dns missing host", "checks:\n  - name: A\n    type: dns\n    query_type: A\n", "host is required"},
		{"dns missing query type", "checks:\n  - name: A\n    type: dns\n    host: service.example\n", "query_type is required"},
		{"dns bad query type", "checks:\n  - name: A\n    type: dns\n    host: service.example\n    query_type: PTR\n", "unknown query_type"},
		{"dns url not allowed", "checks:\n  - name: A\n    type: dns\n    host: service.example\n    query_type: A\n    url: http://service.example\n", "url is for"},
		{"domain missing name", "checks:\n  - name: A\n    type: domain\n", "domain is required"},
		{"domain too frequent", "checks:\n  - name: A\n    type: domain\n    domain: service.example\n    interval: 5m\n", "once per hour"},
		{"host not fqdn", "checks:\n  - name: A\n    type: dns\n    host: localhost\n    query_type: A\n", "FQDN"},
		{"unknown field", minimal + "    intervals: 5s\n", "unknown field"},
		{"duplicate key", minimal + "    url: http://b.invalid\n", "already defined"},
		{"bad duration", minimal + "    interval: 5 seconds\n", "is not a duration"},
		{"unset env var", minimal + "    headers: {X-Token: \"${LOOKOUT_UNSET_VAR_FOR_TEST}\"}\n", "is not set"},
		{"bad method", minimal + "    method: FETCH\n", "unknown HTTP method"},
		{"window too wide", minimal + "    instability: {window: 100}\n", "between 1 and 64"},
		{"failures above window", minimal + "    instability: {failures: 30, window: 20}\n", "cannot exceed window"},
		{"threshold below one", minimal + "    failure_threshold: 0\n", "at least 1"},
		{"response_time above timeout", minimal + "    expect: {response_time: \"<10s\"}\n", "can never fail"},
		{"empty body_contains", minimal + "    expect: {body_contains: \"\"}\n", "would match every response"},
		{"duplicate check names", minimal + "  - name: Example\n    type: http\n    url: http://b.invalid\n", "duplicate check name"},
		{"syntax error", "checks:\n  - name: [unclosed\n", "sequence"},
		{"telegram token in file", minimal + "alerting:\n  telegram:\n    token: secret\n", "environment variable"},
		{"telegram chat_id in file", minimal + "alerting:\n  telegram:\n    chat_id: \"1\"\n", "environment variable"},
		{"http proxy for telegram", minimal + "alerting:\n  telegram:\n    proxy: http://proxy.example:8080\n", "socks5"},
		{"proxy without host", minimal + "alerting:\n  telegram:\n    proxy: socks5://\n", "has no host"},
		{"listen without port", "listen: 127.0.0.1\n" + minimal, "host:port"},
		{"listen empty", "listen: \"\"\n" + minimal, "empty"},
		{"listen bad port", "listen: 127.0.0.1:notaport\n" + minimal, "TCP port"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load("config.yaml", []byte(tc.src))
			if err == nil {
				t.Fatalf("want an error containing %q, got none", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
			var errs Errors
			if errors.As(err, &errs) {
				for _, e := range errs {
					if e.Line == 0 {
						t.Errorf("error without a line number: %v", e)
					}
				}
			}
		})
	}
}

func TestListenDefaultsToLoopback(t *testing.T) {
	cfg := mustLoad(t, minimal)
	if cfg.Listen != DefaultListen {
		t.Errorf("listen = %q, want the loopback default %q", cfg.Listen, DefaultListen)
	}
	if !strings.HasPrefix(cfg.Listen, "127.0.0.1:") {
		t.Errorf("listen = %q, default must be loopback-only", cfg.Listen)
	}
}

func TestListenOverride(t *testing.T) {
	cfg := mustLoad(t, "listen: 127.0.0.1:9090\n"+minimal)
	if cfg.Listen != "127.0.0.1:9090" {
		t.Errorf("listen = %q, want the configured address", cfg.Listen)
	}
}

func TestAlertingDefaultsAndOverrides(t *testing.T) {
	cfg := mustLoad(t, minimal)
	if cfg.Alerting.BatchWindow != DefaultBatchWindow {
		t.Errorf("batch_window = %s, want %s", cfg.Alerting.BatchWindow, DefaultBatchWindow)
	}
	if cfg.Alerting.Telegram.Proxy != "" {
		t.Errorf("proxy = %q, want empty (direct)", cfg.Alerting.Telegram.Proxy)
	}
	if got := cfg.Alerting.Reminders; len(got) != 3 || got[0] != time.Hour {
		t.Errorf("reminders = %v, want the 1h/4h/24h default", got)
	}

	cfg = mustLoad(t, minimal+`
alerting:
  batch_window: 30s
  telegram:
    proxy: socks5://proxy.example:1080
`)
	if cfg.Alerting.BatchWindow != 30*time.Second {
		t.Errorf("batch_window = %s, want 30s", cfg.Alerting.BatchWindow)
	}
	if cfg.Alerting.Telegram.Proxy != "socks5://proxy.example:1080" {
		t.Errorf("proxy = %q", cfg.Alerting.Telegram.Proxy)
	}
}

// Alerting is on unless the config says otherwise in so many words. The
// point is that a monitor which notifies nobody is a deliberate choice and
// never the result of an absent section or a missing environment variable.
// An open outage repeats on this schedule; the last step repeats forever.
// An explicitly empty list is how an operator asks for state changes only.
func TestRemindersOverrideAndOptOut(t *testing.T) {
	cfg := mustLoad(t, minimal+"\nalerting:\n  reminders: [30m, 6h]\n")
	want := []time.Duration{30 * time.Minute, 6 * time.Hour}
	if got := cfg.Alerting.Reminders; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("reminders = %v, want %v", got, want)
	}
	cfg = mustLoad(t, minimal+"\nalerting:\n  reminders: []\n")
	if got := cfg.Alerting.Reminders; len(got) != 0 {
		t.Errorf("reminders = %v, want none", got)
	}
}

// A gap shorter than a probe interval would page on every result.
func TestRemindersRejectAPagingGap(t *testing.T) {
	_, err := Load("config.yaml", []byte(minimal+"\nalerting:\n  reminders: [5s]\n"))
	if err == nil || !strings.Contains(err.Error(), "alerting.reminders[0]") {
		t.Fatalf("err = %v, want a complaint about the gap", err)
	}
}

// A still-alive message that fires because nobody wrote the key is how
// Gatus-style defaults go silent-wrong in the other direction: the
// operator did not ask to be paged weekly.
func TestHeartbeatDefaultsToOff(t *testing.T) {
	if got := mustLoad(t, minimal).Alerting.Heartbeat; got != 0 {
		t.Errorf("heartbeat = %s, want off", got)
	}
}

// 168h is the weekly cadence SPEC §12 names; the parser has to accept
// a duration that long, not just the short forms batch_window uses.
func TestHeartbeatOverride(t *testing.T) {
	cfg := mustLoad(t, minimal+"\nalerting:\n  heartbeat: 168h\n")
	if cfg.Alerting.Heartbeat != 168*time.Hour {
		t.Errorf("heartbeat = %s, want 168h", cfg.Alerting.Heartbeat)
	}
}

// An explicit 0 is how to keep the key in the file and still mean off.
func TestHeartbeatZeroTurnsItOff(t *testing.T) {
	cfg := mustLoad(t, minimal+"\nalerting:\n  heartbeat: 0s\n")
	if cfg.Alerting.Heartbeat != 0 {
		t.Errorf("heartbeat = %s, want off", cfg.Alerting.Heartbeat)
	}
}

// YAML 0 is an integer, not "0s". Rejecting it would make the documented
// "0 means off" fail for the spelling an operator actually writes.
func TestHeartbeatBareZeroTurnsItOff(t *testing.T) {
	cfg := mustLoad(t, minimal+"\nalerting:\n  heartbeat: 0\n")
	if cfg.Alerting.Heartbeat != 0 {
		t.Errorf("heartbeat = %s, want off", cfg.Alerting.Heartbeat)
	}
}

// A typo must fail at validate, not become a silently disabled deadman.
func TestHeartbeatRejectsGarbageTheWayBatchWindowDoes(t *testing.T) {
	_, err := Load("config.yaml", []byte(minimal+"\nalerting:\n  heartbeat: tomorrow\n"))
	if err == nil || !strings.Contains(err.Error(), "alerting.heartbeat") || !strings.Contains(err.Error(), "is not a duration") {
		t.Fatalf("err = %v, want the same duration complaint batch_window uses", err)
	}
}

// A negative interval cannot mean "off" or "soon": those are 0 and a
// positive duration, and mixing them in would make the next ping undated.
func TestHeartbeatRejectsANegativeInterval(t *testing.T) {
	_, err := Load("config.yaml", []byte(minimal+"\nalerting:\n  heartbeat: -1h\n"))
	if err == nil || !strings.Contains(err.Error(), "alerting.heartbeat") {
		t.Fatalf("err = %v, want a complaint about the negative interval", err)
	}
}

func TestAlertingModeDefaultsToTelegram(t *testing.T) {
	if got := mustLoad(t, minimal).Alerting.Mode; got != ModeTelegram {
		t.Errorf("mode = %q, want %q", got, ModeTelegram)
	}
}

func TestAlertingCanBeTurnedOffByName(t *testing.T) {
	cfg := mustLoad(t, minimal+"\nalerting:\n  mode: none\n")
	if cfg.Alerting.Mode != ModeNone {
		t.Errorf("mode = %q, want %q", cfg.Alerting.Mode, ModeNone)
	}
}

func TestAlertingModeRejectsNonsenseAndHalfConfiguration(t *testing.T) {
	for name, src := range map[string]string{
		"unknown mode":        minimal + "\nalerting:\n  mode: carrier-pigeon\n",
		"none with transport": minimal + "\nalerting:\n  mode: none\n  telegram:\n    proxy: socks5://proxy.example:1080\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load("test.yaml", []byte(src)); err == nil {
				t.Fatal("want an error, got none")
			}
		})
	}
}

func TestExampleConfigLoads(t *testing.T) {
	t.Setenv("LOOKOUT_BASIC_AUTH", "dXNlcjpwYXNz")
	if _, err := LoadFile("../../config.example.yaml"); err != nil {
		t.Fatalf("config.example.yaml must be valid: %v", err)
	}
}

func TestDNSCheckDefaultsAndFields(t *testing.T) {
	cfg := mustLoad(t, `
checks:
  - name: MX
    type: dns
    host: service.example
    query_type: mx
    resolver: 192.0.2.53
    expect:
      answers_contain: mail.service.example
`)
	c := cfg.Checks[0]
	if c.Type != TypeDNS {
		t.Fatalf("type = %q", c.Type)
	}
	if c.Host != "service.example" {
		t.Errorf("host = %q", c.Host)
	}
	if c.QueryType != QueryMX {
		t.Errorf("query_type = %q", c.QueryType)
	}
	if c.Resolver != "192.0.2.53:53" {
		t.Errorf("resolver = %q", c.Resolver)
	}
	if c.Interval != DefaultDNSInterval {
		t.Errorf("interval = %s, want the dns default %s", c.Interval, DefaultDNSInterval)
	}
	if c.Expect.Rcode != DefaultRcode {
		t.Errorf("rcode = %q, want the default %q", c.Expect.Rcode, DefaultRcode)
	}
	if c.Expect.AnswersContain != "mail.service.example" {
		t.Errorf("answers_contain = %q", c.Expect.AnswersContain)
	}
	if !c.Expect.Status.IsZero() {
		t.Errorf("dns checks must not inherit the HTTP status default, got %q", c.Expect.Status)
	}
	if !c.Alert {
		t.Error("a dns check without alert: must still alert")
	}
}

func TestDomainCheckDefaultsAndFields(t *testing.T) {
	cfg := mustLoad(t, `
checks:
  - name: Registration
    type: domain
    domain: service.example
`)
	c := cfg.Checks[0]
	if c.Type != TypeDomain || c.Host != "service.example" {
		t.Fatalf("got type=%q host=%q", c.Type, c.Host)
	}
	if c.Interval != DefaultDomainInterval {
		t.Errorf("interval = %s, want %s", c.Interval, DefaultDomainInterval)
	}
	if c.Timeout != DefaultDomainTimeout {
		t.Errorf("timeout = %s, want %s", c.Timeout, DefaultDomainTimeout)
	}
	if !c.Expect.Status.IsZero() {
		t.Errorf("domain checks must not inherit the HTTP status default")
	}
	if !c.Alert {
		t.Error("a domain check without alert: must still alert")
	}
}

func TestDomainAcceptsHostAlias(t *testing.T) {
	cfg := mustLoad(t, `
checks:
  - name: Registration
    type: domain
    host: service.example
`)
	if cfg.Checks[0].Host != "service.example" {
		t.Errorf("host = %q", cfg.Checks[0].Host)
	}
}

func TestDefaultsIntervalDoesNotOverrideTypeDefaultWhenUnset(t *testing.T) {
	// A global defaults.interval must still apply — the operator asked
	// for it — but a missing defaults block leaves dns/domain alone.
	cfg := mustLoad(t, `
defaults:
  interval: 90s
  timeout: 5s
checks:
  - name: Web
    type: http
    url: http://service.example
  - name: MX
    type: dns
    host: service.example
    query_type: MX
    interval: 5m
`)
	if cfg.Checks[0].Interval != 90*time.Second {
		t.Errorf("http interval = %s, want the defaults block", cfg.Checks[0].Interval)
	}
	if cfg.Checks[1].Interval != 5*time.Minute {
		t.Errorf("dns interval = %s, want the per-check override", cfg.Checks[1].Interval)
	}
}

func TestIDNHostMustBeWrittenAsPunycode(t *testing.T) {
	_, err := Load("config.yaml", []byte(`
checks:
  - name: IDN
    type: domain
    domain: "пример.example"
`))
	if err == nil {
		t.Fatal("want an error: we do not take x/text just to convert IDN")
	}
	if !strings.Contains(err.Error(), "punycode") {
		t.Errorf("error = %q, want it to ask for punycode", err)
	}

	cfg := mustLoad(t, `
checks:
  - name: IDN
    type: domain
    domain: xn--e1afmkfd.example
`)
	if cfg.Checks[0].Host != "xn--e1afmkfd.example" {
		t.Errorf("host = %q", cfg.Checks[0].Host)
	}
}

func TestMuteWindowsLoad(t *testing.T) {
	cfg := mustLoad(t, `
mute:
  - every: [Saturday, Sunday]
    at: "02:00"
    duration: 4h
    timezone: UTC
    group: Public
checks:
  - name: MX
    group: Public
    type: dns
    host: service.example
    query_type: MX
`)
	if len(cfg.Mute) != 1 {
		t.Fatalf("mute windows = %d", len(cfg.Mute))
	}
	w := cfg.Mute[0]
	if w.At != 2*time.Hour || w.Duration != 4*time.Hour || w.Group != "Public" {
		t.Errorf("window = %+v", w)
	}
	if len(w.Every) != 2 || w.Every[0] != time.Saturday || w.Every[1] != time.Sunday {
		t.Errorf("every = %v", w.Every)
	}
	if w.TZName != "UTC" || w.Location != time.UTC {
		t.Errorf("timezone = %s loc=%v", w.TZName, w.Location)
	}
}

func TestMuteWindowRejectsCron(t *testing.T) {
	_, err := Load("config.yaml", []byte(`
mute:
  - cron: "0 2 * * 6"
    duration: 4h
checks:
  - name: Example
    type: http
    url: http://example.invalid
`))
	if err == nil {
		t.Fatal("cron must be rejected")
	}
	if !strings.Contains(err.Error(), "cron expressions are not accepted") {
		t.Errorf("error = %q", err)
	}
}

func TestMuteWindowRequiresAtAndDuration(t *testing.T) {
	_, err := Load("config.yaml", []byte(`
mute:
  - every: [Monday]
checks:
  - name: Example
    type: http
    url: http://example.invalid
`))
	if err == nil {
		t.Fatal("missing at/duration must be an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "at is required") || !strings.Contains(msg, "duration is required") {
		t.Errorf("error = %q", msg)
	}
}

func TestMuteWindowUnknownCheck(t *testing.T) {
	_, err := Load("config.yaml", []byte(`
mute:
  - at: "02:00"
    duration: 1h
    check: Missing
checks:
  - name: Example
    type: http
    url: http://example.invalid
`))
	if err == nil {
		t.Fatal("want an error for a window that names a missing check")
	}
	if !strings.Contains(err.Error(), "no check named") {
		t.Errorf("error = %q", err)
	}
}

func TestHistoryFileDefaultsBesideStateFile(t *testing.T) {
	cfg := mustLoad(t, minimal)
	if cfg.HistoryFile != "history.jsonl" {
		t.Errorf("history = %q, want it next to the default state file", cfg.HistoryFile)
	}
	cfg = mustLoad(t, `
state:
  file: /var/lib/lookout/state.json
  history: /var/lib/lookout/history.jsonl
checks:
  - name: Example
    type: http
    url: http://example.invalid
`)
	if cfg.StateFile != "/var/lib/lookout/state.json" || cfg.HistoryFile != "/var/lib/lookout/history.jsonl" {
		t.Errorf("state=%q history=%q", cfg.StateFile, cfg.HistoryFile)
	}
}

// Same reason state.history is next to the state file: a separate
// volume is a second thing to forget when ProtectSystem=strict.
func TestSamplesFileDefaultsBesideStateFile(t *testing.T) {
	cfg := mustLoad(t, minimal)
	if cfg.SamplesFile != "samples.jsonl" {
		t.Errorf("samples = %q, want it next to the default state file", cfg.SamplesFile)
	}
	cfg = mustLoad(t, `
state:
  file: /var/lib/lookout/state.json
  samples: /var/lib/lookout/samples.jsonl
checks:
  - name: Example
    type: http
    url: http://example.invalid
`)
	if cfg.StateFile != "/var/lib/lookout/state.json" || cfg.SamplesFile != "/var/lib/lookout/samples.jsonl" {
		t.Errorf("state=%q samples=%q", cfg.StateFile, cfg.SamplesFile)
	}
	if _, err := Load("config.yaml", []byte("state:\n  samples: \"\"\n"+minimal)); err == nil {
		t.Error("an empty state.samples must not load")
	}
}

// Clocks on the status page are for a person; the timezone is theirs to
// choose, and an unknown one is a config error rather than a silent UTC.
func TestTimezoneOverride(t *testing.T) {
	cfg := mustLoad(t, "timezone: Europe/Moscow\n"+minimal)
	if cfg.TZName != "Europe/Moscow" || cfg.Location == nil {
		t.Fatalf("timezone = %q / %v", cfg.TZName, cfg.Location)
	}
	if _, offset := time.Date(2026, 8, 19, 12, 0, 0, 0, cfg.Location).Zone(); offset != 3*60*60 {
		t.Errorf("offset = %ds, want Moscow's +3h", offset)
	}
	if _, err := Load("config.yaml", []byte("timezone: Mars/Olympus\n"+minimal)); err == nil {
		t.Error("an unknown timezone must not load")
	}
}

// A tcp check is the one for everything that listens without speaking HTTP.
// Its address is the only thing it has, so the validator has to be strict
// about it: a check watching the wrong port is worse than no check.
func TestTCPCheckAddress(t *testing.T) {
	cfg := mustLoad(t, `
checks:
  - name: Database
    type: tcp
    address: db.lan:5432
`)
	c := cfg.Checks[0]
	if c.Type != TypeTCP || c.Address != "db.lan:5432" {
		t.Fatalf("check = %+v", c)
	}
	if c.URL != "" || c.Host != "" {
		t.Errorf("a tcp check picked up http or dns fields: %+v", c)
	}
}

func TestTCPCheckRejectsBadAddresses(t *testing.T) {
	for _, tc := range []struct{ name, want, src string }{
		{"missing", "address is required", `
checks:
  - name: Database
    type: tcp
`},
		{"bare host", "is not host:port", `
checks:
  - name: Database
    type: tcp
    address: db.lan
`},
		{"not a port", "is not a port number", `
checks:
  - name: Database
    type: tcp
    address: db.lan:postgres
`},
		{"url instead", "url is for", `
checks:
  - name: Database
    type: tcp
    address: db.lan:5432
    url: http://db.lan:5432/
`},
		{"body condition", "body_contains is only valid", `
checks:
  - name: Database
    type: tcp
    address: db.lan:5432
    expect:
      body_contains: ready
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load("config.yaml", []byte(tc.src))
			if err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The registration of a name is derived from wherever a host appears, and a
// tcp address is a place a host appears.
func TestTCPCheckDerivesRegistration(t *testing.T) {
	cfg := mustLoad(t, `
checks:
  - name: Mail
    type: tcp
    address: mail.example.com:25
`)
	var derived []Check
	for _, c := range cfg.Checks {
		if c.Implicit {
			derived = append(derived, c)
		}
	}
	if len(derived) != 1 || derived[0].Host != "example.com" {
		t.Fatalf("derived = %+v, want example.com", derived)
	}
}
