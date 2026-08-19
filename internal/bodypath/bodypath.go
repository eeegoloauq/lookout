// Package bodypath implements the "JSONPath-lite" expressions used to assert on
// response bodies. The grammar is deliberately tiny so that every expression can
// be validated when the configuration is loaded rather than while a probe runs:
//
//	path    := segment+
//	segment := "." field | "[" index "]"
//	field   := [A-Za-z0-9_-]+
//	index   := [0-9]+
//
// Example: .result.source.online, .items[0].name
package bodypath

import (
	"fmt"
	"strconv"
	"strings"
)

// Path is a compiled expression. The zero value is not usable; use Parse.
type Path struct {
	raw      string
	segments []segment
}

type segment struct {
	field string
	index int
	isIdx bool
}

// Parse compiles a path expression, reporting the offset of the first problem.
func Parse(s string) (Path, error) {
	if s == "" {
		return Path{}, fmt.Errorf("empty path")
	}
	if s[0] != '.' && s[0] != '[' {
		return Path{}, fmt.Errorf("path %q must start with %q or %q", s, ".", "[")
	}
	p := Path{raw: s}
	for i := 0; i < len(s); {
		switch s[i] {
		case '.':
			j := i + 1
			for j < len(s) && isFieldByte(s[j]) {
				j++
			}
			if j == i+1 {
				return Path{}, fmt.Errorf("path %q: expected a field name after %q at offset %d", s, ".", i)
			}
			p.segments = append(p.segments, segment{field: s[i+1 : j]})
			i = j
		case '[':
			j := i + 1
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			if j == i+1 || j >= len(s) || s[j] != ']' {
				return Path{}, fmt.Errorf("path %q: expected %q at offset %d", s, "[<number>]", i)
			}
			n, err := strconv.Atoi(s[i+1 : j])
			if err != nil {
				return Path{}, fmt.Errorf("path %q: bad array index at offset %d: %w", s, i, err)
			}
			p.segments = append(p.segments, segment{index: n, isIdx: true})
			i = j + 1
		default:
			return Path{}, fmt.Errorf("path %q: unexpected %q at offset %d, expected %q or %q", s, string(s[i]), i, ".", "[")
		}
	}
	return p, nil
}

func isFieldByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}

// String returns the original expression.
func (p Path) String() string { return p.raw }

// Lookup walks decoded JSON. The second return value reports whether the path
// exists at all; that distinction is what separates a "wrong value" outcome from
// a "malformed response" one.
func (p Path) Lookup(v any) (any, bool) {
	cur := v
	for _, seg := range p.segments {
		if seg.isIdx {
			arr, ok := cur.([]any)
			if !ok || seg.index >= len(arr) {
				return nil, false
			}
			cur = arr[seg.index]
			continue
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[seg.field]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Prefixes returns the intermediate paths, shortest first, excluding the full
// path itself. It lets a caller report the deepest part of a path that did
// resolve, which is the useful half of a "field is missing" message.
func (p Path) Prefixes() []string {
	var out []string
	var b strings.Builder
	for _, seg := range p.segments[:max(len(p.segments)-1, 0)] {
		if seg.isIdx {
			fmt.Fprintf(&b, "[%d]", seg.index)
		} else {
			b.WriteString("." + seg.field)
		}
		out = append(out, b.String())
	}
	return out
}
