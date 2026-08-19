// Command lookout is a small uptime monitor: it probes the checks in a YAML
// configuration and remembers what it saw.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	// The timezone database is embedded so that `timezone:` works on a
	// host without tzdata installed, which is most minimal containers.
	"time"
	_ "time/tzdata"

	"github.com/eeegoloauq/lookout/internal/alert"
	"github.com/eeegoloauq/lookout/internal/config"
	"github.com/eeegoloauq/lookout/internal/monitor"
	"github.com/eeegoloauq/lookout/internal/probe"
	"github.com/eeegoloauq/lookout/internal/web"
)

const defaultConfigPath = "config.yaml"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "lookout: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errors.New("no command given")
	}
	switch cmd := args[0]; cmd {
	case "validate":
		return validate(args[1:], stdout)
	case "run":
		return serve(args[1:], stderr)
	case "mute":
		return muteCmd(args[1:], stdout)
	case "unmute":
		return unmuteCmd(args[1:], stdout)
	case "test-alert":
		return testAlert(args[1:], stdout)
	case "version":
		fmt.Fprintln(stdout, version())
		return nil
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: lookout <command> [flags] [config]

commands:
  validate [config]   check the configuration and exit; prints every problem
                      it finds, with the line it is on
  run [-v] [config]   probe the configured checks and serve the status
                      page until interrupted
  mute --for 30m [--group NAME] [--check NAME] [config]
                      silence alerts without stopping probes; talks to the
                      running process over its HTTP listen address
  unmute [--group NAME] [--check NAME] [config]
                      lift an ad-hoc mute (scheduled windows stay)
  test-alert [config] send one message through the configured notifier
                      and exit; does not touch state or the outbox
  version             print the build version

The configuration defaults to `+defaultConfigPath+`.
`)
}

// validate is the guard the rest of the program relies on: a configuration
// that passes here cannot fail later for a reason that could have been seen
// up front (SPEC §1.5).
func validate(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(fs.Args())

	cfg, err := config.LoadFile(path)
	if err != nil {
		return err
	}

	groups := map[string]int{}
	written := 0
	for _, c := range cfg.Checks {
		if c.Implicit {
			// Derived registrations are counted on their own line below;
			// they are not something the operator grouped.
			continue
		}
		written++
		group := c.Group
		if group == "" {
			group = "(no group)"
		}
		groups[group]++
	}
	names := make([]string, 0, len(groups))
	for g := range groups {
		names = append(names, g)
	}
	sort.Strings(names)

	fmt.Fprintf(out, "%s: %s across %s, no problems found\n",
		path, plural(written, "check"), plural(len(groups), "group"))
	for _, g := range names {
		fmt.Fprintf(out, "  %s: %d\n", g, groups[g])
	}
	fmt.Fprintf(out, "state file: %s\n", cfg.StateFile)
	fmt.Fprintf(out, "history file: %s\n", cfg.HistoryFile)
	fmt.Fprintf(out, "samples file: %s\n", cfg.SamplesFile)
	fmt.Fprintf(out, "listen: %s\n", cfg.Listen)
	if n := len(cfg.Mute); n > 0 {
		fmt.Fprintf(out, "mute windows: %s\n", plural(n, "schedule"))
	}
	if cfg.Alerting.Mode == config.ModeNone {
		// Loud on purpose: a monitor that cannot notify is a dashboard.
		fmt.Fprintln(out, "alerting: DISABLED (alerting.mode: none) — nothing will ever be sent")
	} else {
		fmt.Fprintf(out, "alerting: telegram, batch window %s", cfg.Alerting.BatchWindow)
		if cfg.Alerting.Telegram.Proxy != "" {
			fmt.Fprintf(out, ", socks5 proxy configured")
		}
		fmt.Fprintln(out)
		// Reminder cadence is the setting most likely to surprise: it is
		// the only one that sends a message nobody's state change caused.
		if len(cfg.Alerting.Reminders) == 0 {
			fmt.Fprintln(out, "reminders: off — an open outage is reported once and never repeated")
		} else {
			parts := make([]string, 0, len(cfg.Alerting.Reminders))
			for _, d := range cfg.Alerting.Reminders {
				parts = append(parts, d.String())
			}
			fmt.Fprintf(out, "reminders: %s, then every %s while an outage stays open\n",
				strings.Join(parts, ", "), cfg.Alerting.Reminders[len(cfg.Alerting.Reminders)-1])
		}
		if cfg.Alerting.Heartbeat <= 0 {
			fmt.Fprintln(out, "heartbeat: off — lookout will never send a still-alive message")
		} else {
			fmt.Fprintf(out, "heartbeat: every %s\n", cfg.Alerting.Heartbeat)
		}
		if os.Getenv(config.EnvTelegramToken) == "" || os.Getenv(config.EnvTelegramChatID) == "" {
			fmt.Fprintf(out, "note: %s and %s must be set for lookout run to start\n",
				config.EnvTelegramToken, config.EnvTelegramChatID)
		}
	}

	// The registrations lookout worked out for itself are the one part of
	// the running configuration that is not in the file, so validate has to
	// say them out loud.
	var derived []string
	for _, c := range cfg.Checks {
		if c.Implicit && c.Type == config.TypeDomain {
			derived = append(derived, c.Host)
		}
	}
	if len(derived) > 0 {
		fmt.Fprintf(out, "registrations watched (derived from the checks above): %s\n", strings.Join(derived, ", "))
	}

	silent := 0
	for _, c := range cfg.Checks {
		if !c.Alert {
			silent++
		}
	}
	if silent > 0 {
		// Not an error, but worth saying out loud every time: silence is only
		// trustworthy when it is deliberate.
		fmt.Fprintf(out, "note: %s will never notify (alert: false)\n", plural(silent, "check"))
	}
	return nil
}

func serve(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	verbose := fs.Bool("v", false, "log every probe, not only state changes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(fs.Args())

	cfg, err := config.LoadFile(path)
	if err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := []monitor.Option{monitor.WithLogger(log)}
	if cfg.Alerting.Mode == config.ModeNone {
		log.Warn("alerting is disabled by configuration: state, page and metrics only")
	} else {
		notifier, err := alert.TelegramFromEnv(cfg.Alerting.Telegram.Proxy)
		if err != nil {
			return err
		}
		opts = append(opts, monitor.WithNotifier(notifier))
	}

	m := monitor.New(cfg, probe.New(), opts...)
	// Load durable state before the first HTTP request so a start page
	// hitting /api/status during startup does not see a blank machine
	// that is about to be restored.
	m.Restore()

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           web.New(m, version()),
		ReadHeaderTimeout: 5 * time.Second,
	}
	httpErr := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", cfg.Listen)
		err := httpSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- err
			stop()
			return
		}
		httpErr <- nil
	}()

	log.Info("lookout starting",
		"version", version(),
		"checks", len(cfg.Checks),
		"config", path,
		"state", cfg.StateFile,
		"listen", cfg.Listen,
		"batch_window", cfg.Alerting.BatchWindow,
		"telegram_proxy", cfg.Alerting.Telegram.Proxy != "")
	err = m.Run(ctx)

	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
	if herr := <-httpErr; herr != nil && err == nil {
		return herr
	}
	log.Info("lookout stopped")
	return err
}

// newNotifier is the production Telegram constructor. Tests replace it
// so a failed Bot API reply can be asserted without leaving the machine.
var newNotifier = func(proxy string) (alert.Notifier, error) {
	return alert.TelegramFromEnv(proxy)
}

// testAlert is a probe of the channel, not an event: it must not write
// the state file or the outbox, because a "can we still page?" check is
// not an incident and must not reset anyone's cooldown.
func testAlert(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("test-alert", flag.ContinueOnError)
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.LoadFile(configPath(fs.Args()))
	if err != nil {
		return err
	}
	if cfg.Alerting.Mode == config.ModeNone {
		return errors.New("alerting.mode is none: there is no channel to probe")
	}
	n, err := newNotifier(cfg.Alerting.Telegram.Proxy)
	if err != nil {
		return err
	}
	return probeChannel(n, cfg, out)
}

func probeChannel(n alert.Notifier, cfg *config.Config, out io.Writer) error {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	// The count is what the file says, not what lookout derived from it:
	// a probe of the channel that reports a number the operator cannot find
	// in their own config makes them doubt the wrong thing.
	written := 0
	for _, c := range cfg.Checks {
		if !c.Implicit {
			written++
		}
	}
	text := fmt.Sprintf("lookout test from %s, %s configured", host, plural(written, "check"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := n.Notify(ctx, text); err != nil {
		return err
	}
	fmt.Fprintln(out, text)
	return nil
}

func muteCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("mute", flag.ContinueOnError)
	fs.SetOutput(out)
	forFlag := fs.String("for", "", "how long to mute (30m, 2h, 1h30m)")
	group := fs.String("group", "", "limit the mute to this check group")
	check := fs.String("check", "", "limit the mute to this check")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *forFlag == "" {
		return errors.New("mute requires --for (a duration such as 30m)")
	}
	d, err := time.ParseDuration(*forFlag)
	if err != nil || d <= 0 {
		return fmt.Errorf("--for %q is not a positive duration (expected forms like 30m, 2h)", *forFlag)
	}
	cfg, err := config.LoadFile(configPath(fs.Args()))
	if err != nil {
		return err
	}
	body, _ := json.Marshal(web.MuteRequest{For: d.String(), Group: *group, Check: *check})
	resp, err := postJSON(dialURL(cfg.Listen, "/api/mute"), body)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	until := ""
	if resp.Until != nil {
		until = resp.Until.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(out, "muted until %s\n", until)
	return nil
}

func unmuteCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("unmute", flag.ContinueOnError)
	fs.SetOutput(out)
	group := fs.String("group", "", "lift only this group's ad-hoc mute")
	check := fs.String("check", "", "lift only this check's ad-hoc mute")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.LoadFile(configPath(fs.Args()))
	if err != nil {
		return err
	}
	body, _ := json.Marshal(web.UnmuteRequest{Group: *group, Check: *check})
	resp, err := postJSON(dialURL(cfg.Listen, "/api/unmute"), body)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	fmt.Fprintf(out, "unmuted (%s)\n", plural(resp.Cleared, "digest"))
	return nil
}

func dialURL(listen, path string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen + path
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + path
}

func postJSON(url string, body []byte) (web.MuteResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return web.MuteResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return web.MuteResponse{}, fmt.Errorf("cannot reach lookout at %s (is lookout run listening?): %w", url, err)
	}
	defer resp.Body.Close()
	var out web.MuteResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return web.MuteResponse{}, fmt.Errorf("lookout returned HTTP %d with an unreadable body: %w", resp.StatusCode, err)
	}
	if resp.StatusCode >= 400 && out.Error == "" {
		out.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return out, nil
}

func configPath(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return defaultConfigPath
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// version identifies the build the way someone asking "is this the one I
// deployed" needs it: a released tag when there is one, otherwise the short
// commit and the day it was built. A bare twelve-character hash answers
// neither question on its own.
// pseudoVersion matches Go's synthesised module versions.
var pseudoVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[\w.]+)?-\d{14}-[0-9a-f]{12}`)

func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, when, modified := "", "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
				when = t.UTC().Format("2 Jan 2006")
			}
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	tag := info.Main.Version
	// A module built outside a release gets a pseudo-version
	// (v0.0.0-20260819171115-0ca1f7e8d4d0): that is the commit again, in a
	// longer costume, and it is not what anyone means by a version.
	if tag == "(devel)" || pseudoVersion.MatchString(tag) {
		tag = ""
	}
	switch {
	case rev == "" && tag == "":
		return "devel"
	case rev == "":
		return tag
	}
	out := rev[:min(len(rev), 7)]
	if tag != "" {
		out = tag + " (" + out + ")"
	}
	if when != "" {
		out += " · " + when
	}
	if modified {
		out += " · uncommitted changes"
	}
	return out
}
