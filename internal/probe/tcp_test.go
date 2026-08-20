package probe

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
)

func mustDuration(t *testing.T, s string) config.DurationMatcher {
	t.Helper()
	m, err := config.ParseDurationMatcher(s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTCPDialSucceeds(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	res := NewTCP().Probe(context.Background(), config.Check{
		Name: "Port", Type: config.TypeTCP, Address: ln.Addr().String(), Timeout: 2 * time.Second,
	})
	if res.Outcome != check.OutcomeUp {
		t.Fatalf("outcome = %q, err = %q", res.Outcome, res.Err)
	}
	if res.RemoteAddr != ln.Addr().String() {
		t.Errorf("remote = %q, want %q", res.RemoteAddr, ln.Addr())
	}
	if res.Duration <= 0 {
		t.Error("duration was not measured")
	}
}

// Nothing listening is the whole point of the check: it has to be a result,
// not an error, and it has to carry a reason a human can read.
func TestTCPRefusedIsDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	res := NewTCP().Probe(context.Background(), config.Check{
		Name: "Port", Type: config.TypeTCP, Address: addr, Timeout: 2 * time.Second,
	})
	if res.Outcome != check.OutcomeDown {
		t.Fatalf("outcome = %q, want down", res.Outcome)
	}
	if res.Err == "" {
		t.Error("a refused connection said nothing about why")
	}
}

// A port that accepts but answers slowly is a real failure mode: the check
// has to be able to fail on time as well as on reachability.
func TestTCPResponseTimeCondition(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	c := config.Check{
		Name: "Port", Type: config.TypeTCP, Address: ln.Addr().String(), Timeout: 2 * time.Second,
		Expect: config.Expect{ResponseTime: mustDuration(t, "<1ns")},
	}
	res := NewTCP().Probe(context.Background(), c)
	if res.Outcome != check.OutcomeDown {
		t.Fatalf("outcome = %q, want down on an impossible response_time", res.Outcome)
	}
	if len(res.Failures) != 1 || res.Failures[0].Condition != "response_time" {
		t.Errorf("failures = %+v", res.Failures)
	}
}
