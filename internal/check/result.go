// Package check holds the outcome of one probe and the evaluation of the
// conditions a response has to satisfy.
package check

import (
	"strings"
	"time"
)

// Outcome is the verdict of a single probe.
//
// Malformed is deliberately separate from Down: a response that no longer has
// the field we assert on means the other side changed its API, which is not the
// same event as the service being unavailable and must not read the same in an
// alert (SPEC §4).
type Outcome string

const (
	OutcomeUp        Outcome = "up"
	OutcomeDown      Outcome = "down"
	OutcomeMalformed Outcome = "malformed"
	// OutcomeUnknown means the probe could not tell: a registry that did
	// not answer, not a domain that is down. It must not count as a
	// failure or a success (SPEC §5.4).
	OutcomeUnknown Outcome = "unknown"
)

// Failed reports whether the outcome counts against the check. Malformed does:
// an assertion we can no longer evaluate is not a passing check. Unknown
// does not: "I could not look it up" is not "it is down".
func (o Outcome) Failed() bool {
	return o == OutcomeDown || o == OutcomeMalformed
}

// Succeeded reports whether the probe got a definite pass.
func (o Outcome) Succeeded() bool { return o == OutcomeUp }

// Result is one probe outcome. It is small and copied by value. JSON tags
// exist so an Event carrying a Result can live in the durable outbox.
type Result struct {
	Name     string        `json:"name,omitempty"`
	At       time.Time     `json:"at,omitzero"`
	Outcome  Outcome       `json:"outcome,omitempty"`
	Duration time.Duration `json:"duration,omitempty"`

	// StatusCode is 0 when no response arrived.
	StatusCode int `json:"status_code,omitempty"`
	// Err describes a transport-level failure (dial, TLS, timeout, read).
	Err string `json:"err,omitempty"`
	// Failures lists the conditions that did not hold, in evaluation order,
	// each with the value actually observed. This is what an alert needs and
	// what Gatus never provided.
	Failures []Failure `json:"failures,omitempty"`
	// BodySample is the first bytes of the response body, for alert context.
	BodySample string `json:"body_sample,omitempty"`
	// RemoteAddr is the address the probe actually connected to. It is free
	// (the connection knows it) and it answers the question every migration
	// raises: am I still talking to the old box?
	RemoteAddr string `json:"remote_addr,omitempty"`

	// Rcode is the DNS response code, empty when the probe was not DNS
	// or no message arrived.
	Rcode string `json:"rcode,omitempty"`
	// Answers is the rendered DNS answer section, one record per line.
	Answers []string `json:"answers,omitempty"`
	// ZoneSnapshot is the canonical, sorted form used for drift
	// comparison. Empty when no DNS message was decoded.
	ZoneSnapshot string `json:"zone_snapshot,omitempty"`

	// CertNotAfter is the leaf certificate's expiry, captured from the
	// TLS handshake of an https check (SPEC §5.3). Zero when there was
	// no certificate — including a failed handshake that never sent one.
	CertNotAfter time.Time `json:"cert_not_after,omitzero"`

	// DomainExpiresAt is the registry expiry (RDAP expiration or WHOIS
	// paid-till). Zero when the lookup failed or could not be parsed.
	DomainExpiresAt time.Time `json:"domain_expires_at,omitzero"`
	DomainFreeDate  time.Time `json:"domain_free_date,omitzero"`
	DomainState     string    `json:"domain_state,omitempty"`
	DomainSource    string    `json:"domain_source,omitempty"` // "rdap" or "whois"
}

// Reason renders a one-line explanation suitable for a log or an alert.
func (r Result) Reason() string {
	switch {
	case r.Err != "":
		return r.Err
	case len(r.Failures) == 0:
		return ""
	}
	parts := make([]string, 0, len(r.Failures))
	for _, f := range r.Failures {
		parts = append(parts, f.String())
	}
	return strings.Join(parts, "; ")
}

// Failure is one condition that did not hold.
type Failure struct {
	// Condition names the assertion, e.g. "status" or "body .result.online".
	Condition string `json:"condition"`
	Want      string `json:"want,omitempty"`
	Got       string `json:"got,omitempty"`
	// Malformed marks a response whose shape made the condition impossible to
	// evaluate, as opposed to a value that simply did not match.
	Malformed bool `json:"malformed,omitempty"`
}

func (f Failure) String() string {
	if f.Malformed {
		return f.Condition + ": " + f.Got
	}
	return f.Condition + ": want " + f.Want + ", got " + f.Got
}
