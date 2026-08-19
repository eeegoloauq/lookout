// Package probe executes checks against their targets.
package probe

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
)

const (
	// maxBody caps what a probe reads. Bodies are only ever inspected by JSON
	// paths and substring matches; a monitor must not be made to allocate a
	// gigabyte by a target that misbehaves.
	maxBody = 512 << 10
	// sampleBytes is how much of the body an alert carries (SPEC §6).
	sampleBytes = 200
)

// HTTP probes checks of type http.
type HTTP struct {
	client *http.Client
}

// NewHTTP returns a prober using a transport tuned for monitoring rather than
// for throughput.
func NewHTTP() *HTTP {
	return NewHTTPWithClient(&http.Client{Transport: Transport()})
}

// NewHTTPWithClient returns a prober using the given client, for tests and for
// callers that need a custom transport. Per-probe deadlines come from the
// check's timeout, so the client's own Timeout is left alone.
func NewHTTPWithClient(c *http.Client) *HTTP {
	return &HTTP{client: c}
}

// Transport is the probe transport. Keep-alives are disabled on purpose: a
// reused connection skips DNS resolution, the TCP handshake and the TLS
// handshake, so every probe after the first would silently stop covering the
// layers the check is supposed to cover — and the certificate expiry that a
// later release reads out of the handshake would never be observed again.
func Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableKeepAlives = true
	// Probes ignore HTTP_PROXY and friends. The set of hosts lookout talks to
	// is closed and explicit (SPEC §1.3); an inherited proxy variable would
	// silently reroute every probe and make a green check meaningless.
	t.Proxy = nil
	return t
}

// Probe runs one HTTP check. It never returns an error: a failed probe is a
// result, not an exception.
func (p *HTTP) Probe(ctx context.Context, c config.Check) check.Result {
	res := check.Result{Name: c.Name, At: time.Now()}

	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, c.Method, c.URL, nil)
	if err != nil {
		res.Outcome = check.OutcomeDown
		res.Err = "request could not be built: " + redact(err)
		res.Duration = time.Since(res.At)
		return res
	}
	for name, value := range c.Headers {
		req.Header.Set(name, value)
	}

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		res.Duration = time.Since(start)
		res.Outcome = check.OutcomeDown
		res.Err = transportError(err, c.Timeout)
		return res
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, maxBody+1)
	body, readErr := io.ReadAll(limited)
	res.Duration = time.Since(start)
	res.StatusCode = resp.StatusCode
	if readErr != nil {
		res.Outcome = check.OutcomeDown
		res.Err = "response body could not be read: " + transportError(readErr, c.Timeout)
		return res
	}

	truncated := false
	if len(body) > maxBody {
		body = body[:maxBody]
		truncated = true
	}
	res.BodySample = check.Sample(body, sampleBytes)

	outcome, failures := check.Evaluate(c.Expect, check.Response{
		StatusCode: resp.StatusCode,
		Body:       body,
		Duration:   res.Duration,
		Truncated:  truncated,
	})
	res.Outcome = outcome
	res.Failures = failures
	return res
}

// transportError turns a client error into a short, secret-free sentence.
func transportError(err error, timeout time.Duration) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out after " + timeout.String()
	case errors.Is(err, context.Canceled):
		return "probe cancelled"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "name " + dns.Name + " could not be resolved: " + dns.Err
	}
	return redact(err)
}

// redact strips credentials that Go embeds in *url.Error messages, so that a
// URL carrying a token never reaches a log line or an alert (SPEC §11).
func redact(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		if u, perr := url.Parse(uerr.URL); perr == nil && u.User != nil {
			safe := *u
			safe.User = url.User("redacted")
			return strings.ReplaceAll(uerr.Err.Error(), uerr.URL, safe.String())
		}
		return uerr.Op + " " + uerr.URL + ": " + uerr.Err.Error()
	}
	return err.Error()
}
