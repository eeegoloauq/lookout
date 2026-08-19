package check

import (
	"strings"
	"testing"
)

func TestRedactSecretsStripsAuthorization(t *testing.T) {
	tests := []struct {
		name, in, leak string
	}{
		{"header colon", "Authorization: Bearer s3cret-token-value\nOK", "s3cret-token-value"},
		{"header equals", "authorization=Basic dXNlcjpwYXNz", "dXNlcjpwYXNz"},
		{"json field", `{"authorization":"Bearer s3cret-token-value","ok":true}`, "s3cret-token-value"},
		{"bare bearer", "upstream said Bearer s3cret-token-value", "s3cret-token-value"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSecrets(tc.in)
			if got == "" {
				t.Fatal("redaction emptied the string")
			}
			if strings.Contains(got, tc.leak) {
				t.Errorf("redacted text still contains the secret %q: %q", tc.leak, got)
			}
			if got == tc.in {
				t.Errorf("nothing was redacted in %q", tc.in)
			}
		})
	}
}

func TestMaskURLRedactsUserinfo(t *testing.T) {
	tests := []struct {
		in, leak, wantSub string
	}{
		{"https://user:s3cret-pass@private.invalid/v1", "s3cret-pass", "xxxxx:xxxxx@private.invalid"},
		{"https://s3cret-token@private.invalid/v1", "s3cret-token", "xxxxx@private.invalid"},
		{"http://public.invalid/health", "", "http://public.invalid/health"},
	}
	for _, tc := range tests {
		got := MaskURL(tc.in)
		if tc.leak != "" && strings.Contains(got, tc.leak) {
			t.Errorf("MaskURL(%q) still contains %q: %q", tc.in, tc.leak, got)
		}
		if tc.wantSub != "" && !strings.Contains(got, tc.wantSub) {
			t.Errorf("MaskURL(%q) = %q, want it to contain %q", tc.in, got, tc.wantSub)
		}
	}
}

func TestSampleRedactsBeforeTruncating(t *testing.T) {
	// A token that starts inside the first 200 bytes would otherwise leak as
	// a truncated prefix. Redact first, then cut.
	body := []byte("Authorization: Bearer super-secret-token-that-must-not-leak x")
	got := Sample(body, 40)
	if strings.Contains(got, "super-secret-token-that-must-not-leak") {
		t.Errorf("sample leaked the token: %q", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Errorf("sample = %q, want it to show the redaction marker", got)
	}
}
