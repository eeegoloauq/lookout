package probe

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
	"github.com/eeegoloauq/lookout/internal/registry"
)

func domainCheck(t *testing.T, name string) config.Check {
	t.Helper()
	src := "checks:\n  - name: T\n    type: domain\n    domain: " + name + "\n    interval: 24h\n    timeout: 2s\n"
	cfg, err := config.Load("config.yaml", []byte(src))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg.Checks[0]
}

func startWHOIS(t *testing.T, reply string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 256)
				_, _ = c.Read(buf) // one query line; do not wait for EOF
				_, _ = io.WriteString(c, reply)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func TestDomainProbeRDAP(t *testing.T) {
	rdapJSON, err := os.ReadFile("../registry/testdata/rdap_com.json")
	if err != nil {
		t.Fatal(err)
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/rdap/dns.json":
			base := srv.URL + "/rdap/"
			_, _ = w.Write([]byte(`{"publication":"2026-07-23T02:00:03Z","services":[[["example"],["` + base + `"]]]}`))
		case strings.HasPrefix(r.URL.Path, "/rdap/domain/"):
			_, _ = w.Write(rdapJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	d := NewDomain()
	d.client = srv.Client()
	d.bootstrapURL = srv.URL + "/rdap/dns.json"
	d.ianaWHOIS = "127.0.0.1:1" // must not be consulted

	res := d.Probe(context.Background(), domainCheck(t, "service.example"))
	if res.Outcome != check.OutcomeUp {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason())
	}
	if res.DomainSource != registry.SourceRDAP {
		t.Errorf("source = %q", res.DomainSource)
	}
	if res.DomainExpiresAt.Year() != 2027 {
		t.Errorf("expires = %s", res.DomainExpiresAt)
	}
	if !d.Dirty() {
		t.Error("a freshly fetched bootstrap must mark the cache dirty")
	}
}

func TestDomainProbeWHOISFallbackForUnlistedTLD(t *testing.T) {
	whoisText, err := os.ReadFile("../registry/testdata/whois_ru.txt")
	if err != nil {
		t.Fatal(err)
	}
	addr := startWHOIS(t, string(whoisText))

	// Bootstrap with no "example" TLD → RDAP miss → WHOIS via the
	// built-in table? "example" is not in the tcinet table, so we
	// seed the cache as a test-only referral.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"publication":"2026-07-23T02:00:03Z","services":[[["com"],["https://rdap.registry.example/com/v1/"]]]}`))
	}))
	t.Cleanup(srv.Close)

	d := NewDomain()
	d.client = srv.Client()
	d.bootstrapURL = srv.URL
	d.rememberWHOIS("example", addr)

	res := d.Probe(context.Background(), domainCheck(t, "service.example"))
	if res.Outcome != check.OutcomeUp {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason())
	}
	if res.DomainSource != registry.SourceWHOIS {
		t.Errorf("source = %q, want whois", res.DomainSource)
	}
	if res.DomainState != "REGISTERED, DELEGATED, VERIFIED" {
		t.Errorf("state = %q", res.DomainState)
	}
	if res.DomainExpiresAt.Year() != 2027 || res.DomainFreeDate.Month() != time.June {
		t.Errorf("dates expire=%s free=%s", res.DomainExpiresAt, res.DomainFreeDate)
	}
}

func TestDomainProbeTcinetDefaultTable(t *testing.T) {
	whoisText, err := os.ReadFile("../registry/testdata/whois_ru.txt")
	if err != nil {
		t.Fatal(err)
	}
	addr := startWHOIS(t, string(whoisText))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"publication":"2026-07-23T02:00:03Z","services":[[["com"],["https://rdap.registry.example/"]]]}`))
	}))
	t.Cleanup(srv.Close)

	d := NewDomain()
	d.client = srv.Client()
	d.bootstrapURL = srv.URL
	// Override the built-in host by preloading the cache under "ru".
	// The production table points at whois.tcinet.ru; tests must not
	// open a real connection, so the cache wins.
	d.rememberWHOIS("ru", addr)

	res := d.Probe(context.Background(), domainCheck(t, "service.example.ru"))
	if res.Outcome != check.OutcomeUp {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason())
	}
	if res.DomainSource != registry.SourceWHOIS {
		t.Errorf("source = %q", res.DomainSource)
	}
}

func TestDomainProbeUnavailableIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	d := NewDomain()
	d.client = srv.Client()
	d.bootstrapURL = srv.URL
	d.ianaWHOIS = "127.0.0.1:1"

	res := d.Probe(context.Background(), domainCheck(t, "service.example"))
	if res.Outcome != check.OutcomeUnknown {
		t.Fatalf("outcome = %q (%s), want unknown — a dead registry is not a dead domain", res.Outcome, res.Err)
	}
}

func TestDomainProbeUnparseableWHOISIsMalformed(t *testing.T) {
	addr := startWHOIS(t, "domain: SERVICE.EXAMPLE\nregistrar: Example\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"publication":"2026-07-23T02:00:03Z","services":[[["com"],["https://rdap.registry.example/"]]]}`))
	}))
	t.Cleanup(srv.Close)

	d := NewDomain()
	d.client = srv.Client()
	d.bootstrapURL = srv.URL
	d.rememberWHOIS("example", addr)

	res := d.Probe(context.Background(), domainCheck(t, "service.example"))
	if res.Outcome != check.OutcomeMalformed {
		t.Fatalf("outcome = %q (%s), want malformed — we got a reply we cannot read", res.Outcome, res.Err)
	}
	if !strings.Contains(res.Err, "no recognisable expiry field") {
		t.Errorf("err = %q", res.Err)
	}
}

func TestDomainProbeRDAP404FallsBackToWHOIS(t *testing.T) {
	whoisText, err := os.ReadFile("../registry/testdata/whois_com.txt")
	if err != nil {
		t.Fatal(err)
	}
	addr := startWHOIS(t, string(whoisText))

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dns.json" {
			_, _ = w.Write([]byte(`{"publication":"2026-07-23T02:00:03Z","services":[[["example"],["` + srv.URL + `/rdap/"]]]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	d := NewDomain()
	d.client = srv.Client()
	d.bootstrapURL = srv.URL + "/dns.json"
	d.rememberWHOIS("example", addr)

	res := d.Probe(context.Background(), domainCheck(t, "service.example"))
	if res.Outcome != check.OutcomeUp {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason())
	}
	if res.DomainSource != registry.SourceWHOIS {
		t.Errorf("source = %q after RDAP 404", res.DomainSource)
	}
}
