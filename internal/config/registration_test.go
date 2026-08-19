package config

import (
	"strings"
	"testing"
	"time"
)

// Adding a site is adding its name: forgetting a renewal is silent for a
// year and then total, so it must not depend on remembering a second block.
func TestRegistrationsAreDerivedFromTheChecks(t *testing.T) {
	cfg := mustLoad(t, `
checks:
  - name: Site
    type: http
    url: https://example.co.uk/
  - name: Shop
    type: http
    url: https://shop.example.co.uk/health
  - name: Mail
    type: dns
    host: example.co.uk
    query_type: MX
  - name: Router
    type: http
    url: http://198.51.100.1/
  - name: Immich
    type: http
    url: http://immich.lan:2283/
  - name: Third party
    type: http
    url: https://api.telegram.org/
    registration: off
`)
	var derived []Check
	for _, c := range cfg.Checks {
		if c.Implicit {
			derived = append(derived, c)
		}
	}
	if len(derived) != 1 {
		names := make([]string, 0, len(derived))
		for _, c := range derived {
			names = append(names, c.Host)
		}
		t.Fatalf("derived %v, want exactly example.co.uk", names)
	}
	d := derived[0]
	// The public suffix list is what makes co.uk one name rather than two,
	// and three checks under it one query rather than three.
	if d.Host != "example.co.uk" || d.Type != TypeDomain {
		t.Errorf("derived = %+v", d)
	}
	if d.Interval != DefaultDomainInterval || d.SuccessThreshold != 1 || !d.Alert {
		t.Errorf("derived check is not a daily authoritative one: %+v", d)
	}
}

// An address, a bare hostname and every homelab-only suffix have no registry
// behind them; publicsuffix's ICANN flag is what tells them apart.
func TestRegistrableName(t *testing.T) {
	for host, want := range map[string]string{
		"example.ru":             "example.ru",
		"music.cdn.example.dev": "example.dev",
		"example.co.uk":              "example.co.uk",
		"a.b.example.co.uk":          "example.co.uk",
		"198.51.100.1":                "",
		"::1":                        "",
		"localhost":                  "",
		"immich.lan":                 "",
		"box.home.arpa":              "",
		"site.invalid":               "",
		"com":                        "",
		"":                           "",
	} {
		if got := registrable(host); got != want {
			t.Errorf("registrable(%q) = %q, want %q", host, got, want)
		}
	}
}

// An explicitly written domain check is the operator's decision about
// interval, group and alerting; deriving a second one for the same name
// would double every renewal notice.
func TestExplicitDomainCheckWins(t *testing.T) {
	cfg := mustLoad(t, `
checks:
  - name: Site
    type: http
    url: https://example.com/
  - name: example.com
    group: Domains
    type: domain
    domain: example.com
    interval: 12h
`)
	n := 0
	for _, c := range cfg.Checks {
		if c.Type == TypeDomain {
			n++
			if c.Implicit {
				t.Error("a written domain check was duplicated by a derived one")
			}
			if c.Interval != 12*time.Hour {
				t.Errorf("interval = %s, the operator asked for 12h", c.Interval)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d domain checks, want the one that was written", n)
	}
}

// A name you cannot renew must not produce renewal notices; the opt-out is
// per check, and a misspelt one is a config error rather than silence.
func TestRegistrationOptOutAndOverride(t *testing.T) {
	cfg := mustLoad(t, `
checks:
  - name: Vendor
    type: http
    url: https://status.vendor.example/
    registration: off
  - name: Behind a CDN
    type: http
    url: https://cdn-edge-7.example.net/
    registration: mysite.org
`)
	var derived []string
	for _, c := range cfg.Checks {
		if c.Implicit {
			derived = append(derived, c.Host)
		}
	}
	if len(derived) != 1 || derived[0] != "mysite.org" {
		t.Fatalf("derived %v, want just the overridden mysite.org", derived)
	}
	_, err := Load("config.yaml", []byte(`
checks:
  - name: Bad
    type: http
    url: https://example.com/
    registration: not a domain
`))
	if err == nil || !strings.Contains(err.Error(), "registration") {
		t.Fatalf("err = %v, want a complaint about the registration name", err)
	}
}

// The derived registrations are worth a board of their own when there are
// several names — one line each, with the date. That is a preference, so it
// is one line of configuration rather than a rule.
func TestRegistrationGroupGivesDerivedChecksARow(t *testing.T) {
	const checks = `
checks:
  - name: Site
    type: http
    url: https://example.com/
`
	if got := mustLoad(t, checks); derivedGroup(got.Checks) != "" {
		t.Errorf("derived group = %q, want none by default", derivedGroup(got.Checks))
	}
	cfg := mustLoad(t, "registration_group: Domains\n"+checks)
	if got := derivedGroup(cfg.Checks); got != "Domains" {
		t.Errorf("derived group = %q, want Domains", got)
	}
}

func derivedGroup(checks []Check) string {
	for _, c := range checks {
		if c.Implicit {
			return c.Group
		}
	}
	return "(none derived)"
}
