package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eeegoloauq/lookout/internal/monitor"
	"github.com/eeegoloauq/lookout/internal/state"
)

const pushChecks = `
checks:
  - name: ZFS tank
    group: Jobs
    type: push
    expect_every: 10m
    grace: 2m
    token: 0123456789abcdef
`

const pushToken = "0123456789abcdef"

func pushHandler(t *testing.T) (http.Handler, *monitor.Monitor) {
	t.Helper()
	m := testMonitor(t, pushChecks)
	return New(m, "test", ""), m
}

// send fires one ping the way a cron line would, and returns the reply.
func send(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, nil)
	// A ping comes from the machine doing the work, which is not this one.
	req.RemoteAddr = "198.51.100.7:41234"
	h.ServeHTTP(rec, req)
	return rec
}

// lastPing is what the page and the API would show for the check.
func lastPing(t *testing.T, m *monitor.Monitor) state.Ping {
	t.Helper()
	p, ok := m.LastPing("ZFS tank")
	if !ok {
		t.Fatal("no ping was recorded")
	}
	return p
}

func TestPushRecordsAPing(t *testing.T) {
	h, m := pushHandler(t)
	rec := send(t, h, http.MethodPost, "/api/push/"+pushToken)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := lastPing(t, m).Status; got != state.PingOK {
		t.Fatalf("status = %q, want %q: a ping with no status is a good one", got, state.PingOK)
	}
}

// A cron line should be curl and a URL, with no flags to get wrong.
func TestPushAcceptsGET(t *testing.T) {
	h, m := pushHandler(t)
	if rec := send(t, h, http.MethodGet, "/api/push/"+pushToken); rec.Code != http.StatusNoContent {
		t.Fatalf("GET status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if _, ok := m.LastPing("ZFS tank"); !ok {
		t.Fatal("a GET ping was not recorded")
	}
}

func TestPushCarriesStatusAndMessage(t *testing.T) {
	h, m := pushHandler(t)
	rec := send(t, h, http.MethodPost, "/api/push/"+pushToken+"?status=fail&msg=pool+degraded")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	p := lastPing(t, m)
	if !p.Failed() || p.Msg != "pool degraded" {
		t.Fatalf("ping = %+v, want a failure carrying its message", p)
	}
}

// Silently reading an unrecognised word as "ok" is how a job reports
// success for a year while failing every night.
func TestPushRejectsAnUnknownStatus(t *testing.T) {
	h, m := pushHandler(t)
	rec := send(t, h, http.MethodPost, "/api/push/"+pushToken+"?status=failed")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if _, ok := m.LastPing("ZFS tank"); ok {
		t.Fatal("a rejected ping was recorded anyway")
	}
}

// The message ends up in the state file, on the page and in an alert, so
// the endpoint is where it stops being arbitrary.
func TestPushTruncatesTheMessage(t *testing.T) {
	h, m := pushHandler(t)
	long := strings.Repeat("x", 500)
	if rec := send(t, h, http.MethodPost, "/api/push/"+pushToken+"?msg="+long); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if n := len([]rune(lastPing(t, m).Msg)); n != 200 {
		t.Fatalf("stored message is %d runes, want it capped at 200", n)
	}
}

// A body that could not be read must not fall through to the default
// status: an oversized `status=fail` recorded as a healthy heartbeat would
// push the deadline out and report the failing job as the fine one.
func TestPushRejectsABodyItCannotRead(t *testing.T) {
	h, m := pushHandler(t)
	rec := httptest.NewRecorder()
	body := "status=fail&msg=" + strings.Repeat("x", pushMaxBody)
	req := httptest.NewRequest(http.MethodPost, "/api/push/"+pushToken, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "198.51.100.7:41234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if _, ok := m.LastPing("ZFS tank"); ok {
		t.Fatal("an unreadable ping was recorded as a heartbeat")
	}
}

// A cache in front of lookout that answered a ping out of its own copy
// would leave the deadline running against a heartbeat nobody saw.
func TestPushRepliesAreNotCacheable(t *testing.T) {
	h, _ := pushHandler(t)
	rec := send(t, h, http.MethodGet, "/api/push/"+pushToken)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	rec = send(t, h, http.MethodGet, "/api/push/0123456789abcdeF")
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control on a 404 = %q, want no-store", got)
	}
}

func TestPushUnknownTokenIs404AndSaysNothing(t *testing.T) {
	h, m := pushHandler(t)
	rec := send(t, h, http.MethodPost, "/api/push/0123456789abcdeF")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	body := rec.Body.String()
	for _, leak := range []string{"token", "ZFS", "check"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(leak)) {
			t.Fatalf("the 404 body hints at %q: %q", leak, body)
		}
	}
	if _, ok := m.LastPing("ZFS tank"); ok {
		t.Fatal("a ping with the wrong token was recorded")
	}
}

// The token is a credential in a URL: it must not turn up in the document
// anyone who can reach the port may read.
func TestStatusDocumentShowsThePingAndNotTheToken(t *testing.T) {
	h, _ := pushHandler(t)
	send(t, h, http.MethodPost, "/api/push/"+pushToken+"?msg=backup+done")

	rec := get(t, h, "/api/status")
	body := rec.Body.String()
	if strings.Contains(body, pushToken) {
		t.Fatal("the status document contains the push token")
	}
	if !strings.Contains(body, `"expect_every_ms":600000`) {
		t.Fatalf("status document has no push deadline: %s", body)
	}
	if !strings.Contains(body, "backup done") {
		t.Fatalf("status document has no last ping: %s", body)
	}
}

func TestPageShowsAPushRowWithoutALatency(t *testing.T) {
	h, _ := pushHandler(t)
	send(t, h, http.MethodPost, "/api/push/"+pushToken)

	rec := get(t, h, "/")
	body := rec.Body.String()
	if strings.Contains(body, pushToken) {
		t.Fatal("the page contains the push token")
	}
	if !strings.Contains(body, "ZFS tank") {
		t.Fatal("the push check is not on the board")
	}
	if strings.Contains(body, "<1ms") {
		t.Fatal("the board printed a response time for a check that measures none")
	}
	if !strings.Contains(body, "a ping every 10m") {
		t.Fatal("the panel does not say what the check is waiting for")
	}
}
