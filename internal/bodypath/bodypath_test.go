package bodypath

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseRejects(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "empty path"},
		{"result.online", `must start with "." or "["`},
		{".result..online", `expected a field name after "."`},
		{".result.", `expected a field name after "."`},
		{".items[", "[<number>]"},
		{".items[a]", "[<number>]"},
		{".items[0", "[<number>]"},
		{".a b", "unexpected"},
	}
	for _, tc := range tests {
		if _, err := Parse(tc.in); err == nil {
			t.Errorf("Parse(%q) = nil error, want %q", tc.in, tc.want)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Parse(%q) = %v, want it to contain %q", tc.in, err, tc.want)
		}
	}
}

func TestLookup(t *testing.T) {
	var doc any
	body := `{"result":{"source":{"online":true}},"items":[{"name":"a"},{"name":"b"}],"n":3,"nil":null}`
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		path  string
		want  any
		found bool
	}{
		{".result.source.online", true, true},
		{".items[1].name", "b", true},
		{".n", float64(3), true},
		{".nil", nil, true},
		{".result.source.offline", nil, false},
		{".items[9].name", nil, false},
		{".result.source.online.deeper", nil, false},
		{".items.name", nil, false},
	}
	for _, tc := range tests {
		p, err := Parse(tc.path)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.path, err)
		}
		got, found := p.Lookup(doc)
		if found != tc.found || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Lookup(%q) = %v, %v; want %v, %v", tc.path, got, found, tc.want, tc.found)
		}
	}
}

// A null value present in the body is not the same as an absent field: the
// first is a value we can compare, the second is a malformed response.
func TestLookupDistinguishesNullFromAbsent(t *testing.T) {
	var doc any
	if err := json.Unmarshal([]byte(`{"a":null}`), &doc); err != nil {
		t.Fatal(err)
	}
	p, _ := Parse(".a")
	if v, found := p.Lookup(doc); !found || v != nil {
		t.Errorf("Lookup(.a) = %v, %v; want nil, true", v, found)
	}
	q, _ := Parse(".b")
	if _, found := q.Lookup(doc); found {
		t.Error("Lookup(.b) reported found for an absent field")
	}
}

func TestPrefixes(t *testing.T) {
	p, err := Parse(".result.source.online")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".result", ".result.source"}
	if got := p.Prefixes(); !reflect.DeepEqual(got, want) {
		t.Errorf("Prefixes() = %v, want %v", got, want)
	}
	single, _ := Parse(".a")
	if got := single.Prefixes(); len(got) != 0 {
		t.Errorf("Prefixes() on a single segment = %v, want none", got)
	}
}
