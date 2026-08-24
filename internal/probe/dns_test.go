package probe

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
	"golang.org/x/net/dns/dnsmessage"
)

func dnsCheck(t *testing.T, extra string) config.Check {
	t.Helper()
	src := "checks:\n  - name: T\n    type: dns\n    host: service.example\n    query_type: A\n    interval: 5m\n    timeout: 2s\n" + extra
	cfg, err := config.Load("config.yaml", []byte(src))
	if err != nil {
		t.Fatalf("loading test config: %v", err)
	}
	return cfg.Checks[0]
}

type dnsReply func(q dnsmessage.Question) (dnsmessage.RCode, []dnsmessage.Resource)

func startDNS(t *testing.T, reply dnsReply) (addr string, queries *atomic.Int32) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	var n atomic.Int32
	go func() {
		buf := make([]byte, 4096)
		for {
			nr, raddr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			n.Add(1)
			var msg dnsmessage.Message
			if err := msg.Unpack(buf[:nr]); err != nil || len(msg.Questions) == 0 {
				continue
			}
			if reply == nil {
				continue // drop
			}
			rcode, answers := reply(msg.Questions[0])
			resp := dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 msg.ID,
					Response:           true,
					RecursionDesired:   msg.RecursionDesired,
					RecursionAvailable: true,
					RCode:              rcode,
				},
				Questions: msg.Questions,
				Answers:   answers,
			}
			packed, err := resp.Pack()
			if err != nil {
				continue
			}
			_, _ = pc.WriteTo(packed, raddr)
		}
	}()
	return pc.LocalAddr().String(), &n
}

func aRR(name, ip string) dnsmessage.Resource {
	var a [4]byte
	copy(a[:], net.ParseIP(ip).To4())
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{
			Name:  dnsmessage.MustNewName(name),
			Type:  dnsmessage.TypeA,
			Class: dnsmessage.ClassINET,
			TTL:   60,
		},
		Body: &dnsmessage.AResource{A: a},
	}
}

func mxRR(name string, pref uint16, mx string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{
			Name:  dnsmessage.MustNewName(name),
			Type:  dnsmessage.TypeMX,
			Class: dnsmessage.ClassINET,
			TTL:   60,
		},
		Body: &dnsmessage.MXResource{Pref: pref, MX: dnsmessage.MustNewName(mx)},
	}
}

func TestDNSProbeNOERROR(t *testing.T) {
	addr, _ := startDNS(t, func(q dnsmessage.Question) (dnsmessage.RCode, []dnsmessage.Resource) {
		if q.Type != dnsmessage.TypeA {
			t.Errorf("query type = %v, want A", q.Type)
		}
		return dnsmessage.RCodeSuccess, []dnsmessage.Resource{aRR("service.example.", "192.0.2.10")}
	})
	c := dnsCheck(t, "    resolver: "+addr+"\n")
	res := NewDNS().Probe(context.Background(), c)
	if res.Outcome != check.OutcomeUp {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason())
	}
	if res.Rcode != "NOERROR" {
		t.Errorf("rcode = %q", res.Rcode)
	}
	if len(res.Answers) != 1 || res.Answers[0] != "A 192.0.2.10" {
		t.Errorf("answers = %v", res.Answers)
	}
	if res.ZoneSnapshot != "NOERROR\nA 192.0.2.10" {
		t.Errorf("snapshot = %q", res.ZoneSnapshot)
	}
}

func TestDNSProbeRcodeMismatch(t *testing.T) {
	addr, _ := startDNS(t, func(dnsmessage.Question) (dnsmessage.RCode, []dnsmessage.Resource) {
		return dnsmessage.RCodeNameError, nil
	})
	c := dnsCheck(t, "    resolver: "+addr+"\n")
	res := NewDNS().Probe(context.Background(), c)
	if res.Outcome != check.OutcomeDown {
		t.Fatalf("outcome = %q, want down", res.Outcome)
	}
	if !strings.Contains(res.Reason(), "rcode: want NOERROR, got NXDOMAIN") {
		t.Errorf("reason = %q", res.Reason())
	}
	if res.ZoneSnapshot != "NXDOMAIN" {
		t.Errorf("snapshot = %q, want NXDOMAIN so a wipe is visible", res.ZoneSnapshot)
	}
}

func TestDNSProbeAnswersContain(t *testing.T) {
	addr, _ := startDNS(t, func(dnsmessage.Question) (dnsmessage.RCode, []dnsmessage.Resource) {
		return dnsmessage.RCodeSuccess, []dnsmessage.Resource{
			mxRR("service.example.", 10, "mail.service.example."),
			mxRR("service.example.", 20, "backup.service.example."),
		}
	})
	src := `
checks:
  - name: T
    type: dns
    host: service.example
    query_type: MX
    resolver: ` + addr + `
    expect:
      answers_contain: mail.service.example
`
	cfg, err := config.Load("config.yaml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	res := NewDNS().Probe(context.Background(), cfg.Checks[0])
	if res.Outcome != check.OutcomeUp {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason())
	}
	if !strings.Contains(res.ZoneSnapshot, "MX 10 mail.service.example.") {
		t.Errorf("snapshot = %q", res.ZoneSnapshot)
	}
}

func TestDNSProbeRetriesAfterDroppedUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	var n atomic.Int32
	go func() {
		buf := make([]byte, 4096)
		for {
			nr, raddr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := n.Add(1)
			if q == 1 {
				continue // drop the first datagram
			}
			var msg dnsmessage.Message
			if err := msg.Unpack(buf[:nr]); err != nil {
				continue
			}
			resp := dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:       msg.ID,
					Response: true,
					RCode:    dnsmessage.RCodeSuccess,
				},
				Questions: msg.Questions,
				Answers:   []dnsmessage.Resource{aRR("service.example.", "192.0.2.10")},
			}
			packed, err := resp.Pack()
			if err != nil {
				continue
			}
			_, _ = pc.WriteTo(packed, raddr)
		}
	}()

	c := dnsCheck(t, "    resolver: "+pc.LocalAddr().String()+"\n")
	c.Timeout = time.Second
	res := NewDNS().Probe(context.Background(), c)
	if res.Outcome != check.OutcomeUp {
		t.Fatalf("outcome = %q (%s); retries should have recovered from one lost UDP packet", res.Outcome, res.Reason())
	}
	if n.Load() < 2 {
		t.Errorf("resolver saw %d queries, want at least 2 (one drop + one retry)", n.Load())
	}
}

func TestDNSProbeTimeoutAfterRetries(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	var n atomic.Int32
	go func() {
		buf := make([]byte, 4096)
		for {
			_, _, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			n.Add(1)
			// drop every packet
		}
	}()
	c := dnsCheck(t, "    resolver: "+pc.LocalAddr().String()+"\n")
	c.Timeout = 200 * time.Millisecond
	res := NewDNS().Probe(context.Background(), c)
	if res.Outcome != check.OutcomeDown {
		t.Fatalf("outcome = %q, want down", res.Outcome)
	}
	if !strings.Contains(res.Err, "timed out") {
		t.Errorf("err = %q, want a timeout", res.Err)
	}
	if n.Load() < 2 {
		t.Errorf("resolver saw %d queries, want retries against silence", n.Load())
	}
}

func TestDNSProbeDoesNotRetryDecodedRcode(t *testing.T) {
	addr, n := startDNS(t, func(dnsmessage.Question) (dnsmessage.RCode, []dnsmessage.Resource) {
		return dnsmessage.RCodeServerFailure, nil
	})
	c := dnsCheck(t, "    resolver: "+addr+"\n")
	res := NewDNS().Probe(context.Background(), c)
	if res.Rcode != "SERVFAIL" {
		t.Errorf("rcode = %q", res.Rcode)
	}
	if n.Load() != 1 {
		t.Errorf("queries = %d, want 1: a decoded SERVFAIL is not packet loss", n.Load())
	}
}

func TestDNSProbeCancelled(t *testing.T) {
	addr, _ := startDNS(t, func(dnsmessage.Question) (dnsmessage.RCode, []dnsmessage.Resource) {
		time.Sleep(time.Second)
		return dnsmessage.RCodeSuccess, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := dnsCheck(t, "    resolver: "+addr+"\n")
	res := NewDNS().Probe(ctx, c)
	if res.Err != "probe cancelled" && !strings.Contains(res.Err, "canceled") && !strings.Contains(res.Err, "cancelled") {
		t.Errorf("err = %q", res.Err)
	}
}

func TestSystemResolverReadsResolvConf(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(path, []byte("# comment\nnameserver 192.0.2.53\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolverFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "192.0.2.53:53" {
		t.Errorf("resolver = %q", got)
	}
}

func TestZoneSnapshotSortsAnswers(t *testing.T) {
	got := zoneSnapshot("NOERROR", []string{"A 192.0.2.20", "A 192.0.2.10"})
	if got != "NOERROR\nA 192.0.2.10\nA 192.0.2.20" {
		t.Errorf("snapshot = %q", got)
	}
}

func TestZoneSnapshotOnlyForZoneAnswers(t *testing.T) {
	if got := zoneSnapshot("NXDOMAIN", nil); got != "NXDOMAIN" {
		t.Errorf("NXDOMAIN snapshot = %q, want the wipe to be comparable", got)
	}
	for _, rcode := range []string{"SERVFAIL", "REFUSED", "FORMERR", "RCODE9"} {
		if got := zoneSnapshot(rcode, []string{"A 192.0.2.10"}); got != "" {
			t.Errorf("%s snapshot = %q, want none: the resolver did not answer", rcode, got)
		}
	}
}

func TestDNSProbeServfailLeavesNoSnapshot(t *testing.T) {
	addr, _ := startDNS(t, func(dnsmessage.Question) (dnsmessage.RCode, []dnsmessage.Resource) {
		return dnsmessage.RCodeServerFailure, nil
	})
	c := dnsCheck(t, "    resolver: "+addr+"\n")
	res := NewDNS().Probe(context.Background(), c)
	if res.Outcome != check.OutcomeDown || res.Rcode != "SERVFAIL" {
		t.Fatalf("outcome = %q rcode = %q, want a failed check", res.Outcome, res.Rcode)
	}
	if res.ZoneSnapshot != "" {
		t.Errorf("snapshot = %q, want none: a SERVFAIL must not read as a zone change", res.ZoneSnapshot)
	}
}
