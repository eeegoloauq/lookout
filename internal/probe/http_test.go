package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
)

// checkFor builds a check against url through the real loader.
func checkFor(t *testing.T, url, extra string) config.Check {
	t.Helper()
	src := "checks:\n  - name: T\n    type: http\n    url: " + url + "\n    interval: 10s\n    timeout: 2s\n" + extra
	cfg, err := config.Load("config.yaml", []byte(src))
	if err != nil {
		t.Fatalf("loading test config: %v", err)
	}
	return cfg.Checks[0]
}

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeScenarios(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		extra   string
		want    check.Outcome
		reason  string
	}{
		{
			name:    "healthy",
			handler: func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"res":"pong"}`)) },
			extra:   "    expect:\n      status: 200\n      body:\n        \".res\": \"pong\"\n",
			want:    check.OutcomeUp,
		},
		{
			name:    "server error",
			handler: func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) },
			want:    check.OutcomeDown,
			reason:  "status: want 200-299, got 500",
		},
		{
			name:    "broken json",
			handler: func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"res":`)) },
			extra:   "    expect:\n      body:\n        \".res\": \"pong\"\n",
			want:    check.OutcomeMalformed,
			reason:  "not valid JSON",
		},
		{
			name:    "slow response",
			handler: func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) },
			extra:   "    expect:\n      response_time: \"<1ns\"\n",
			want:    check.OutcomeDown,
			reason:  "response_time: want <1ns",
		},
	}
	p := NewHTTP()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, tc.handler)
			res := p.Probe(context.Background(), checkFor(t, srv.URL, tc.extra))
			if res.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q (reason: %s)", res.Outcome, tc.want, res.Reason())
			}
			if tc.reason != "" && !strings.Contains(res.Reason(), tc.reason) {
				t.Errorf("reason = %q, want it to contain %q", res.Reason(), tc.reason)
			}
			if res.Duration <= 0 {
				t.Error("duration was not measured")
			}
		})
	}
}

func TestProbeTimeout(t *testing.T) {
	// The handler blocks until the client gives up, so the timeout path is
	// exercised without sleeping for a fixed duration.
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	c := checkFor(t, srv.URL, "")
	c.Timeout = 50 * time.Millisecond

	res := NewHTTP().Probe(context.Background(), c)
	if res.Outcome != check.OutcomeDown {
		t.Fatalf("outcome = %q, want %q", res.Outcome, check.OutcomeDown)
	}
	if want := "timed out after 50ms"; res.Err != want {
		t.Errorf("err = %q, want %q", res.Err, want)
	}
	if res.StatusCode != 0 {
		t.Errorf("status = %d, want 0 when no response arrived", res.StatusCode)
	}
}

func TestProbeUnresolvableHost(t *testing.T) {
	c := checkFor(t, "http://lookout-test.invalid/", "")
	res := NewHTTP().Probe(context.Background(), c)
	if res.Outcome != check.OutcomeDown {
		t.Fatalf("outcome = %q, want %q", res.Outcome, check.OutcomeDown)
	}
	if res.Err == "" {
		t.Error("a transport failure must be explained")
	}
}

func TestProbeSendsMethodAndHeaders(t *testing.T) {
	t.Setenv("LOOKOUT_TEST_SECRET", "s3cret")
	var gotMethod, gotAuth string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotAuth = r.Method, r.Header.Get("Authorization")
		w.Write([]byte("ok"))
	})
	c := checkFor(t, srv.URL, "    method: head\n    headers:\n      Authorization: \"Basic ${LOOKOUT_TEST_SECRET}\"\n")
	if res := NewHTTP().Probe(context.Background(), c); res.Outcome != check.OutcomeUp {
		t.Fatalf("outcome = %q: %s", res.Outcome, res.Reason())
	}
	if gotMethod != "HEAD" {
		t.Errorf("method = %q, want %q", gotMethod, "HEAD")
	}
	if gotAuth != "Basic s3cret" {
		t.Errorf("Authorization = %q, want the expanded secret", gotAuth)
	}
}

// Every probe must open a fresh connection, otherwise it stops covering DNS,
// TCP and TLS after the first one.
func TestProbeDoesNotReuseConnections(t *testing.T) {
	remotes := make(map[string]bool)
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		remotes[r.RemoteAddr] = true
		w.Write([]byte("ok"))
	})
	p := NewHTTP()
	c := checkFor(t, srv.URL, "")
	for range 3 {
		if res := p.Probe(context.Background(), c); res.Outcome != check.OutcomeUp {
			t.Fatalf("probe failed: %s", res.Reason())
		}
	}
	if len(remotes) != 3 {
		t.Errorf("got %d distinct client ports, want 3: connections are being reused", len(remotes))
	}
}

func TestProbeTruncatesLargeBody(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"pad":"` + strings.Repeat("x", maxBody) + `"}`))
	})
	c := checkFor(t, srv.URL, "    expect:\n      body:\n        \".pad\": \"x\"\n")
	res := NewHTTP().Probe(context.Background(), c)
	if res.Outcome != check.OutcomeMalformed {
		t.Fatalf("outcome = %q, want %q", res.Outcome, check.OutcomeMalformed)
	}
	if !strings.Contains(res.Reason(), "truncated") {
		t.Errorf("reason = %q, want it to mention truncation", res.Reason())
	}
	if len([]byte(res.BodySample)) > sampleBytes+4 {
		t.Errorf("body sample is %d bytes, want at most %d", len(res.BodySample), sampleBytes)
	}
}

func TestProbeHonoursCancelledContext(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := NewHTTP().Probe(ctx, checkFor(t, srv.URL, ""))
	if res.Err != "probe cancelled" {
		t.Errorf("err = %q, want %q", res.Err, "probe cancelled")
	}
}
