package check

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eeegoloauq/lookout/internal/config"
)

// Response is what a probe observed. It is the only input to Evaluate, which
// keeps condition evaluation a pure function and therefore exhaustively
// table-testable.
type Response struct {
	StatusCode int
	Body       []byte
	Duration   time.Duration
	// Truncated reports that Body was cut at the read limit, so a body
	// condition that does not match cannot be trusted to have failed.
	Truncated bool

	// DNS fields. Rcode is the canonical name (NOERROR, NXDOMAIN, …);
	// Answers are the rendered records the probe collected.
	Rcode   string
	Answers []string
}

// Evaluate checks a response against the expectations of a check. It returns
// the failures in a stable order and the resulting outcome.
func Evaluate(exp config.Expect, resp Response) (Outcome, []Failure) {
	var failures []Failure
	malformed := false

	if !exp.Status.IsZero() && !exp.Status.Match(resp.StatusCode) {
		failures = append(failures, Failure{
			Condition: "status",
			Want:      exp.Status.String(),
			Got:       strconv.Itoa(resp.StatusCode),
		})
	}

	if !exp.ResponseTime.IsZero() && !exp.ResponseTime.Match(resp.Duration) {
		failures = append(failures, Failure{
			Condition: "response_time",
			Want:      exp.ResponseTime.String(),
			Got:       resp.Duration.Round(time.Millisecond).String(),
		})
	}

	if exp.BodyContains != "" && !strings.Contains(string(resp.Body), exp.BodyContains) {
		f := Failure{
			Condition: "body_contains",
			Want:      strconv.Quote(exp.BodyContains),
			Got:       "not present in the response body",
		}
		if resp.Truncated {
			f.Malformed = true
			f.Got = "response body was truncated at the read limit before the string could be found"
			malformed = true
		}
		failures = append(failures, f)
	}

	if len(exp.Body) > 0 {
		bodyFailures, bodyMalformed := evaluateBody(exp.Body, resp)
		failures = append(failures, bodyFailures...)
		malformed = malformed || bodyMalformed
	}

	if exp.Rcode != "" {
		got := resp.Rcode
		if got == "" {
			got = "(no DNS response)"
		}
		if !strings.EqualFold(got, exp.Rcode) {
			failures = append(failures, Failure{
				Condition: "rcode",
				Want:      exp.Rcode,
				Got:       got,
			})
		}
	}

	if exp.AnswersContain != "" {
		if !answersContain(resp.Answers, exp.AnswersContain) {
			got := "not present in the DNS answers"
			if len(resp.Answers) == 0 {
				got = "answer section is empty"
			}
			failures = append(failures, Failure{
				Condition: "answers_contain",
				Want:      strconv.Quote(exp.AnswersContain),
				Got:       got,
			})
		}
	}

	switch {
	case len(failures) == 0:
		return OutcomeUp, nil
	case malformed:
		return OutcomeMalformed, failures
	default:
		return OutcomeDown, failures
	}
}

func evaluateBody(want []config.BodyExpect, resp Response) ([]Failure, bool) {
	var doc any
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		got := "response body is not valid JSON: " + err.Error()
		if resp.Truncated {
			got = "response body was truncated at the read limit, so it cannot be parsed as JSON"
		}
		return []Failure{{Condition: "body", Got: got, Malformed: true}}, true
	}

	var failures []Failure
	malformed := false
	for _, be := range want {
		got, found := be.Path.Lookup(doc)
		if !found {
			// A path that does not resolve is a changed response format, not a
			// failing service. It reads differently in an alert.
			failures = append(failures, Failure{
				Condition: "body " + be.Path.String(),
				Want:      render(be.Want),
				Got:       missingDetail(be, doc),
				Malformed: true,
			})
			malformed = true
			continue
		}
		if !equalJSON(be.Want, got) {
			failures = append(failures, Failure{
				Condition: "body " + be.Path.String(),
				Want:      render(be.Want),
				Got:       render(got),
			})
		}
	}
	return failures, malformed
}

// missingDetail names the deepest prefix of the path that does resolve, which
// turns "field is missing" into something actionable.
func missingDetail(be config.BodyExpect, doc any) string {
	deepest := ""
	for _, prefix := range be.Path.Prefixes() {
		if _, ok := prefix.Lookup(doc); !ok {
			break
		}
		deepest = prefix.String()
	}
	if deepest == "" {
		return fmt.Sprintf("field %s is missing from the response", be.Path)
	}
	return fmt.Sprintf("field %s is missing from the response (%s exists, the rest does not)", be.Path, deepest)
}

// equalJSON compares a configured expectation with a decoded JSON value.
// Types are compared strictly; YAML numbers were normalised to float64 when the
// configuration was loaded, so an integer in either source compares equal.
func equalJSON(want, got any) bool {
	switch w := want.(type) {
	case nil:
		return got == nil
	case bool:
		g, ok := got.(bool)
		return ok && g == w
	case string:
		g, ok := got.(string)
		return ok && g == w
	case float64:
		g, ok := got.(float64)
		return ok && g == w
	default:
		return false
	}
}

// render prints a value the way it appeared on the wire, with its kind when
// that is what differs.
func render(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(t)
	case string:
		return strconv.Quote(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return truncate(string(b), 120)
	}
}

// Sample returns the leading bytes of a body for alert context, cut at a rune
// boundary so the text stays printable. Secrets are stripped before the cut
// so a token that starts in the first 200 bytes cannot leak as a prefix.
func Sample(body []byte, limit int) string {
	return truncate(RedactSecrets(string(body)), limit)
}

func answersContain(answers []string, want string) bool {
	if want == "" {
		return true
	}
	needle := strings.ToLower(want)
	for _, a := range answers {
		if strings.Contains(strings.ToLower(a), needle) {
			return true
		}
	}
	return false
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
