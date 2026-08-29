// Package demo builds a monitor with synthetic data.
//
// It exists so the status page can be looked at, screenshotted and reviewed
// without pointing lookout at anything real. Nothing here probes: results are
// written straight into state and history, so the board renders the same way
// it does in production while the machine stays offline.
package demo

import (
	"io"
	"log/slog"
	"math/rand/v2"
	"path/filepath"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
	"github.com/eeegoloauq/lookout/internal/history"
	"github.com/eeegoloauq/lookout/internal/monitor"
)

// Config is the demo board. Hostnames are the reserved ones from RFC 2606, so
// a reader who copies a line out of a screenshot cannot end up probing someone
// else's machine. Two registrable names appear (example.com and example.org),
// which is what gives the board its derived domain rows.
const Config = `
timezone: Europe/London
registration_group: Domains

defaults:
  interval: 60s
  timeout: 5s

alerting:
  mode: none

checks:
  - name: Router
    group: Core
    type: http
    url: http://router.lan/
    expect: {status: 200, response_time: "<1s"}

  - name: Proxy
    group: Core
    type: http
    url: http://proxy.lan:81/
    expect: {status: 200, response_time: "<3s"}

  - name: Git
    group: Core
    type: http
    url: http://git.lan:3000/
    expect: {status: 200, response_time: "<3s"}

  - name: Photos
    group: Services
    type: http
    url: http://photos.lan:2283/api/server/ping
    expect:
      status: 200
      body: {".res": "pong"}

  - name: Database
    group: Services
    type: tcp
    address: db.lan:5432
    expect: {response_time: "<1s"}

  - name: Vault
    group: Services
    type: http
    url: https://vault.example.org/
    timeout: 15s
    expect: {status: 200, response_time: "<8s"}

  - name: Music
    group: Services
    type: http
    url: https://music.example.org/ping
    timeout: 15s
    expect: {status: "<500", response_time: "<8s"}

  - name: Website
    group: Public
    type: http
    url: https://example.com/
    timeout: 15s
    expect: {status: 200, response_time: "<8s"}

  - name: Website API
    group: Public
    type: http
    url: https://example.com/api/health
    expect:
      status: 200
      body: {".status": "ok"}

  - name: Website MX
    group: Public
    type: dns
    host: example.com
    query_type: MX
    interval: 300s
    expect: {rcode: NOERROR, answers_contain: mail.example.com}
`

// Monitor returns a monitor seeded with a day of history, ending at now.
// State files go under dir, which the caller owns and may throw away.
func Monitor(dir string, now time.Time) (*monitor.Monitor, error) {
	cfg, err := config.Load("demo.yaml", []byte(Config))
	if err != nil {
		return nil, err
	}
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.HistoryFile = filepath.Join(dir, "history.jsonl")
	cfg.SamplesFile = filepath.Join(dir, "samples.jsonl")

	m := monitor.New(cfg, nil, monitor.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	seed(m, now)
	return m, nil
}

// day is one probe per minute for 24 hours, the length every pattern below
// has to add up to.
const day = 24 * 60

// seed writes the history. Each check tells a different story on purpose: a
// board where everything is green demonstrates nothing.
func seed(m *monitor.Monitor, now time.Time) {
	start := now.Add(-24 * time.Hour)

	// The boring majority. A monitor is mostly this, and the page has to
	// stay readable when it is.
	feed(m, "Router", up(day), start, 12*time.Millisecond, 200)
	feed(m, "Proxy", up(day), start, 2*time.Millisecond, 200)
	feed(m, "Vault", up(day), start, 30*time.Millisecond, 200)
	feed(m, "Music", up(day), start, 44*time.Millisecond, 200)
	feed(m, "Database", up(day), start, 1*time.Millisecond, 0)
	feed(m, "Website MX", up(288), start, 7*time.Millisecond, 0)

	// A restart in the small hours: down long enough to alert, recovered
	// long ago. This is what most of the incident log looks like.
	feed(m, "Photos", up(430)+down(25)+up(day-455), start, 38*time.Millisecond, 200)

	// A check that alternates instead of failing outright. Consecutive
	// counters never see this; the N-of-M detector is why it is on the board.
	feed(m, "Git", up(day-140)+flap(70), start, 4*time.Millisecond, 200)

	// Slow, then broken. Latency climbs before the failure, which is the
	// case the "slowest 5%" column is there to make visible.
	feed(m, "Website", up(day-90)+slow(70)+down(20), start, 74*time.Millisecond, 200)

	// Still down right now, and for a reason worth reading: the endpoint
	// answers, the body no longer says what it promised.
	feed(m, "Website API", up(day-35), start, 60*time.Millisecond, 200)
	malformed(m, "Website API", 35, start.Add((day-35)*time.Minute), 58*time.Millisecond)

	// The month strip. Only the last day of it comes out of the probes
	// above, and a strip that is grey everywhere else would demonstrate the
	// opposite of what it is for: that a bad Tuesday is still visible three
	// weeks later. Each check gets the past it implies — the flapper has
	// had bad days before, the boring ones have not.
	seedMonth(m, now)

	certExpiry(m, "Vault", now, 12)
	certExpiry(m, "Website", now, 74)
	registration(m, "example.com", now, 11)
	registration(m, "example.org", now, 213)
}

// monthCheck is one check's normal day: how many probes it lands and how
// long they usually take. The month is drawn from this, so a day in the
// strip carries the same numbers a day of probes would have.
type monthCheck struct {
	name    string
	samples int
	typical int64
	slowest int64
}

var monthChecks = []monthCheck{
	{"Router", day, 12, 19},
	{"Proxy", day, 2, 4},
	{"Vault", day, 30, 41},
	{"Music", day, 44, 58},
	{"Database", day, 1, 2},
	{"Website MX", 288, 7, 12},
	{"Photos", day, 38, 52},
	{"Git", day, 4, 9},
	{"Website", day, 74, 96},
	{"Website API", day, 60, 88},
}

// badDay is a day in the strip that is not clean. Every one of them carries
// the reason it went wrong: a square that can only say "something happened
// here" raises the question and then refuses to answer it.
type badDay struct {
	check   string
	daysAgo int
	uptime  float64
	outages int
	reason  string
}

var monthStory = []badDay{
	// Today, which has to agree with the day bar under it: the same four
	// checks that are failing in the last 24 hours are the ones whose last
	// square is not clean.
	{"Photos", 0, 0.983, 1, "Get http://photos.lan/: dial tcp: connect: connection refused"},
	{"Git", 0, 0.951, 1, "status: want 200, got 502"},
	{"Website", 0, 0.986, 1, "status: want 200, got 502"},
	{"Website API", 0, 0.976, 1, `body: want "ok", got "maintenance"`},

	{"Photos", 3, 0.982, 1, "Get http://photos.lan/: dial tcp: connect: connection refused"},
	{"Git", 6, 0.947, 2, "status: want 200, got 502"},
	{"Git", 17, 0.996, 1, "status: want 200, got 502"},
	{"Website", 9, 0.891, 1, "Get https://www.example.com/: context deadline exceeded"},
	{"Website API", 9, 0.905, 1, `body: want "ok", got "maintenance"`},
	{"Vault", 24, 0.999, 1, "Get https://vault.lan/: EOF"},
}

// seedMonth writes the days behind the board, today included. In production
// today's square is drawn from the in-progress accumulator, which the fed
// probes here deliberately bypass; writing the day gives the demo the same
// strip a running lookout would have.
func seedMonth(m *monitor.Monitor, now time.Time) {
	bad := map[string]badDay{}
	for _, b := range monthStory {
		bad[b.check+utcDay(now, b.daysAgo)] = b
	}
	for _, c := range monthChecks {
		for daysAgo := 29; daysAgo >= 0; daysAgo-- {
			date := utcDay(now, daysAgo)
			rec := history.Daily{
				Date:    date,
				Check:   c.name,
				Uptime:  1,
				Samples: c.samples,
				P50MS:   c.typical,
				P95MS:   c.slowest,
			}
			if b, ok := bad[c.name+date]; ok {
				rec.Uptime = b.uptime
				rec.Incidents = b.outages
				rec.Reason = b.reason
			}
			// A demo that cannot seed its own history is a demo that shows
			// an empty strip, which is the one thing it must not do.
			if err := m.SeedDay(rec); err != nil {
				return
			}
		}
	}
}

func utcDay(now time.Time, daysAgo int) string {
	return now.UTC().AddDate(0, 0, -daysAgo).Format("2006-01-02")
}

func up(n int) string   { return repeat("U", n) }
func down(n int) string { return repeat("D", n) }
func flap(n int) string { return repeat("UD", n/2) }

// slow marks probes that pass but take long enough to be worth a look. The
// pattern alphabet is deliberately tiny; "S" is the only special case.
func slow(n int) string { return repeat("S", n) }

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}

// feed replays a pattern, one probe per check interval, into state and history.
func feed(m *monitor.Monitor, name, pattern string, start time.Time, dur time.Duration, code int) {
	c, ok := lookup(m, name)
	if !ok {
		return
	}
	// Seeded from nothing but the check name: the same board every run, so a
	// screenshot can be reproduced and a layout diff means a layout change.
	jitter := rand.New(rand.NewPCG(nameSeed(name), 0x10f0))
	for i, r := range pattern {
		res := check.Result{
			Name: name, At: start.Add(time.Duration(i) * c.Interval),
			Outcome: check.OutcomeUp, Duration: vary(jitter, dur), StatusCode: code,
		}
		switch r {
		case 'D':
			res.Outcome = check.OutcomeDown
			res.StatusCode = 502
			res.Failures = []check.Failure{{Condition: "status", Want: "200", Got: "502"}}
		case 'S':
			// Four times the usual, still under the threshold.
			res.Duration = vary(jitter, dur*4)
		}
		m.Machine().Observe(c, res)
		m.History().Record(res)
	}
}

// malformed is the failure that a status code cannot express: the service is
// up, the contract is not.
func malformed(m *monitor.Monitor, name string, n int, start time.Time, dur time.Duration) {
	c, ok := lookup(m, name)
	if !ok {
		return
	}
	for i := range n {
		res := check.Result{
			Name: name, At: start.Add(time.Duration(i) * c.Interval),
			Outcome: check.OutcomeMalformed, Duration: dur, StatusCode: 200,
			Failures: []check.Failure{{
				Condition: "body .status", Want: `"ok"`, Got: "missing", Malformed: true,
			}},
			BodySample: `{"error":"upstream index unavailable"}`,
		}
		m.Machine().Observe(c, res)
		m.History().Record(res)
	}
}

func certExpiry(m *monitor.Monitor, name string, now time.Time, days int) {
	c, ok := lookup(m, name)
	if !ok {
		return
	}
	res := check.Result{
		Name: name, At: now, Outcome: check.OutcomeUp, Duration: 40 * time.Millisecond,
		StatusCode: 200, CertNotAfter: now.AddDate(0, 0, days),
	}
	m.Machine().Observe(c, res)
}

func registration(m *monitor.Monitor, host string, now time.Time, days int) {
	c, ok := lookup(m, host)
	if !ok {
		return
	}
	res := check.Result{
		Name: c.Name, At: now.Add(-8 * time.Hour), Outcome: check.OutcomeUp,
		Duration:        190 * time.Millisecond,
		DomainExpiresAt: now.AddDate(0, 0, days).Add(time.Hour),
		DomainState:     "clientTransferProhibited",
		DomainSource:    "rdap",
	}
	m.Machine().Observe(c, res)
	m.History().Record(res)
}

// vary spreads a latency the way a real one is spread: mostly close to the
// median, with a long thin tail. A board where every probe took exactly the
// same milliseconds is the one thing that gives synthetic data away, and it
// would leave the "slowest 5%" column with nothing to say.
func vary(r *rand.Rand, base time.Duration) time.Duration {
	d := time.Duration(float64(base) * (0.85 + 0.3*r.Float64()))
	if r.IntN(40) == 0 {
		d += time.Duration(float64(base) * (1 + 3*r.Float64()))
	}
	return d
}

// nameSeed hashes a name into a PRNG seed (FNV-1a, inlined to keep the package
// free of imports it needs for nothing else).
func nameSeed(name string) uint64 {
	h := uint64(14695981039346656037)
	for i := range len(name) {
		h ^= uint64(name[i])
		h *= 1099511628211
	}
	return h
}

func lookup(m *monitor.Monitor, name string) (config.Check, bool) {
	for _, c := range m.Config().Checks {
		if c.Name == name {
			return c, true
		}
	}
	return config.Check{}, false
}
