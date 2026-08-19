package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eeegoloauq/lookout/internal/config"
	"github.com/eeegoloauq/lookout/internal/monitor"
	"github.com/eeegoloauq/lookout/internal/web"
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
	if !strings.Contains(out.String(), "listen: "+config.DefaultListen) {
		t.Errorf("output = %q, want the loopback listen address", out.String())
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

func TestMuteCommandTalksToTheRunningProcess(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load("config.yaml", []byte(valid+"    group: Services\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.HistoryFile = filepath.Join(dir, "history.jsonl")
	m := monitor.New(cfg, nil, monitor.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	srv := httptest.NewServer(web.New(m, "test"))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "config.yaml")
	src := "listen: " + u.Host + "\n" + valid + "    group: Services\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if err := run([]string{"mute", "--for", "30m", "--group", "Services", path}, &out, &errOut); err != nil {
		t.Fatalf("mute: %v", err)
	}
	if !strings.Contains(out.String(), "muted until") {
		t.Errorf("mute output = %q", out.String())
	}
	if !m.CheckMuted("Services", "Example", time.Now()) {
		t.Fatal("CLI mute did not reach the running process")
	}

	out.Reset()
	if err := run([]string{"unmute", "--group", "Services", path}, &out, &errOut); err != nil {
		t.Fatalf("unmute: %v", err)
	}
	if m.CheckMuted("Services", "Example", time.Now()) {
		t.Fatal("unmute did not lift the hold")
	}
}

func TestMuteRequiresFor(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"mute", write(t, valid)}, &out, &errOut); err == nil {
		t.Fatal("mute without --for must be an error")
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
