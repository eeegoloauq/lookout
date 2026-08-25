package probe

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
	"golang.org/x/net/dns/dnsmessage"
)

const (
	// dnsAttempts is why this probe exists as its own type. One lost UDP
	// datagram is not an outage; Gatus treated it as one, and that is
	// the failure this retry budget is here to stop.
	dnsAttempts = 3
	// maxDNSResponse is well above a classic 512-byte UDP payload and
	// above a typical EDNS 1232-byte one. Anything larger is not a
	// homelab A/MX/NS/TXT answer.
	maxDNSResponse = 4096
)

// DNS probes checks of type dns.
type DNS struct {
	// Dial opens a connected UDP socket to the resolver. Tests replace
	// it; the default is the process network.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// LookupResolver returns host:port of the system nameserver when a
	// check does not name one. Tests replace it so they never read
	// /etc/resolv.conf.
	LookupResolver func() (string, error)
}

// NewDNS returns a prober that speaks DNS over UDP.
func NewDNS() *DNS {
	return &DNS{
		Dial:           defaultDial,
		LookupResolver: systemResolver,
	}
}

func defaultDial(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

// Probe runs one DNS check. It never returns an error: a failed probe is a
// result, not an exception. Transport failures are retried; a decoded
// response — including NXDOMAIN or SERVFAIL — is final.
func (d *DNS) Probe(ctx context.Context, c config.Check) check.Result {
	res := check.Result{Name: c.Name, At: time.Now()}

	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	resolver, err := d.resolverAddr(c)
	if err != nil {
		res.Outcome = check.OutcomeDown
		res.Err = err.Error()
		res.Duration = time.Since(res.At)
		return res
	}

	qtype, err := dnsType(c.QueryType)
	if err != nil {
		res.Outcome = check.OutcomeDown
		res.Err = err.Error()
		res.Duration = time.Since(res.At)
		return res
	}

	start := time.Now()
	msg, err := d.query(ctx, resolver, c.Host, qtype)
	res.Duration = time.Since(start)
	if err != nil {
		res.Outcome = check.OutcomeDown
		res.Err = dnsTransportError(err, c.Timeout)
		return res
	}

	rcode := rcodeName(msg.RCode)
	answers := renderAnswers(msg.Answers)
	res.Rcode = rcode
	res.Answers = answers
	res.ZoneSnapshot = zoneSnapshot(rcode, answers)

	outcome, failures := check.Evaluate(c.Expect, check.Response{
		Rcode:    rcode,
		Answers:  answers,
		Duration: res.Duration,
	})
	res.Outcome = outcome
	res.Failures = failures
	return res
}

func (d *DNS) resolverAddr(c config.Check) (string, error) {
	if c.Resolver != "" {
		return c.Resolver, nil
	}
	lookup := d.LookupResolver
	if lookup == nil {
		lookup = systemResolver
	}
	addr, err := lookup()
	if err != nil {
		return "", fmt.Errorf("no DNS resolver configured: %w", err)
	}
	return addr, nil
}

func (d *DNS) query(ctx context.Context, resolver, host string, qtype dnsmessage.Type) (dnsmessage.Message, error) {
	dial := d.Dial
	if dial == nil {
		dial = defaultDial
	}

	var last error
	for attempt := 0; attempt < dnsAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if last != nil {
				return dnsmessage.Message{}, last
			}
			return dnsmessage.Message{}, err
		}
		msg, err := d.oneQuery(ctx, dial, resolver, host, qtype, attempt)
		if err == nil {
			return msg, nil
		}
		last = err
	}
	return dnsmessage.Message{}, last
}

func (d *DNS) oneQuery(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), resolver, host string, qtype dnsmessage.Type, attempt int) (dnsmessage.Message, error) {
	id := dnsID()
	packed, err := packQuery(id, host, qtype)
	if err != nil {
		return dnsmessage.Message{}, err
	}

	// Split the remaining budget across the leftover attempts so a
	// single hung datagram cannot eat the whole timeout.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return dnsmessage.Message{}, context.DeadlineExceeded
	}
	left := dnsAttempts - attempt
	if left < 1 {
		left = 1
	}
	slice := remaining / time.Duration(left)
	if slice < 50*time.Millisecond {
		slice = remaining
	}
	attemptCtx, cancel := context.WithTimeout(ctx, slice)
	defer cancel()

	conn, err := dial(attemptCtx, "udp", resolver)
	if err != nil {
		return dnsmessage.Message{}, err
	}
	defer conn.Close()
	if dl, ok := attemptCtx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	if _, err := conn.Write(packed); err != nil {
		return dnsmessage.Message{}, err
	}
	buf := make([]byte, maxDNSResponse)
	n, err := conn.Read(buf)
	if err != nil {
		return dnsmessage.Message{}, err
	}

	var msg dnsmessage.Message
	if err := msg.Unpack(buf[:n]); err != nil {
		return dnsmessage.Message{}, fmt.Errorf("DNS response could not be parsed: %w", err)
	}
	if !msg.Response {
		return dnsmessage.Message{}, errors.New("DNS message is not a response")
	}
	if msg.ID != id {
		return dnsmessage.Message{}, fmt.Errorf("DNS response id %d does not match query %d", msg.ID, id)
	}
	return msg, nil
}

func packQuery(id uint16, host string, qtype dnsmessage.Type) ([]byte, error) {
	name, err := dnsmessage.NewName(strings.TrimSuffix(host, ".") + ".")
	if err != nil {
		return nil, fmt.Errorf("DNS name %q is not valid: %w", host, err)
	}
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name:  name,
			Type:  qtype,
			Class: dnsmessage.ClassINET,
		}},
	}
	return msg.Pack()
}

func dnsID() uint16 {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint16(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint16(b[:])
}

func dnsType(q config.QueryType) (dnsmessage.Type, error) {
	switch q {
	case config.QueryA:
		return dnsmessage.TypeA, nil
	case config.QueryAAAA:
		return dnsmessage.TypeAAAA, nil
	case config.QueryMX:
		return dnsmessage.TypeMX, nil
	case config.QueryNS:
		return dnsmessage.TypeNS, nil
	case config.QueryTXT:
		return dnsmessage.TypeTXT, nil
	default:
		return 0, fmt.Errorf("unsupported query type %q", q)
	}
}

func rcodeName(c dnsmessage.RCode) string {
	switch c {
	case dnsmessage.RCodeSuccess:
		return "NOERROR"
	case dnsmessage.RCodeFormatError:
		return "FORMERR"
	case dnsmessage.RCodeServerFailure:
		return "SERVFAIL"
	case dnsmessage.RCodeNameError:
		return "NXDOMAIN"
	case dnsmessage.RCodeNotImplemented:
		return "NOTIMP"
	case dnsmessage.RCodeRefused:
		return "REFUSED"
	default:
		return fmt.Sprintf("RCODE%d", uint16(c))
	}
}

func renderAnswers(rrs []dnsmessage.Resource) []string {
	out := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		if line := renderRecord(rr); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func renderRecord(rr dnsmessage.Resource) string {
	switch b := rr.Body.(type) {
	case *dnsmessage.AResource:
		return "A " + net.IP(b.A[:]).String()
	case *dnsmessage.AAAAResource:
		return "AAAA " + net.IP(b.AAAA[:]).String()
	case *dnsmessage.MXResource:
		return fmt.Sprintf("MX %d %s", b.Pref, canonicalDNSName(b.MX.String()))
	case *dnsmessage.NSResource:
		return "NS " + canonicalDNSName(b.NS.String())
	case *dnsmessage.TXTResource:
		return "TXT " + strings.Join(b.TXT, "")
	case *dnsmessage.CNAMEResource:
		return "CNAME " + canonicalDNSName(b.CNAME.String())
	default:
		return ""
	}
}

func canonicalDNSName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "."
	}
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// zoneSnapshot is the comparable form stored in durable state. Rcode is
// on the first line so NXDOMAIN and an empty NOERROR are distinct; answers
// are sorted so record order is not a change.
//
// Only NOERROR and NXDOMAIN produce a snapshot, because only they say
// something about the zone: these are the records, or the name is gone. A
// SERVFAIL or REFUSED says the resolver could not answer, and snapshotting it
// reports drift twice — once on the failure, once on the recovery — for a zone
// nobody touched. A zone that is genuinely broken (a bad signature, a dead
// authoritative server) still surfaces as a down check on the rcode condition,
// which is the event that actually happened.
func zoneSnapshot(rcode string, answers []string) string {
	if rcode != "NOERROR" && rcode != "NXDOMAIN" {
		return ""
	}
	sorted := append([]string(nil), answers...)
	sort.Strings(sorted)
	var b strings.Builder
	b.WriteString(rcode)
	for _, a := range sorted {
		b.WriteByte('\n')
		b.WriteString(a)
	}
	return b.String()
}

func dnsTransportError(err error, timeout time.Duration) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out after " + timeout.String()
	case errors.Is(err, context.Canceled):
		return "probe cancelled"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timed out after " + timeout.String()
	}
	return "DNS query failed: " + err.Error()
}

// systemResolver returns the first nameserver in /etc/resolv.conf. The
// default is the LAN resolver, not a public one: a homelab monitor that
// silently phones 8.8.8.8 is asking a different question from "is my
// resolver answering" (research O1).
func systemResolver() (string, error) {
	return resolverFromFile("/etc/resolv.conf")
}

func resolverFromFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		host, port, err := splitResolverAddr(fields[1])
		if err != nil {
			continue
		}
		return net.JoinHostPort(host, port), nil
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", errors.New("no nameserver in " + path)
}

func splitResolverAddr(addr string) (host, port string, err error) {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return net.SplitHostPort(addr)
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return "", "", fmt.Errorf("not an address: %s", addr)
	}
	return addr, "53", nil
}
