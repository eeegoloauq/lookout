package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const valid = `
checks:
  - name: Example
    type: http
    url: http://example.invalid:8080
`

func TestValidateAcceptsAGoodConfig(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"validate", write(t, valid)}, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "1 check") || !strings.Contains(out.String(), "no problems found") {
		t.Errorf("output = %q", out.String())
	}
}

// A broken configuration must produce a readable error with a line number, and
// must never take the process down by surprise.
func TestValidateReportsProblemsWithLines(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"validate", write(t, "checks:\n  - name: Example\n    type: http\n    url: nope\n")}, &out, &errOut)
	if err == nil {
		t.Fatal("a broken config must be an error")
	}
	if !strings.Contains(err.Error(), "config.yaml:4") || !strings.Contains(err.Error(), "no scheme") {
		t.Errorf("error = %q, want it to name the line and the problem", err)
	}
}

func TestValidatePointsOutChecksThatNeverNotify(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"validate", write(t, valid+"    alert: false\n")}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "will never notify") {
		t.Errorf("output = %q, want a note about the silent check", out.String())
	}
}

func TestUnknownCommandAndMissingFile(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"nonsense"}, &out, &errOut); err == nil {
		t.Error("an unknown command must be an error")
	}
	if !strings.Contains(errOut.String(), "usage:") {
		t.Error("an unknown command must print the usage")
	}
	if err := run([]string{"validate", "does-not-exist.yaml"}, &out, &errOut); err == nil {
		t.Error("a missing config file must be an error")
	}
}

func TestRunRequiresTelegramCredentials(t *testing.T) {
	t.Setenv("LOOKOUT_TELEGRAM_TOKEN", "")
	t.Setenv("LOOKOUT_TELEGRAM_CHAT_ID", "")
	var out, errOut bytes.Buffer
	err := run([]string{"run", write(t, valid)}, &out, &errOut)
	if err == nil {
		t.Fatal("run without telegram credentials must be an error")
	}
	if !strings.Contains(err.Error(), "LOOKOUT_TELEGRAM_TOKEN") {
		t.Errorf("error = %q, want it to name the missing variable", err)
	}
}

func TestValidateNotesMissingTelegramCredentials(t *testing.T) {
	t.Setenv("LOOKOUT_TELEGRAM_TOKEN", "")
	t.Setenv("LOOKOUT_TELEGRAM_CHAT_ID", "")
	var out, errOut bytes.Buffer
	if err := run([]string{"validate", write(t, valid)}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "LOOKOUT_TELEGRAM_TOKEN") {
		t.Errorf("validate output = %q, want a note about the telegram environment", out.String())
	}
}

func TestHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"version"}} {
		var out, errOut bytes.Buffer
		if err := run(args, &out, &errOut); err != nil {
			t.Errorf("%v: %v", args, err)
		}
		if out.Len() == 0 {
			t.Errorf("%v printed nothing", args)
		}
	}
}
