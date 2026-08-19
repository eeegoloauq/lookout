package check

import "regexp"

// RedactSecrets strips credentials that a response body or an error string
// might echo, so they cannot leak into a log line or an alert (SPEC §11).
// The patterns target authorization headers and bearer tokens, not every
// field named "password": over-redacting turns a useful body sample into
// noise, and the founding requirement is that an alert still explains
// what failed.
func RedactSecrets(s string) string {
	if s == "" {
		return s
	}
	s = authHeader.ReplaceAllString(s, "${1}[redacted]")
	s = authJSON.ReplaceAllString(s, `${1}[redacted]$3`)
	s = bearerToken.ReplaceAllString(s, "${1}[redacted]")
	return s
}

var (
	// Authorization: Bearer xxx / Basic xxx, and the JSON equivalent
	// "authorization":"...". The replacement keeps the key so the sample
	// still shows that a credential was present, just not its value.
	authHeader  = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(?:[Bb]earer\s+\S+|[Bb]asic\s+\S+|\S+)`)
	authJSON    = regexp.MustCompile(`(?i)("authorization"\s*:\s*")([^"]*)(")`)
	bearerToken = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._\-+/=]+`)
)
