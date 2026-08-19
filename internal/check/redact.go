package check

import (
	"net/url"
	"regexp"
)

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

// MaskURL redacts userinfo from a URL so a status page or API response
// cannot leak credentials that were written into the URL itself
// (SPEC §11). Authorization headers are simply never serialized; this
// is the remaining leak. The host and path stay visible so the check
// is still identifiable.
func MaskURL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, hasPass := u.User.Password(); hasPass {
		// url.Redacted keeps the username and replaces only the
		// password. A username can itself be a token, so wipe both.
		u.User = url.UserPassword("xxxxx", "xxxxx")
		return u.String()
	}
	u.User = url.User("xxxxx")
	return u.String()
}
