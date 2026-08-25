package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
	"github.com/eeegoloauq/lookout/internal/registry"
	"github.com/eeegoloauq/lookout/internal/state"
)

const (
	// ianaBootstrapURL is the official TLD → RDAP map. Public
	// aggregators such as rdap.org are a redirector on the critical
	// path and are not used.
	ianaBootstrapURL = "https://data.iana.org/rdap/dns.json"
	ianaWHOISAddr    = "whois.iana.org:43"
	bootstrapTTL     = 7 * 24 * time.Hour
	maxWHOIS         = 64 << 10
	maxRDAPBody      = 512 << 10
)

// Domain probes checks of type domain: registry expiry via RDAP, with
// WHOIS on TCP/43 as the fallback for zones that have no RDAP (.ru).
type Domain struct {
	client       *http.Client
	dial         func(ctx context.Context, network, address string) (net.Conn, error)
	now          func() time.Time
	bootstrapURL string
	ianaWHOIS    string

	mu    sync.Mutex
	cache state.RegistryCache
	dirty bool
}

// NewDomain returns a production domain prober.
func NewDomain() *Domain {
	return &Domain{
		client:       &http.Client{Transport: Transport()},
		dial:         defaultDial,
		now:          time.Now,
		bootstrapURL: ianaBootstrapURL,
		ianaWHOIS:    ianaWHOISAddr,
	}
}

// LoadCache restores the bootstrap and WHOIS referrals from durable state.
func (d *Domain) LoadCache(c state.RegistryCache) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cache = cloneRegistry(c)
	d.dirty = false
}

// Cache returns a copy of the current registry cache.
func (d *Domain) Cache() state.RegistryCache {
	d.mu.Lock()
	defer d.mu.Unlock()
	return cloneRegistry(d.cache)
}

func (d *Domain) Dirty() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dirty
}

func (d *Domain) ClearDirty() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dirty = false
}

func cloneRegistry(c state.RegistryCache) state.RegistryCache {
	out := state.RegistryCache{RDAPFetchedAt: c.RDAPFetchedAt}
	if c.RDAP != nil {
		out.RDAP = make(map[string]string, len(c.RDAP))
		for k, v := range c.RDAP {
			out.RDAP[k] = v
		}
	}
	if c.WHOIS != nil {
		out.WHOIS = make(map[string]string, len(c.WHOIS))
		for k, v := range c.WHOIS {
			out.WHOIS[k] = v
		}
	}
	return out
}

// Probe looks up the registered name. A registry that does not answer is
// OutcomeUnknown, never Down: "I could not ask" is not "the domain fell
// over".
func (d *Domain) Probe(ctx context.Context, c config.Check) check.Result {
	res := check.Result{Name: c.Name, At: d.now()}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	name := c.Host
	if name == "" {
		res.Outcome = check.OutcomeDown
		res.Err = "domain name is empty"
		res.Duration = d.now().Sub(res.At)
		return res
	}

	start := d.now()
	rec, err := d.lookup(ctx, name)
	res.Duration = d.now().Sub(start)
	if err != nil {
		res.Err = err.Error()
		if isUnavailable(err) {
			res.Outcome = check.OutcomeUnknown
		} else {
			res.Outcome = check.OutcomeMalformed
		}
		return res
	}

	res.DomainExpiresAt = rec.Expires
	res.DomainFreeDate = rec.FreeDate
	res.DomainState = rec.State
	res.DomainSource = rec.Source

	outcome, failures := check.Evaluate(c.Expect, check.Response{Duration: res.Duration})
	res.Outcome = outcome
	res.Failures = failures
	return res
}

type unavailableError struct{ err error }

func (e unavailableError) Error() string { return e.err.Error() }
func (e unavailableError) Unwrap() error { return e.err }

func isUnavailable(err error) bool {
	var u unavailableError
	return errors.As(err, &u) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func (d *Domain) lookup(ctx context.Context, name string) (registry.Record, error) {
	tld := registry.TLD(name)
	if base, ok := d.rdapBase(ctx, tld); ok {
		rec, err := d.rdap(ctx, base, name)
		if err == nil {
			return rec, nil
		}
		// 404 / parse / transport: fall through to WHOIS. A TLD in
		// the bootstrap can still refuse a particular name.
		if !isUnavailable(err) {
			// Keep going; WHOIS may still know the date.
		} else {
			// Transport failed. Try WHOIS before giving up: .ru
			// is not in the bootstrap, but a listed TLD with a
			// dead RDAP host should still be askable over 43.
		}
	}
	return d.whois(ctx, name, tld)
}

func (d *Domain) rdapBase(ctx context.Context, tld string) (string, bool) {
	if err := d.ensureBootstrap(ctx); err != nil {
		// Stale cache is still usable.
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cache.RDAP == nil {
		return "", false
	}
	base, ok := d.cache.RDAP[tld]
	return base, ok && base != ""
}

func (d *Domain) ensureBootstrap(ctx context.Context) error {
	d.mu.Lock()
	fetched := d.cache.RDAPFetchedAt
	have := len(d.cache.RDAP) > 0
	d.mu.Unlock()
	now := d.now()
	if have && !fetched.IsZero() && now.Sub(fetched) < bootstrapTTL {
		return nil
	}
	body, err := d.get(ctx, d.bootstrapURL)
	if err != nil {
		if have {
			return nil
		}
		return unavailableError{fmt.Errorf("IANA RDAP bootstrap: %w", err)}
	}
	boot, err := registry.ParseBootstrap(body)
	if err != nil {
		if have {
			return nil
		}
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cache.RDAP = boot.Services
	d.cache.RDAPFetchedAt = now
	d.dirty = true
	return nil
}

func (d *Domain) rdap(ctx context.Context, base, name string) (registry.Record, error) {
	u, err := url.JoinPath(base, "domain", name)
	if err != nil {
		return registry.Record{}, err
	}
	body, err := d.get(ctx, u)
	if err != nil {
		return registry.Record{}, err
	}
	return registry.ParseRDAP(body)
}

func (d *Domain) get(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/rdap+json, application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, unavailableError{err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRDAPBody+1))
	if err != nil {
		return nil, unavailableError{err}
	}
	if len(body) > maxRDAPBody {
		body = body[:maxRDAPBody]
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("RDAP 404")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("RDAP HTTP %d", resp.StatusCode)
	}
	return body, nil
}

func (d *Domain) whois(ctx context.Context, name, tld string) (registry.Record, error) {
	host, err := d.whoisServer(ctx, tld)
	if err != nil {
		return registry.Record{}, err
	}
	text, err := d.whoisQuery(ctx, host, name)
	if err != nil {
		return registry.Record{}, unavailableError{fmt.Errorf("whois %s: %w", host, err)}
	}
	return registry.ParseWHOIS(text)
}

func (d *Domain) whoisServer(ctx context.Context, tld string) (string, error) {
	d.mu.Lock()
	if d.cache.WHOIS != nil {
		if host := d.cache.WHOIS[tld]; host != "" {
			d.mu.Unlock()
			return host, nil
		}
	}
	d.mu.Unlock()
	if host, ok := registry.DefaultWHOIS(tld); ok {
		d.rememberWHOIS(tld, host)
		return host, nil
	}
	text, err := d.whoisQuery(ctx, d.ianaWHOIS, tld)
	if err != nil {
		return "", unavailableError{fmt.Errorf("IANA whois referral for %s: %w", tld, err)}
	}
	host, err := registry.ParseIANAReferral(text)
	if err != nil {
		return "", err
	}
	d.rememberWHOIS(tld, host)
	return host, nil
}

func (d *Domain) rememberWHOIS(tld, host string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cache.WHOIS == nil {
		d.cache.WHOIS = map[string]string{}
	}
	if d.cache.WHOIS[tld] != host {
		d.cache.WHOIS[tld] = host
		d.dirty = true
	}
}

func (d *Domain) whoisQuery(ctx context.Context, addr, query string) (string, error) {
	if !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(addr, "43")
	}
	dial := d.dial
	if dial == nil {
		dial = defaultDial
	}
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := io.WriteString(conn, query+"\r\n"); err != nil {
		return "", err
	}
	body, err := io.ReadAll(io.LimitReader(conn, maxWHOIS+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxWHOIS {
		body = body[:maxWHOIS]
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return "", errors.New("empty reply")
	}
	return string(body), nil
}
