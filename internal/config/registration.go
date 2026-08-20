package config

import (
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Adding a site should start watching the name it lives on. Otherwise the
// registration is a thing you have to remember to add separately — and the
// failure of remembering is silent for a year, then total.
//
// So every http and dns check contributes the registrable name of its host,
// and one domain check per name is synthesised. Names are deduped (a dozen
// checks across four names cost four registry queries a day and produce four
// renewal notices, never one per check), and an explicitly written
// `type: domain` check always wins over the derived one.

// specialUseTLD lists names that resolve but are nobody's to register.
// The public suffix list keeps most of them out of its ICANN section; .arpa
// is in it, because it is infrastructure rather than a place to buy a name.
var specialUseTLD = map[string]bool{
	"arpa": true, "internal": true, "intranet": true, "lan": true,
	"local": true, "localhost": true, "home": true, "corp": true, "private": true,
}

// RegistrationOff is the per-check opt-out: a target whose name is not yours
// to renew — someone else's API, a CDN — has no business producing renewal
// notices you cannot act on.
const RegistrationOff = "off"

// derivedRegistrations returns the domain checks implied by the http and dns
// checks in the configuration, in the order the names first appear.
func derivedRegistrations(checks []Check, def Check, group string) []Check {
	declared := map[string]bool{}
	taken := map[string]bool{}
	for _, c := range checks {
		taken[c.Name] = true
		if c.Type == TypeDomain && c.Host != "" {
			declared[strings.ToLower(c.Host)] = true
		}
	}

	var out []Check
	seen := map[string]bool{}
	for _, c := range checks {
		name := registrableName(c)
		if name == "" || declared[name] || seen[name] {
			continue
		}
		seen[name] = true
		derived := def
		derived.Name = name
		if taken[name] {
			// A check already carries this exact name — usually the site
			// itself. The registration still needs a row of its own in the
			// internal model, so it gets a name nobody would type.
			derived.Name = name + " (registration)"
		}
		// With a group the derived checks become rows of their own — a list
		// of names and dates, which is worth having when there are many.
		// Without one they stay behind the sites that implied them.
		derived.Group = group
		derived.Type = TypeDomain
		derived.Host = name
		derived.URL = ""
		derived.Method = ""
		derived.Headers = nil
		derived.QueryType = ""
		derived.Resolver = ""
		derived.Expect = Expect{}
		derived.Interval = DefaultDomainInterval
		derived.Timeout = DefaultDomainTimeout
		// The registry answers authoritatively the first time, and the probe
		// runs once a day: a threshold of two would leave a new check unknown
		// for a day and a real change unreported for another.
		derived.SuccessThreshold = 1
		derived.Alert = true
		derived.Implicit = true
		out = append(out, derived)
	}
	return out
}

// registrableName is the name a check's target is registered under, or "" if
// the check should not imply a registration at all.
func registrableName(c Check) string {
	if c.Type == TypeDomain || c.Registration == RegistrationOff {
		return ""
	}
	if c.Registration != "" {
		return strings.ToLower(c.Registration)
	}
	host := c.Host
	if c.Address != "" {
		if h, _, err := net.SplitHostPort(c.Address); err == nil {
			host = h
		}
	}
	if c.URL != "" {
		u, err := url.Parse(c.URL)
		if err != nil {
			return ""
		}
		host = u.Hostname()
	}
	return registrable(host)
}

// registrable maps a hostname to the name that is actually registered with a
// registry, or "" when there is nobody to register it with. An address, a
// bare label and every homelab-only suffix (.lan, .local, .internal, and any
// other name outside the ICANN part of the public suffix list) answer "".
func registrable(host string) string {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return ""
	}
	if tld := host[strings.LastIndexByte(host, '.')+1:]; specialUseTLD[tld] {
		return ""
	}
	suffix, icann := publicsuffix.PublicSuffix(host)
	if !icann {
		// Either a suffix nobody sells (.lan, .home.arpa, an unknown TLD) or
		// a private one (github.io): in both cases there is no registration
		// of ours to expire.
		return ""
	}
	if host == suffix {
		// The host *is* a public suffix ("com"): not a registrable name.
		return ""
	}
	name, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	return name
}
