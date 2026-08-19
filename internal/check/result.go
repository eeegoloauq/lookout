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
)

// Failed reports whether the outcome counts against the check. Malformed does:
// an assertion we can no longer evaluate is not a passing check.
func (o Outcome) Failed() bool { return o != OutcomeUp }

// Result is one probe outcome. It is small and copied by value.
type Result struct {
	Name     string
	At       time.Time
	Outcome  Outcome
	Duration time.Duration

	// StatusCode is 0 when no response arrived.
	StatusCode int
	// Err describes a transport-level failure (dial, TLS, timeout, read).
	Err string
	// Failures lists the conditions that did not hold, in evaluation order,
	// each with the value actually observed. This is what an alert needs and
	// what Gatus never provided.
	Failures []Failure
	// BodySample is the first bytes of the response body, for alert context.
	BodySample string
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
	Condition string
	Want      string
	Got       string
	// Malformed marks a response whose shape made the condition impossible to
	// evaluate, as opposed to a value that simply did not match.
	Malformed bool
}

func (f Failure) String() string {
	if f.Malformed {
		return f.Condition + ": " + f.Got
	}
	return f.Condition + ": want " + f.Want + ", got " + f.Got
}
