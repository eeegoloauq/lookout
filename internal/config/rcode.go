package config

import (
	"fmt"
	"strings"
)

// DefaultRcode is applied when a dns check declares no rcode expectation.
// Without it a SERVFAIL would report "up", which is the silent-failure
// mode SPEC §1.1 exists to prevent.
const DefaultRcode = "NOERROR"

// KnownRcodes is the RCODE set a dns check may name, in IANA spelling.
var KnownRcodes = []string{
	"NOERROR",
	"FORMERR",
	"SERVFAIL",
	"NXDOMAIN",
	"NOTIMP",
	"REFUSED",
}

var rcodeAliases = map[string]string{
	"NOERROR":   "NOERROR",
	"SUCCESS":   "NOERROR",
	"FORMERR":   "FORMERR",
	"SERVFAIL":  "SERVFAIL",
	"NXDOMAIN":  "NXDOMAIN",
	"NAMEERROR": "NXDOMAIN",
	"NOTIMP":    "NOTIMP",
	"REFUSED":   "REFUSED",
}

// ParseRcode compiles a DNS RCODE name. The result is the canonical
// uppercase token, so a typo is a load-time error, not a silent miss
// at probe time.
func ParseRcode(s string) (string, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if t == "" {
		return "", fmt.Errorf("rcode is empty")
	}
	if canon, ok := rcodeAliases[t]; ok {
		return canon, nil
	}
	return "", fmt.Errorf("unknown rcode %q, expected one of %s", s, strings.Join(KnownRcodes, ", "))
}
