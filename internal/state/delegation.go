package state

import "strings"

// tcinetDelegation reads a WHOIS/RDAP state string for the tcinet
// DELEGATED flag. gTLD status codes (clientTransferProhibited, …)
// are a different vocabulary: a missing DELEGATED token there is not
// an emergency, so known is false.
//
// Tokens are comma-separated. "NOT DELEGATED" is a single token and
// must not be confused with a substring of DELEGATED.
func tcinetDelegation(state string) (delegated, known bool) {
	if strings.TrimSpace(state) == "" {
		return false, false
	}
	var sawTCI, sawDelegated, sawNot bool
	for _, tok := range strings.Split(state, ",") {
		tok = strings.ToUpper(strings.TrimSpace(tok))
		switch tok {
		case "DELEGATED":
			sawDelegated = true
			sawTCI = true
		case "NOT DELEGATED":
			sawNot = true
			sawTCI = true
		case "REGISTERED", "VERIFIED", "UNVERIFIED":
			sawTCI = true
		}
	}
	if !sawTCI {
		return false, false
	}
	if sawDelegated && !sawNot {
		return true, true
	}
	return false, true
}
