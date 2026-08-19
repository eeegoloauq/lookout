package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// StatusMatcher matches an HTTP status code. It is written in YAML as a plain
// number (200), an inclusive range ("200-299") or a comparison ("<500", ">=200").
// The zero value matches nothing and reports IsZero.
type StatusMatcher struct {
	raw string
	op  string // "", "-", "<", "<=", ">", ">="
	lo  int
	hi  int
}

// ParseStatus compiles a status expectation from a YAML scalar.
func ParseStatus(v any) (StatusMatcher, error) {
	switch t := v.(type) {
	case int:
		return statusExact(t)
	case uint64:
		return statusExact(int(t))
	case int64:
		return statusExact(int(t))
	case float64:
		if t != float64(int(t)) {
			return StatusMatcher{}, fmt.Errorf("status %v is not a whole number", t)
		}
		return statusExact(int(t))
	case string:
		return parseStatusString(t)
	default:
		return StatusMatcher{}, fmt.Errorf("status must be a number, a range like %q or a comparison like %q, got %T", "200-299", "<500", v)
	}
}

func statusExact(code int) (StatusMatcher, error) {
	if err := validCode(code); err != nil {
		return StatusMatcher{}, err
	}
	return StatusMatcher{raw: strconv.Itoa(code), lo: code, hi: code}, nil
}

func parseStatusString(s string) (StatusMatcher, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return StatusMatcher{}, fmt.Errorf("status is empty")
	}
	for _, op := range []string{"<=", ">=", "<", ">"} {
		if strings.HasPrefix(t, op) {
			code, err := strconv.Atoi(strings.TrimSpace(t[len(op):]))
			if err != nil {
				return StatusMatcher{}, fmt.Errorf("status %q: %q must be followed by a number", s, op)
			}
			if err := validCode(code); err != nil {
				return StatusMatcher{}, err
			}
			return StatusMatcher{raw: t, op: op, lo: code}, nil
		}
	}
	if lo, hi, ok := strings.Cut(t, "-"); ok {
		l, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return StatusMatcher{}, fmt.Errorf("status range %q: %q is not a number", s, lo)
		}
		h, err := strconv.Atoi(strings.TrimSpace(hi))
		if err != nil {
			return StatusMatcher{}, fmt.Errorf("status range %q: %q is not a number", s, hi)
		}
		if err := validCode(l); err != nil {
			return StatusMatcher{}, err
		}
		if err := validCode(h); err != nil {
			return StatusMatcher{}, err
		}
		if l > h {
			return StatusMatcher{}, fmt.Errorf("status range %q is inverted: %d is above %d", s, l, h)
		}
		return StatusMatcher{raw: t, op: "-", lo: l, hi: h}, nil
	}
	code, err := strconv.Atoi(t)
	if err != nil {
		return StatusMatcher{}, fmt.Errorf("status %q is not a number, a range like %q or a comparison like %q", s, "200-299", "<500")
	}
	return statusExact(code)
}

func validCode(code int) error {
	if code < 100 || code > 599 {
		return fmt.Errorf("status %d is outside the HTTP range 100-599", code)
	}
	return nil
}

// IsZero reports whether the matcher was never set.
func (m StatusMatcher) IsZero() bool { return m.raw == "" }

// Match reports whether code satisfies the expectation.
func (m StatusMatcher) Match(code int) bool {
	switch m.op {
	case "":
		return code == m.lo
	case "-":
		return code >= m.lo && code <= m.hi
	case "<":
		return code < m.lo
	case "<=":
		return code <= m.lo
	case ">":
		return code > m.lo
	case ">=":
		return code >= m.lo
	}
	return false
}

func (m StatusMatcher) String() string { return m.raw }

// DurationMatcher matches a duration against a comparison such as "<5s".
type DurationMatcher struct {
	raw string
	op  string
	d   time.Duration
}

// ParseDurationMatcher compiles a duration comparison.
func ParseDurationMatcher(s string) (DurationMatcher, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return DurationMatcher{}, fmt.Errorf("empty duration condition")
	}
	for _, op := range []string{"<=", ">=", "<", ">"} {
		if strings.HasPrefix(t, op) {
			d, err := time.ParseDuration(strings.TrimSpace(t[len(op):]))
			if err != nil {
				return DurationMatcher{}, fmt.Errorf("%q: %w", s, err)
			}
			if d <= 0 {
				return DurationMatcher{}, fmt.Errorf("%q: duration must be positive", s)
			}
			return DurationMatcher{raw: t, op: op, d: d}, nil
		}
	}
	return DurationMatcher{}, fmt.Errorf("%q must start with a comparison, for example %q", s, "<5s")
}

// IsZero reports whether the matcher was never set.
func (m DurationMatcher) IsZero() bool { return m.raw == "" }

// Match reports whether d satisfies the expectation.
func (m DurationMatcher) Match(d time.Duration) bool {
	switch m.op {
	case "<":
		return d < m.d
	case "<=":
		return d <= m.d
	case ">":
		return d > m.d
	case ">=":
		return d >= m.d
	}
	return false
}

func (m DurationMatcher) String() string { return m.raw }
