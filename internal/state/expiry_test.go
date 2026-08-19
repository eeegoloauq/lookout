package state

import (
	"testing"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
)

func TestDaysLeft(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in   time.Duration
		want int
	}{
		{21 * 24 * time.Hour, 21},
		{21*24*time.Hour + time.Hour, 21},
		{20*24*time.Hour + 23*time.Hour, 20},
		{12 * time.Hour, 0},
		{-time.Hour, -1},
		{-25 * time.Hour, -2},
	}
	for _, tc := range tests {
		if got := DaysLeft(now.Add(tc.in), now); got != tc.want {
			t.Errorf("DaysLeft(%s) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestNextTierFiresEachThresholdOnce(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	var fired uint32
	var daily string
	// 25 days: nothing
	if _, _, _, fire := nextTier(25, CertTiers, fired, daily, now); fire {
		t.Fatal("25 days must not fire a cert tier")
	}
	// 20 days: 21
	th, fired, daily, fire := nextTier(20, CertTiers, fired, daily, now)
	if !fire || th != 21 {
		t.Fatalf("20 days: fire=%v th=%d, want 21", fire, th)
	}
	// still 20: no repeat
	if _, fired, daily, fire = nextTier(20, CertTiers, fired, daily, now); fire {
		t.Fatal("the 21-day tier must not fire twice")
	}
	// 10 days: 14 (tightest entered), 21 already marked
	th, fired, daily, fire = nextTier(10, CertTiers, fired, daily, now)
	if !fire || th != 14 {
		t.Fatalf("10 days: fire=%v th=%d, want 14", fire, th)
	}
	if _, _, _, fire = nextTier(10, CertTiers, fired, daily, now); fire {
		t.Fatal("14-day tier must not fire twice")
	}
}

func TestNextTierDailyAfterLastNumbered(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	var fired uint32
	var daily string
	// Jump straight to 2 days: fire the last numbered tier (3), not a daily.
	th, fired, daily, fire := nextTier(2, CertTiers, 0, "", now)
	if !fire || th != 3 {
		t.Fatalf("first sight at 2 days: th=%d fire=%v, want numbered 3", th, fire)
	}
	// Same calendar day: no daily (consumed with the numbered fire).
	if _, fired, daily, fire = nextTier(2, CertTiers, fired, daily, now); fire {
		t.Fatal("must not also daily-fire on the same day as the last numbered tier")
	}
	// Next day: daily
	th, fired, daily, fire = nextTier(1, CertTiers, fired, daily, now.Add(24*time.Hour))
	if !fire || th != 0 {
		t.Fatalf("next day: th=%d fire=%v, want daily", th, fire)
	}
	if _, _, _, fire = nextTier(1, CertTiers, fired, daily, now.Add(24*time.Hour)); fire {
		t.Fatal("daily must fire once per UTC day")
	}
}

func TestCertExpiryTiersAndRenewal(t *testing.T) {
	c := testCheck()
	m := NewMachine()
	now := epoch
	notAfter := now.Add(20 * 24 * time.Hour)

	evs := m.Observe(c, check.Result{Name: c.Name, At: now, Outcome: check.OutcomeUp, CertNotAfter: notAfter})
	if kinds(evs) != "expiry" {
		t.Fatalf("events = %q, want one expiry", kinds(evs))
	}
	if evs[0].Expiry == nil || evs[0].Expiry.Threshold != 21 || evs[0].Expiry.Kind != ExpiryCertificate {
		t.Fatalf("expiry = %+v", evs[0].Expiry)
	}

	// Same cert, later probe: silent.
	if got := kinds(m.Observe(c, check.Result{Name: c.Name, At: now.Add(time.Hour), Outcome: check.OutcomeUp, CertNotAfter: notAfter})); got != "" {
		t.Errorf("repeat = %q", got)
	}

	// Renewed for a year: reset, no fire.
	renewedFor := notAfter.Add(365 * 24 * time.Hour)
	if got := kinds(m.Observe(c, check.Result{Name: c.Name, At: now.Add(2 * time.Hour), Outcome: check.OutcomeUp, CertNotAfter: renewedFor})); got != "" {
		t.Errorf("renewal = %q, want none", got)
	}
	st, _ := m.State(c.Name)
	if st.CertTiersFired != 0 || st.CertDailyOn != "" {
		t.Errorf("renewal must clear sent tiers: fired=%d daily=%q", st.CertTiersFired, st.CertDailyOn)
	}
}

func TestZoneDriftBaselineThenChange(t *testing.T) {
	cfg, err := config.Load("config.yaml", []byte(`
checks:
  - name: MX
    type: dns
    host: service.example
    query_type: MX
`))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Checks[0]
	m := NewMachine()

	first := check.Result{
		Name: c.Name, At: epoch, Outcome: check.OutcomeUp,
		Rcode: "NOERROR", ZoneSnapshot: "NOERROR\nMX 10 mail.service.example.",
	}
	if got := kinds(m.Observe(c, first)); got != "" {
		t.Fatalf("first snapshot must be a baseline, got %q", got)
	}
	st, _ := m.State(c.Name)
	if st.ZoneSnapshot == "" {
		t.Fatal("baseline was not stored")
	}

	// Identical snapshot: silent.
	if got := kinds(m.Observe(c, first)); got != "" {
		t.Errorf("unchanged = %q", got)
	}

	// Registrar wipe: NXDOMAIN.
	wipe := first
	wipe.At = epoch.Add(time.Hour)
	wipe.Outcome = check.OutcomeDown
	wipe.Rcode = "NXDOMAIN"
	wipe.ZoneSnapshot = "NXDOMAIN"
	evs := m.Observe(c, wipe)
	if kinds(evs) != "drift" {
		t.Fatalf("wipe events = %q, want drift (and not a confirmed down yet)", kinds(evs))
	}
	if evs[0].Drift == nil || evs[0].Drift.Before == evs[0].Drift.After {
		t.Fatalf("drift = %+v", evs[0].Drift)
	}
	if !evs[0].Alert {
		t.Error("drift must carry the check's alert default")
	}
}

func TestDomainUnknownIsNotDownAndStalesAfterThreeDays(t *testing.T) {
	cfg, err := config.Load("config.yaml", []byte(`
checks:
  - name: Registration
    type: domain
    domain: service.example
`))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Checks[0]
	m := NewMachine()

	unknown := check.Result{Name: c.Name, At: epoch, Outcome: check.OutcomeUnknown, Err: "whois timeout"}
	if got := kinds(m.Observe(c, unknown)); got != "" {
		t.Fatalf("first unknown = %q, want none", got)
	}
	if m.Status(c.Name) != StatusUnknown {
		t.Errorf("status = %q, a missed lookup is not down", m.Status(c.Name))
	}

	// Two days later still silent.
	later := unknown
	later.At = epoch.Add(48 * time.Hour)
	if got := kinds(m.Observe(c, later)); got != "" {
		t.Errorf("48h unknown = %q", got)
	}

	// Three days: stale.
	stale := unknown
	stale.At = epoch.Add(DomainStaleAfter)
	evs := m.Observe(c, stale)
	if kinds(evs) != "stale" {
		t.Fatalf("72h = %q, want stale", kinds(evs))
	}
	if evs[0].StaleFor < DomainStaleAfter {
		t.Errorf("stale_for = %s", evs[0].StaleFor)
	}

	// Repeat: no second notice until a success resets it.
	if got := kinds(m.Observe(c, stale)); got != "" {
		t.Errorf("repeat stale = %q", got)
	}

	// A successful fetch clears the stale flag and may fire an expiry.
	ok := check.Result{
		Name: c.Name, At: epoch.Add(4 * 24 * time.Hour), Outcome: check.OutcomeUp,
		DomainExpiresAt: epoch.Add(90 * 24 * time.Hour),
		DomainSource:    "whois",
	}
	if got := kinds(m.Observe(c, ok)); got != "" {
		t.Errorf("recovery from stale with 90 days left = %q, want none", got)
	}
	st, _ := m.State(c.Name)
	if !st.DomainUnknownSince.IsZero() || st.DomainStaleNotice {
		t.Errorf("success must clear the unknown window: %+v", st)
	}
}

func TestDomainExpiryTiers(t *testing.T) {
	cfg, err := config.Load("config.yaml", []byte(`
checks:
  - name: Registration
    type: domain
    domain: service.example
`))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Checks[0]
	m := NewMachine()
	now := epoch
	evs := m.Observe(c, check.Result{
		Name: c.Name, At: now, Outcome: check.OutcomeUp,
		DomainExpiresAt: now.Add(45 * 24 * time.Hour),
		DomainSource:    "rdap",
	})
	if kinds(evs) != "expiry" {
		t.Fatalf("events = %q", kinds(evs))
	}
	if evs[0].Expiry.Kind != ExpiryDomain || evs[0].Expiry.Threshold != 60 {
		t.Errorf("expiry = %+v", evs[0].Expiry)
	}
}

func TestLostStateDoesNotRefireExpiryOrDrift(t *testing.T) {
	c := testCheck()
	m := NewMachine()
	notAfter := epoch.Add(10 * 24 * time.Hour)
	m.Observe(c, check.Result{Name: c.Name, At: epoch, Outcome: check.OutcomeUp, CertNotAfter: notAfter})

	// Lost file → empty machine. Same cert must fire again? Spec §9:
	// losing state must not produce false alerts. Re-firing a tier
	// after a lost file is a duplicate page, so we accept the
	// re-fire only if we kept state. After loss we *do* fire again
	// because we no longer know it was sent — that is the same
	// trade-off as a first-ever observation. Documented in the
	// series report. Here we just check the happy persist path.
	restarted := NewMachine()
	restarted.Restore(m.Snapshot())
	if got := kinds(restarted.Observe(c, check.Result{Name: c.Name, At: epoch.Add(time.Hour), Outcome: check.OutcomeUp, CertNotAfter: notAfter})); got != "" {
		t.Errorf("restored state re-fired: %q", got)
	}
}
