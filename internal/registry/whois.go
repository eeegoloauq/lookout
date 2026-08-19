package registry

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Built-in WHOIS servers for TLDs that have no RDAP. Verified 2026-08-19
// against whois.iana.org (whois: whois.tcinet.ru for ru and xn--p1ai)
// and against a live TCP/43 query to whois.tcinet.ru.
var defaultWHOIS = map[string]string{
	"ru":       "whois.tcinet.ru",
	"su":       "whois.tcinet.ru",
	"xn--p1ai": "whois.tcinet.ru", // .рф
}

// DefaultWHOIS returns the built-in server for tld, if any.
func DefaultWHOIS(tld string) (string, bool) {
	host, ok := defaultWHOIS[strings.ToLower(strings.TrimSpace(tld))]
	return host, ok
}

// expiryKeys are tried in order. paid-till is first because it is the
// field .ru actually uses; the rest cover the common gTLD formats.
var expiryKeys = []string{
	"paid-till",
	"registry expiry date",
	"registrar registration expiration date",
	"expiry date",
	"expiration date",
	"expiration time",
	"expire",
	"expires",
	"paid till",
}

var freeDateKeys = []string{"free-date", "free date"}
var stateKeys = []string{"state", "domain status"}

// ParseWHOIS extracts expiry from a raw TCP/43 reply. Different
// registries use different field names; if none of them are present
// this returns an error naming what was looked for, never a zero
// Record that the caller could mistake for "parsed, no date".
func ParseWHOIS(text string) (Record, error) {
	fields := whoisFields(text)
	if len(fields) == 0 {
		return Record{}, fmt.Errorf("whois response has no key: value lines")
	}

	expires, key, err := lookupTime(fields, expiryKeys)
	if err != nil {
		return Record{}, err
	}
	if key == "" {
		return Record{}, fmt.Errorf("whois response has no recognisable expiry field (looked for %s); saw %s",
			strings.Join(expiryKeys, ", "), joinKeys(fields))
	}

	rec := Record{Expires: expires, Source: SourceWHOIS}
	if t, k, err := lookupTime(fields, freeDateKeys); err != nil {
		return Record{}, fmt.Errorf("%s: %w", k, err)
	} else if k != "" {
		rec.FreeDate = t
	}
	if v, ok := firstValue(fields, stateKeys); ok {
		rec.State = v
	}
	return rec, nil
}

// ParseIANAReferral extracts the "whois:" server from an IANA TLD record.
func ParseIANAReferral(text string) (string, error) {
	fields := whoisFields(text)
	vals := fields["whois"]
	if len(vals) == 0 {
		return "", fmt.Errorf("IANA whois reply has no whois: referral; saw %s", joinKeys(fields))
	}
	host := strings.TrimSpace(vals[0])
	host = strings.TrimPrefix(host, "whois://")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		return "", fmt.Errorf("IANA whois: referral is empty")
	}
	return host, nil
}

func lookupTime(fields map[string][]string, keys []string) (time.Time, string, error) {
	for _, key := range keys {
		vals := fields[key]
		if len(vals) == 0 {
			continue
		}
		t, err := parseTime(vals[0])
		if err != nil {
			return time.Time{}, key, fmt.Errorf("whois field %s value %q is not a date: %w", key, vals[0], err)
		}
		return t, key, nil
	}
	return time.Time{}, "", nil
}

func firstValue(fields map[string][]string, keys []string) (string, bool) {
	for _, key := range keys {
		if vals := fields[key]; len(vals) > 0 {
			return vals[0], true
		}
	}
	return "", false
}

func whoisFields(text string) map[string][]string {
	out := map[string][]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "%") || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ">>>") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		if key == "" || val == "" {
			continue
		}
		out[key] = append(out[key], val)
	}
	return out
}

func joinKeys(fields map[string][]string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
