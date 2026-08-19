package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseWHOISTcinet(t *testing.T) {
	rec, err := ParseWHOIS(string(testdata(t, "whois_ru.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Source != SourceWHOIS {
		t.Errorf("source = %q", rec.Source)
	}
	want := time.Date(2027, 4, 30, 21, 0, 0, 0, time.UTC)
	if !rec.Expires.Equal(want) {
		t.Errorf("expires = %s, want %s (paid-till is Moscow midnight in UTC)", rec.Expires, want)
	}
	if !rec.FreeDate.Equal(time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("free-date = %s", rec.FreeDate)
	}
	if rec.State != "REGISTERED, DELEGATED, VERIFIED" {
		t.Errorf("state = %q", rec.State)
	}
}

func TestParseWHOISVerisign(t *testing.T) {
	rec, err := ParseWHOIS(string(testdata(t, "whois_com.txt")))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2027, 8, 13, 4, 0, 0, 0, time.UTC)
	if !rec.Expires.Equal(want) {
		t.Errorf("expires = %s, want %s", rec.Expires, want)
	}
}

func TestParseWHOISUnrecognisedIsAnError(t *testing.T) {
	_, err := ParseWHOIS("domain: SERVICE.EXAMPLE\nregistrar: Example\n")
	if err == nil {
		t.Fatal("want an error, got a silent zero record")
	}
	if !strings.Contains(err.Error(), "no recognisable expiry field") {
		t.Errorf("error = %q", err)
	}
	if !strings.Contains(err.Error(), "paid-till") || !strings.Contains(err.Error(), "saw domain, registrar") {
		t.Errorf("error should name what was looked for and what was seen: %q", err)
	}
}

func TestParseWHOISGarbageDateIsAnError(t *testing.T) {
	_, err := ParseWHOIS("paid-till: not-a-date\n")
	if err == nil {
		t.Fatal("want an error for an unparseable paid-till")
	}
	if !strings.Contains(err.Error(), "not a date") {
		t.Errorf("error = %q", err)
	}
}

func TestParseIANAReferral(t *testing.T) {
	host, err := ParseIANAReferral(string(testdata(t, "whois_iana_ru.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if host != "whois.tcinet.ru" {
		t.Errorf("host = %q", host)
	}
}

func TestParseIANAReferralMissing(t *testing.T) {
	_, err := ParseIANAReferral("domain: EXAMPLE-TLD\nstatus: ACTIVE\n")
	if err == nil {
		t.Fatal("want an error")
	}
}

func TestParseRDAP(t *testing.T) {
	rec, err := ParseRDAP(testdata(t, "rdap_com.json"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Source != SourceRDAP {
		t.Errorf("source = %q", rec.Source)
	}
	want := time.Date(2027, 8, 13, 4, 0, 0, 0, time.UTC)
	if !rec.Expires.Equal(want) {
		t.Errorf("expires = %s, want %s", rec.Expires, want)
	}
}

func TestParseRDAPMissingExpiration(t *testing.T) {
	_, err := ParseRDAP([]byte(`{"events":[{"eventAction":"registration","eventDate":"1995-08-14T04:00:00Z"}]}`))
	if err == nil {
		t.Fatal("want an error when expiration is absent")
	}
	if !strings.Contains(err.Error(), "expiration") {
		t.Errorf("error = %q", err)
	}
}

func TestParseRDAPErrorObject(t *testing.T) {
	_, err := ParseRDAP([]byte(`{"errorCode":404,"title":"Not Found"}`))
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %q", err)
	}
}

func TestParseBootstrap(t *testing.T) {
	b, err := ParseBootstrap(testdata(t, "rdap_bootstrap.json"))
	if err != nil {
		t.Fatal(err)
	}
	if b.Services["com"] != "https://rdap.registry.example/com/v1/" {
		t.Errorf("com = %q", b.Services["com"])
	}
	if b.Services["dev"] != "https://rdap.registry.example/dev/" {
		t.Errorf("dev = %q", b.Services["dev"])
	}
	if _, ok := b.Services["ru"]; ok {
		t.Error("fixture must not invent an RDAP service for ru")
	}
	if !b.Publication.Equal(time.Date(2026, 7, 23, 2, 0, 3, 0, time.UTC)) {
		t.Errorf("publication = %s", b.Publication)
	}
}

func TestDefaultWHOISCoversTcinetZones(t *testing.T) {
	for _, tld := range []string{"ru", "su", "xn--p1ai"} {
		host, ok := DefaultWHOIS(tld)
		if !ok || host != "whois.tcinet.ru" {
			t.Errorf("%s → %q ok=%v, want whois.tcinet.ru", tld, host, ok)
		}
	}
	if _, ok := DefaultWHOIS("com"); ok {
		t.Error("com has RDAP; it must not be in the WHOIS fallback table")
	}
}

func TestTLD(t *testing.T) {
	if got := TLD("service.example"); got != "example" {
		t.Errorf("TLD = %q", got)
	}
	if got := TLD("xn--e1afmkfd.xn--p1ai"); got != "xn--p1ai" {
		t.Errorf("TLD = %q", got)
	}
}
