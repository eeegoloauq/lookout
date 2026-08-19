// Package registry talks to domain registries: IANA's RDAP bootstrap,
// registry RDAP, and WHOIS on TCP/43. It is a parser-and-client, not a
// check — the probe package decides what a miss means.
package registry

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Source names the protocol that produced a Record.
const (
	SourceRDAP  = "rdap"
	SourceWHOIS = "whois"
)

// Record is the expiry information lookout actually uses. Extra fields
// from a WHOIS reply (state, free-date) ride along because they change
// the meaning of the date (research O15).
type Record struct {
	Expires  time.Time
	FreeDate time.Time
	State    string
	Source   string
}

// Bootstrap is IANA's TLD → RDAP-base map.
type Bootstrap struct {
	Publication time.Time
	// Services maps a lower-case TLD (com, xn--p1ai) to a registry
	// RDAP base URL, trailing slash included when the file had one.
	Services map[string]string
}

// ParseBootstrap decodes the IANA dns.json document. An empty or
// unreadable file is an error — we must not cache "nothing" as if it
// were a real bootstrap.
func ParseBootstrap(body []byte) (Bootstrap, error) {
	var raw struct {
		Publication string            `json:"publication"`
		Services    [][]json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Bootstrap{}, fmt.Errorf("RDAP bootstrap is not valid JSON: %w", err)
	}
	if len(raw.Services) == 0 {
		return Bootstrap{}, fmt.Errorf("RDAP bootstrap has no services")
	}
	out := Bootstrap{Services: make(map[string]string, len(raw.Services))}
	if raw.Publication != "" {
		if t, err := parseTime(raw.Publication); err == nil {
			out.Publication = t
		}
	}
	for i, svc := range raw.Services {
		if len(svc) < 2 {
			return Bootstrap{}, fmt.Errorf("RDAP bootstrap service %d is malformed", i)
		}
		var tlds []string
		var urls []string
		if err := json.Unmarshal(svc[0], &tlds); err != nil {
			return Bootstrap{}, fmt.Errorf("RDAP bootstrap service %d tlds: %w", i, err)
		}
		if err := json.Unmarshal(svc[1], &urls); err != nil {
			return Bootstrap{}, fmt.Errorf("RDAP bootstrap service %d urls: %w", i, err)
		}
		if len(urls) == 0 {
			continue
		}
		base := strings.TrimSpace(urls[0])
		if base == "" {
			continue
		}
		for _, tld := range tlds {
			tld = strings.ToLower(strings.TrimSpace(tld))
			if tld == "" {
				continue
			}
			out.Services[tld] = base
		}
	}
	if len(out.Services) == 0 {
		return Bootstrap{}, fmt.Errorf("RDAP bootstrap listed no usable TLD")
	}
	return out, nil
}

// ParseRDAP extracts the expiration event from a registry RDAP domain
// object. A 200 body with no expiration is a parse error, never a
// silent zero time: "could not read" must not look like "no expiry".
func ParseRDAP(body []byte) (Record, error) {
	var raw struct {
		ErrorCode int `json:"errorCode"`
		Title     string `json:"title"`
		Events    []struct {
			Action string `json:"eventAction"`
			Date   string `json:"eventDate"`
		} `json:"events"`
		Status []string `json:"status"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Record{}, fmt.Errorf("RDAP response is not valid JSON: %w", err)
	}
	if raw.ErrorCode != 0 {
		return Record{}, fmt.Errorf("RDAP error %d %s", raw.ErrorCode, raw.Title)
	}
	var expires time.Time
	var found bool
	for _, ev := range raw.Events {
		if !strings.EqualFold(ev.Action, "expiration") {
			continue
		}
		t, err := parseTime(ev.Date)
		if err != nil {
			return Record{}, fmt.Errorf("RDAP expiration %q is not a timestamp: %w", ev.Date, err)
		}
		expires = t
		found = true
		break
	}
	if !found {
		return Record{}, fmt.Errorf("RDAP response has no eventAction=expiration; cannot determine expiry")
	}
	return Record{
		Expires: expires,
		State:   strings.Join(raw.Status, ", "),
		Source:  SourceRDAP,
	}, nil
}

func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	layouts := []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
		"02-Jan-2006",
		"2006.01.02 15:04:05",
		"2006.01.02",
	}
	var first error
	for _, layout := range layouts {
		t, err := time.Parse(layout, s)
		if err == nil {
			return t.UTC(), nil
		}
		if first == nil {
			first = err
		}
	}
	return time.Time{}, first
}
