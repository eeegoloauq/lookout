package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
	"github.com/eeegoloauq/lookout/internal/monitor"
	"github.com/eeegoloauq/lookout/internal/state"
)

const twoChecks = `
checks:
  - name: Photos
    group: Services
    type: http
    url: http://photos.invalid/ping
  - name: Router
    group: Core
    type: http
    url: http://router.invalid/
    interval: 30s
    timeout: 3s
`

// localPost builds a request that looks like it came from the host
// running lookout: mutating endpoints refuse anything else.
func localPost(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	return req
}

func testMonitor(t *testing.T, src string) *monitor.Monitor {
	t.Helper()
	cfg, err := config.Load("config.yaml", []byte(src))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	dir := t.TempDir()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.HistoryFile = filepath.Join(dir, "history.jsonl")
	return monitor.New(cfg, nil, monitor.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	h.ServeHTTP(rec, req)
	return rec
}

func feed(t *testing.T, m *monitor.Monitor, name string, pattern string, start time.Time, dur time.Duration, code int) {
	t.Helper()
	var c config.Check
	for _, ch := range m.Config().Checks {
		if ch.Name == name {
			c = ch
			break
		}
	}
	if c.Name == "" {
		t.Fatalf("no check named %q", name)
	}
	for i, r := range pattern {
		outcome := check.OutcomeUp
		status := code
		if r == 'D' {
			outcome = check.OutcomeDown
			if status == 0 {
				status = 500
			}
		} else if r == 'M' {
			outcome = check.OutcomeMalformed
		} else {
			status = 200
		}
		at := start.Add(time.Duration(i) * c.Interval)
		res := check.Result{Name: name, At: at, Outcome: outcome, Duration: dur, StatusCode: status}
		m.Machine().Observe(c, res)
		m.History().Record(res)
	}
}

func TestStatusAPIContract(t *testing.T) {
	m := testMonitor(t, twoChecks)
	now := time.Now()
	feed(t, m, "Photos", "UU", now.Add(-2*time.Minute), 42*time.Millisecond, 200)
	feed(t, m, "Router", "DDD", now.Add(-time.Hour), 1500*time.Millisecond, 502)

	h := New(m, "abc1234")
	rec := get(t, h, "/api/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}

	var doc StatusDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, rec.Body.Bytes())
	}
	if doc.Version != APIVersion {
		t.Errorf("version = %d, want %d", doc.Version, APIVersion)
	}
	if doc.Build != "abc1234" {
		t.Errorf("build = %q", doc.Build)
	}
	if len(doc.Checks) != 2 {
		t.Fatalf("checks = %d, want 2", len(doc.Checks))
	}

	photos, router := doc.Checks[0], doc.Checks[1]
	if photos.Name != "Photos" || photos.Group != "Services" || photos.Type != "http" {
		t.Errorf("photos identity = %+v", photos)
	}
	if photos.Status != "up" || photos.Unstable {
		t.Errorf("photos state = %s unstable=%v", photos.Status, photos.Unstable)
	}
	if photos.LastProbe == nil || photos.LastProbe.Outcome != "up" || photos.LastProbe.DurationMS != 42 {
		t.Errorf("photos last_probe = %+v", photos.LastProbe)
	}
	if photos.Uptime24h == nil || photos.Uptime24h.Samples != 2 || photos.Uptime24h.Ratio != 1 {
		t.Errorf("photos uptime = %+v", photos.Uptime24h)
	}
	if photos.Incident != nil {
		t.Errorf("photos must not have an incident: %+v", photos.Incident)
	}

	if router.Status != "down" {
		t.Errorf("router status = %q", router.Status)
	}
	if router.LastProbe == nil || router.LastProbe.StatusCode != 502 {
		t.Errorf("router last_probe = %+v", router.LastProbe)
	}
	if router.Incident == nil {
		t.Fatal("router incident is null, want the current outage")
	}
	if router.Incident.DurationMS < int64((59 * time.Minute).Milliseconds()) {
		t.Errorf("incident duration_ms = %d, want about an hour", router.Incident.DurationMS)
	}
	if router.Uptime24h == nil || router.Uptime24h.Ratio != 0 {
		t.Errorf("router uptime = %+v, want 0 over the failed probes", router.Uptime24h)
	}
}

func TestStatusOmitsUptimeWhenThereAreNoSamples(t *testing.T) {
	m := testMonitor(t, twoChecks)
	h := New(m, "test")
	rec := get(t, h, "/api/status")
	var doc StatusDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Checks {
		if c.Status != "unknown" {
			t.Errorf("%s status = %q, want unknown before any probe", c.Name, c.Status)
		}
		if c.LastProbe != nil {
			t.Errorf("%s last_probe = %+v, want null", c.Name, c.LastProbe)
		}
		if c.Uptime24h != nil {
			t.Errorf("%s uptime_24h = %+v, want null (no data, not 0%%)", c.Name, c.Uptime24h)
		}
	}
}

func TestCheckHistory(t *testing.T) {
	m := testMonitor(t, twoChecks)
	feed(t, m, "Photos", "UDU", time.Now().Add(-3*time.Minute), 10*time.Millisecond, 200)
	h := New(m, "test")

	rec := get(t, h, "/api/checks/Photos")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.Bytes())
	}
	var doc HistoryDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != APIVersion || doc.Name != "Photos" || doc.Group != "Services" {
		t.Errorf("header = %+v", doc)
	}
	if len(doc.Points) != 3 {
		t.Fatalf("points = %d, want 3", len(doc.Points))
	}
	if !doc.Points[0].OK || doc.Points[1].OK || !doc.Points[2].OK {
		t.Errorf("ok flags = %v, %v, %v", doc.Points[0].OK, doc.Points[1].OK, doc.Points[2].OK)
	}
	if doc.Points[1].Outcome != "down" {
		t.Errorf("point[1].outcome = %q", doc.Points[1].Outcome)
	}

	missing := get(t, h, "/api/checks/DoesNotExist")
	if missing.Code != http.StatusNotFound {
		t.Errorf("missing check: %d, want 404", missing.Code)
	}
}

func TestAPIDoesNotLeakSecrets(t *testing.T) {
	const src = `
checks:
  - name: Private
    group: Services
    type: http
    url: https://user:super-secret-password@private.invalid/v1
    headers:
      Authorization: "Bearer super-secret-header-value"
      X-Token: "another-secret-token"
`
	m := testMonitor(t, src)
	feed(t, m, "Private", "U", time.Now(), 5*time.Millisecond, 200)
	h := New(m, "test")

	leaks := []string{
		"super-secret-password",
		"super-secret-header-value",
		"another-secret-token",
		"Bearer super-secret",
		"user:super-secret",
	}
	paths := []string{"/api/status", "/api/checks/Private", "/", "/metrics", "/healthz"}
	for _, path := range paths {
		rec := get(t, h, path)
		body := rec.Body.String()
		for _, leak := range leaks {
			if strings.Contains(body, leak) {
				t.Errorf("%s leaked %q:\n%s", path, leak, body)
			}
		}
		if strings.Contains(body, "Authorization") && path != "/" {
			// The page CSS/HTML must not mention it either; skip only if
			// a future copy change uses the word. Headers themselves
			// must never be serialized.
			if strings.Contains(body, `"Authorization"`) || strings.Contains(body, "Bearer ") {
				t.Errorf("%s serialized an authorization header: %s", path, body)
			}
		}
	}

	status := get(t, h, "/api/status")
	var doc StatusDocument
	if err := json.Unmarshal(status.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Checks) != 1 {
		t.Fatal(doc.Checks)
	}
	u := doc.Checks[0].URL
	if !strings.Contains(u, "xxxxx") {
		t.Errorf("url = %q, want userinfo masked", u)
	}
	if !strings.Contains(u, "private.invalid") {
		t.Errorf("url = %q, want the host to remain so the check is identifiable", u)
	}

	// Metrics must not carry the target URL at all: a label is a leak
	// surface even after masking.
	metrics := get(t, h, "/metrics").Body.String()
	if strings.Contains(metrics, "private.invalid") {
		t.Errorf("metrics include the target host:\n%s", metrics)
	}
}

func TestMetricsCanonicalNames(t *testing.T) {
	m := testMonitor(t, twoChecks)
	now := time.Now()
	feed(t, m, "Photos", "UU", now.Add(-time.Minute), 42*time.Millisecond, 200)
	feed(t, m, "Router", "DDD", now.Add(-time.Hour), time.Second, 500)

	body := get(t, New(m, "test"), "/metrics").Body.String()
	for _, want := range []string{
		"# TYPE lookout_up gauge",
		"# TYPE lookout_probe_success gauge",
		"# TYPE lookout_probe_duration_seconds gauge",
		"# TYPE lookout_uptime_ratio gauge",
		"# TYPE lookout_undelivered_alert_age_seconds gauge",
		`lookout_up{check="Photos",group="Services"} 1`,
		`lookout_up{check="Router",group="Core"} 0`,
		`lookout_probe_success{check="Photos",group="Services"} 1`,
		`lookout_probe_success{check="Router",group="Core"} 0`,
		`lookout_probe_duration_seconds{check="Photos",group="Services"} 0.042`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n%s", want, body)
		}
	}
	for _, leak := range []string{
		"probe_ssl_earliest_cert_expiry",
	} {
		if strings.Contains(body, leak) {
			t.Errorf("metrics contain %q", leak)
		}
	}
	// No certificate or domain has been observed on these HTTP checks,
	// so the gauges must be absent — 0 would page as "expired".
	if strings.Contains(body, "lookout_cert_days_left{") {
		t.Errorf("cert gauge present without a certificate:\n%s", body)
	}
	if strings.Contains(body, "lookout_domain_days_left{") {
		t.Errorf("domain gauge present without a registry lookup:\n%s", body)
	}
}

func TestStatusAndMetricsExposeExpiry(t *testing.T) {
	const src = `
checks:
  - name: Photos
    group: Services
    type: http
    url: https://photos.invalid/
  - name: Registration
    group: Public
    type: domain
    domain: service.example
`
	m := testMonitor(t, src)
	now := time.Now()
	httpCheck := m.Config().Checks[0]
	domCheck := m.Config().Checks[1]
	notAfter := now.Add(14 * 24 * time.Hour)
	expires := now.Add(45 * 24 * time.Hour)
	m.Machine().Observe(httpCheck, check.Result{
		Name: httpCheck.Name, At: now, Outcome: check.OutcomeUp, Duration: time.Millisecond, StatusCode: 200,
		CertNotAfter: notAfter,
	})
	m.History().Record(check.Result{Name: httpCheck.Name, At: now, Outcome: check.OutcomeUp, Duration: time.Millisecond})
	m.Machine().Observe(domCheck, check.Result{
		Name: domCheck.Name, At: now, Outcome: check.OutcomeUp,
		DomainExpiresAt: expires, DomainSource: "rdap",
	})

	h := New(m, "test")
	var doc StatusDocument
	if err := json.Unmarshal(get(t, h, "/api/status").Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Checks[0].CertDaysLeft == nil || *doc.Checks[0].CertDaysLeft < 13 || *doc.Checks[0].CertDaysLeft > 14 {
		t.Errorf("cert_days_left = %v", doc.Checks[0].CertDaysLeft)
	}
	if doc.Checks[1].DomainDaysLeft == nil || *doc.Checks[1].DomainDaysLeft < 44 || *doc.Checks[1].DomainDaysLeft > 45 {
		t.Errorf("domain_days_left = %v", doc.Checks[1].DomainDaysLeft)
	}

	body := get(t, h, "/metrics").Body.String()
	if !strings.Contains(body, `lookout_cert_days_left{check="Photos",group="Services"}`) {
		t.Errorf("metrics missing cert gauge:\n%s", body)
	}
	if !strings.Contains(body, `lookout_domain_days_left{check="Registration",group="Public"}`) {
		t.Errorf("metrics missing domain gauge:\n%s", body)
	}

	page := get(t, h, "/").Body.String()
	if !strings.Contains(page, "cert ") || !strings.Contains(page, "domain ") {
		t.Errorf("status page missing expiry:\n%s", page)
	}
}

func TestMetricsOmitUnknownAndEmptyUptime(t *testing.T) {
	m := testMonitor(t, twoChecks)
	body := get(t, New(m, "test"), "/metrics").Body.String()
	if strings.Contains(body, `lookout_up{`) {
		t.Errorf("lookout_up must be absent while state is unknown:\n%s", body)
	}
	if strings.Contains(body, "lookout_uptime_ratio{") {
		t.Errorf("lookout_uptime_ratio must be absent with no samples:\n%s", body)
	}
	if !strings.Contains(body, "lookout_undelivered_alerts 0") {
		t.Errorf("outbox depth missing:\n%s", body)
	}
}

func TestHealthzOKByDefault(t *testing.T) {
	m := testMonitor(t, twoChecks)
	rec := get(t, New(m, "test"), "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.Bytes())
	}
	var doc HealthDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Status != "ok" {
		t.Errorf("status = %q", doc.Status)
	}
}

func TestHealthzDegradedWhenOutboxIsStuck(t *testing.T) {
	m := testMonitor(t, twoChecks)
	box := state.Outbox{
		Attempts: degradedAfterAttempts,
		Items: []state.OutboxItem{{
			ID:       1,
			Enqueued: time.Now().Add(-2 * time.Minute),
			Event:    state.Event{Kind: state.EventDown, Check: "Photos", Alert: true},
		}},
	}
	if err := state.NewStore(m.Config().StateFile).Save(state.Snapshot{Checks: map[string]state.CheckState{}, Outbox: box}); err != nil {
		t.Fatal(err)
	}
	m.Restore()

	rec := get(t, New(m, "test"), "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", rec.Code, rec.Body.Bytes())
	}
	var doc HealthDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Status != "degraded" {
		t.Errorf("status = %q", doc.Status)
	}
	if !strings.Contains(doc.Reason, "failed") {
		t.Errorf("reason = %q", doc.Reason)
	}

	page := get(t, New(m, "test"), "/")
	if !strings.Contains(page.Body.String(), "cannot notify") {
		t.Errorf("status page must show a stuck outbox, got:\n%s", page.Body.String())
	}
}

func TestHealthzStaysOKDuringTheBatchWindow(t *testing.T) {
	m := testMonitor(t, twoChecks)
	box := state.Outbox{
		Attempts: 0,
		Items: []state.OutboxItem{{
			ID:       1,
			Enqueued: time.Now(),
			Event:    state.Event{Kind: state.EventDown, Check: "Photos", Alert: true},
		}},
	}
	if err := state.NewStore(m.Config().StateFile).Save(state.Snapshot{Checks: map[string]state.CheckState{}, Outbox: box}); err != nil {
		t.Fatal(err)
	}
	m.Restore()
	rec := get(t, New(m, "test"), "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("queued but not yet retried must not look sick: %d %s", rec.Code, rec.Body.Bytes())
	}
}

type failNotifier struct{ err error }

func (f failNotifier) Notify(context.Context, string) error { return f.err }

type errString string

func (e errString) Error() string { return string(e) }

type scriptedProber struct {
	outcome check.Outcome
}

func (s scriptedProber) Probe(_ context.Context, c config.Check) check.Result {
	code := 200
	if s.outcome.Failed() {
		code = 500
	}
	return check.Result{Name: c.Name, At: time.Now(), Outcome: s.outcome, StatusCode: code, Duration: time.Millisecond}
}

func TestHealthzDegradedAfterFailedDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, err := config.Load("config.yaml", []byte(`
alerting:
  batch_window: 45s
checks:
  - name: Photos
    group: Services
    type: http
    url: http://photos.invalid
`))
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		cfg.StateFile = filepath.Join(dir, "state.json")
		cfg.HistoryFile = filepath.Join(dir, "history.jsonl")
		m := monitor.New(cfg, scriptedProber{outcome: check.OutcomeDown},
			monitor.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
			monitor.WithNotifier(failNotifier{err: errString("telegram unreachable")}),
		)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- m.Run(ctx) }()
		time.Sleep(10 * time.Minute)
		rec := get(t, New(m, "test"), "/healthz")
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("after a stuck channel: %d %s", rec.Code, rec.Body.Bytes())
		}
		if !strings.Contains(rec.Body.String(), `"degraded"`) {
			t.Errorf("body = %s", rec.Body.Bytes())
		}
		if n := len(m.Outbox().Items); n == 0 {
			t.Fatal("outbox is empty; the alert was lost rather than held")
		}
	})
}

func TestStatusPageIsSelfContained(t *testing.T) {
	m := testMonitor(t, twoChecks)
	feed(t, m, "Photos", "UU", time.Now(), 12*time.Millisecond, 200)
	feed(t, m, "Router", "DDD", time.Now().Add(-time.Hour), time.Second, 500)
	rec := get(t, New(m, "test"), "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<table",
		"Photos",
		"Router",
		"UP",
		"DOWN",
		"prefers-color-scheme: dark",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"<script",
		"cdn.",
		"fonts.googleapis",
		"fonts.gstatic",
		"font-awesome",
		"http://",
		"https://",
	} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("page must not pull %q (no JS, no external assets)", forbidden)
		}
	}
}

func TestMuteAPIAndStatusVisibility(t *testing.T) {
	m := testMonitor(t, twoChecks)
	h := New(m, "test")

	body := strings.NewReader(`{"for":"30m","group":"Services"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mute", body)
	req.RemoteAddr = "127.0.0.1:54321"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mute: %d %s", rec.Code, rec.Body.Bytes())
	}
	var mr MuteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &mr); err != nil {
		t.Fatal(err)
	}
	if !mr.OK || mr.Until == nil {
		t.Fatalf("mute response = %+v", mr)
	}

	docRec := get(t, h, "/api/status")
	var doc StatusDocument
	if err := json.Unmarshal(docRec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Mutes) != 1 || doc.Mutes[0].Group != "Services" {
		t.Fatalf("mutes = %+v", doc.Mutes)
	}
	var photos, router CheckStatus
	for _, c := range doc.Checks {
		switch c.Name {
		case "Photos":
			photos = c
		case "Router":
			router = c
		}
	}
	if !photos.Muted {
		t.Error("Photos is in Services and must show muted")
	}
	if router.Muted {
		t.Error("Router is in Core and must not be muted")
	}

	page := get(t, h, "/").Body.String()
	if !strings.Contains(page, "Alerts muted") || !strings.Contains(page, "MUTED") {
		t.Errorf("status page must show the mute, got:\n%s", page)
	}
	metrics := get(t, h, "/metrics").Body.String()
	if !strings.Contains(metrics, `lookout_muted{check="Photos",group="Services"} 1`) {
		t.Errorf("metrics missing muted gauge:\n%s", metrics)
	}

	un := httptest.NewRecorder()
	h.ServeHTTP(un, localPost("/api/unmute", `{"group":"Services"}`))
	if un.Code != http.StatusOK {
		t.Fatalf("unmute: %d %s", un.Code, un.Body.Bytes())
	}
	var doc2 StatusDocument
	if err := json.Unmarshal(get(t, h, "/api/status").Body.Bytes(), &doc2); err != nil {
		t.Fatal(err)
	}
	if len(doc2.Mutes) != 0 {
		t.Errorf("mutes after unmute = %+v", doc2.Mutes)
	}
}

func TestMuteRejectsBadDuration(t *testing.T) {
	h := New(testMonitor(t, twoChecks), "test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localPost("/api/mute", `{"for":"nope"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	rec := get(t, New(testMonitor(t, twoChecks), "test"), "/nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d", rec.Code)
	}
}

func TestFaviconIsSilent(t *testing.T) {
	rec := get(t, New(testMonitor(t, twoChecks), "test"), "/favicon.ico")
	if rec.Code != http.StatusNoContent {
		t.Errorf("status %d, want 204 so a browser tab does not log a 404", rec.Code)
	}
}

// Reading the status is open to anyone who can reach the port; silencing
// the monitor is not. The read-only surface is routinely bound to a LAN
// address so a browser can open the page, and that must not hand every
// host on the network a mute switch.
func TestMutingIsRefusedFromTheNetwork(t *testing.T) {
	h := New(testMonitor(t, twoChecks), "test")
	for _, path := range []string{"/api/mute", "/api/unmute"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"for":"30m"}`))
		req.RemoteAddr = "192.0.2.10:54321"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s from a remote address = %d, want %d", path, rec.Code, http.StatusForbidden)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/mute", strings.NewReader(`{"for":"30m"}`))
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("mute from loopback = %d, want %d", rec.Code, http.StatusOK)
	}
}
