package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eeegoloauq/lookout/internal/alert"
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

// The cadence is the setting most likely to surprise after reminders:
// a missing still-alive message is only trustworthy when it was asked for.
func TestValidateNotesHeartbeatOff(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"validate", write(t, valid)}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "heartbeat: off") {
		t.Errorf("validate output = %q, want heartbeat off named the way reminders are", out.String())
	}
}

// Same reason reminders are printed: a weekly ping nobody noticed
// in the file is how a deadman fails closed.
func TestValidateNotesHeartbeatSchedule(t *testing.T) {
	var out, errOut bytes.Buffer
	src := valid + "\nalerting:\n  heartbeat: 168h\n"
	if err := run([]string{"validate", write(t, src)}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "heartbeat: every") || !strings.Contains(out.String(), "168h") {
		t.Errorf("validate output = %q, want the heartbeat cadence", out.String())
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
	cfg.SamplesFile = filepath.Join(dir, "samples.jsonl")
	m := monitor.New(cfg, nil, monitor.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	srv := httptest.NewServer(web.New(m, "test", ""))
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

// A monitor whose alerting is switched off has no channel. Pretending
// the probe succeeded would be the silent-failure mode lookout exists
// to prevent.
func TestTestAlertRejectsModeNone(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"test-alert", write(t, valid+"\nalerting:\n  mode: none\n")}, &out, &errOut)
	if err == nil {
		t.Fatal("test-alert with mode: none must be an error")
	}
	if !strings.Contains(err.Error(), "none") {
		t.Errorf("error = %q, want it to name mode: none", err)
	}
}

// Same credentials as `run`: a missing token must fail here too, or
// test-alert would be a green light for a process that cannot page.
func TestTestAlertRequiresTelegramCredentials(t *testing.T) {
	t.Setenv("LOOKOUT_TELEGRAM_TOKEN", "")
	t.Setenv("LOOKOUT_TELEGRAM_CHAT_ID", "")
	var out, errOut bytes.Buffer
	err := run([]string{"test-alert", write(t, valid)}, &out, &errOut)
	if err == nil {
		t.Fatal("test-alert without telegram credentials must be an error")
	}
	if !strings.Contains(err.Error(), "LOOKOUT_TELEGRAM_TOKEN") {
		t.Errorf("error = %q, want it to name the missing variable", err)
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

// The command is how an operator proves the channel before the first
// outage; it has to be visible next to validate and run, not hidden.
func TestHelpListsTestAlert(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"help"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "test-alert") {
		t.Errorf("usage = %q, want test-alert listed next to the other commands", out.String())
	}
}

func stubTelegram(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	orig := newNotifier
	t.Cleanup(func() { newNotifier = orig })
	newNotifier = func(string) (alert.Notifier, error) {
		tg, err := alert.NewTelegram("123:test-token", "1", "")
		if err != nil {
			return nil, err
		}
		tg.SetAPI(srv.URL)
		return tg, nil
	}
}

// The whole point of the command is to find out the channel is dead
// *before* an outage, with the Bot API's own words, not a generic failure.
func TestTestAlertPrintsWhyDeliveryFailed(t *testing.T) {
	stubTelegram(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized: bad token"}`))
	})
	var out, errOut bytes.Buffer
	err := run([]string{"test-alert", write(t, valid)}, &out, &errOut)
	if err == nil {
		t.Fatal("a rejected sendMessage must be an error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %q, want the HTTP status", err)
	}
	if !strings.Contains(err.Error(), "Unauthorized: bad token") {
		t.Errorf("error = %q, want Telegram's description", err)
	}
}

// Exit 0 is the only success signal the command has, so it must mean
// sendMessage returned ok, not merely that lookout built a payload.
func TestTestAlertSucceedsWhenBotAPIConfirms(t *testing.T) {
	var gotText string
	stubTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text string `json:"text"`
		}
		_ = jsonDecode(r, &req)
		gotText = req.Text
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var out, errOut bytes.Buffer
	if err := run([]string{"test-alert", write(t, valid)}, &out, &errOut); err != nil {
		t.Fatalf("test-alert: %v", err)
	}
	want := "lookout test from " + host + ", 1 check configured"
	if gotText != want {
		t.Errorf("sent %q, want %q", gotText, want)
	}
	if !strings.Contains(out.String(), want) {
		t.Errorf("stdout = %q, want the delivered text", out.String())
	}
}

func jsonDecode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// A probe of the channel is not an incident. Writing the state file would
// make a successful test look like a restart, and an unsuccessful one
// like a queued outage.
func TestTestAlertDoesNotTouchTheStateFile(t *testing.T) {
	stubTelegram(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	})
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	cfgPath := filepath.Join(dir, "config.yaml")
	src := "state:\n  file: " + statePath + "\n" + valid
	if err := os.WriteFile(cfgPath, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := run([]string{"test-alert", cfgPath}, &out, &errOut); err != nil {
		t.Fatalf("test-alert: %v", err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state file was written: %v", err)
	}
}

// A pseudo-version is the commit again in a longer costume. Printing one where
// a release number belongs tells a reader they are running something they are
// not, so every shape Go synthesises has to be recognised.
func TestPseudoVersionsAreNotMistakenForReleases(t *testing.T) {
	for _, v := range []string{
		"v0.0.0-20260820215511-f8380ee4d13c",
		"v0.1.1-0.20260820215511-f8380ee4d13c",
		"v0.1.1-rc.2.0.20260820215511-f8380ee4d13c",
		"v0.1.1-0.20260820215511-f8380ee4d13c+dirty",
	} {
		if !pseudoVersion.MatchString(v) {
			t.Errorf("%q was taken for a release", v)
		}
	}
	for _, v := range []string{"v0.1.0", "v1.2.3-rc1", "v2.0.0+meta"} {
		if pseudoVersion.MatchString(v) {
			t.Errorf("%q was taken for a pseudo-version", v)
		}
	}
}
