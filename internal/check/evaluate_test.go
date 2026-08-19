package check

import (
	"strings"
	"testing"
	"time"

	"github.com/eeegoloauq/lookout/internal/config"
)

// expectFrom compiles an expect block through the real config loader, so the
// tests exercise the same compiled matchers the daemon uses.
func expectFrom(t *testing.T, yaml string) config.Expect {
	t.Helper()
	src := "checks:\n  - name: T\n    type: http\n    url: http://t.invalid\n" + yaml
	cfg, err := config.Load("config.yaml", []byte(src))
	if err != nil {
		t.Fatalf("loading test config: %v", err)
	}
	return cfg.Checks[0].Expect
}

func TestEvaluate(t *testing.T) {
	body := `{"result":{"source":{"online":true}},"status":"ok","count":2}`
	tests := []struct {
		name        string
		expect      string
		resp        Response
		want        Outcome
		wantFailure string // substring of the rendered failure list
	}{
		{
			name:   "status in range",
			expect: "    expect: {status: \"200-299\"}\n",
			resp:   Response{StatusCode: 204},
			want:   OutcomeUp,
		},
		{
			name:        "status outside range",
			expect:      "    expect: {status: \"200-299\"}\n",
			resp:        Response{StatusCode: 503},
			want:        OutcomeDown,
			wantFailure: "status: want 200-299, got 503",
		},
		{
			name:        "response too slow",
			expect:      "    expect: {response_time: \"<1s\"}\n",
			resp:        Response{StatusCode: 200, Duration: 1500 * time.Millisecond},
			want:        OutcomeDown,
			wantFailure: "response_time: want <1s, got 1.5s",
		},
		{
			name:   "body contains",
			expect: "    expect: {body_contains: \"ok\"}\n",
			resp:   Response{StatusCode: 200, Body: []byte(body)},
			want:   OutcomeUp,
		},
		{
			name:        "body does not contain",
			expect:      "    expect: {body_contains: \"pong\"}\n",
			resp:        Response{StatusCode: 200, Body: []byte(body)},
			want:        OutcomeDown,
			wantFailure: `body_contains: want "pong"`,
		},
		{
			name:   "body path matches",
			expect: "    expect:\n      body:\n        \".result.source.online\": true\n        \".count\": 2\n",
			resp:   Response{StatusCode: 200, Body: []byte(body)},
			want:   OutcomeUp,
		},
		{
			name:        "body path value differs",
			expect:      "    expect:\n      body:\n        \".result.source.online\": false\n",
			resp:        Response{StatusCode: 200, Body: []byte(body)},
			want:        OutcomeDown,
			wantFailure: "body .result.source.online: want false, got true",
		},
		{
			// A missing field is a changed API, not an outage: it must read
			// differently from "the service is down" (SPEC §4).
			name:        "body path missing",
			expect:      "    expect:\n      body:\n        \".result.source.reachable\": true\n",
			resp:        Response{StatusCode: 200, Body: []byte(body)},
			want:        OutcomeMalformed,
			wantFailure: "field .result.source.reachable is missing from the response (.result.source exists",
		},
		{
			name:        "body is not json",
			expect:      "    expect:\n      body:\n        \".online\": true\n",
			resp:        Response{StatusCode: 200, Body: []byte("<html>nope</html>")},
			want:        OutcomeMalformed,
			wantFailure: "not valid JSON",
		},
		{
			name:        "body truncated",
			expect:      "    expect:\n      body:\n        \".online\": true\n",
			resp:        Response{StatusCode: 200, Body: []byte(`{"online":`), Truncated: true},
			want:        OutcomeMalformed,
			wantFailure: "truncated",
		},
		{
			name:        "type mismatch is a value mismatch, not malformed",
			expect:      "    expect:\n      body:\n        \".count\": \"2\"\n",
			resp:        Response{StatusCode: 200, Body: []byte(body)},
			want:        OutcomeDown,
			wantFailure: `body .count: want "2", got 2`,
		},
		{
			name:        "status failure and body failure are both reported",
			expect:      "    expect:\n      status: 200\n      body:\n        \".count\": 9\n",
			resp:        Response{StatusCode: 500, Body: []byte(body)},
			want:        OutcomeDown,
			wantFailure: "status: want 200, got 500; body .count: want 9, got 2",
		},
		{
			// A malformed body outweighs a plain mismatch: we cannot claim to
			// know what the service is doing.
			name:        "malformed wins over mismatch",
			expect:      "    expect:\n      status: 200\n      body:\n        \".missing\": 1\n",
			resp:        Response{StatusCode: 500, Body: []byte(body)},
			want:        OutcomeMalformed,
			wantFailure: "status: want 200, got 500",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exp := expectFrom(t, tc.expect)
			outcome, failures := Evaluate(exp, tc.resp)
			if outcome != tc.want {
				t.Errorf("outcome = %q, want %q (failures: %v)", outcome, tc.want, failures)
			}
			got := Result{Failures: failures}.Reason()
			if tc.wantFailure == "" {
				if len(failures) != 0 {
					t.Errorf("unexpected failures: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantFailure) {
				t.Errorf("failures = %q, want it to contain %q", got, tc.wantFailure)
			}
		})
	}
}

func TestSampleCutsAtRuneBoundary(t *testing.T) {
	body := []byte(strings.Repeat("я", 10)) // two bytes per rune
	got := Sample(body, 5)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("Sample did not mark truncation: %q", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Fatalf("Sample cut a rune in half: %q", got)
	}
}

func TestOutcomeFailed(t *testing.T) {
	if OutcomeUp.Failed() {
		t.Error("up must not count as a failure")
	}
	// Malformed counts against the check: an assertion we can no longer
	// evaluate is not a passing check.
	if !OutcomeDown.Failed() || !OutcomeMalformed.Failed() {
		t.Error("down and malformed must both count as failures")
	}
}
