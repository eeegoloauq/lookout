// Command lookout is a small uptime monitor: it probes the checks in a YAML
// configuration and remembers what it saw.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"syscall"
	"time"

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
	for _, c := range cfg.Checks {
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
		path, plural(len(cfg.Checks), "check"), plural(len(groups), "group"))
	for _, g := range names {
		fmt.Fprintf(out, "  %s: %d\n", g, groups[g])
	}
	fmt.Fprintf(out, "state file: %s\n", cfg.StateFile)
	fmt.Fprintf(out, "listen: %s\n", cfg.Listen)
	fmt.Fprintf(out, "alerting: telegram, batch window %s", cfg.Alerting.BatchWindow)
	if cfg.Alerting.Telegram.Proxy != "" {
		fmt.Fprintf(out, ", socks5 proxy configured")
	}
	fmt.Fprintln(out)
	if os.Getenv(config.EnvTelegramToken) == "" || os.Getenv(config.EnvTelegramChatID) == "" {
		fmt.Fprintf(out, "note: %s and %s must be set for lookout run to start\n",
			config.EnvTelegramToken, config.EnvTelegramChatID)
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

	notifier, err := alert.TelegramFromEnv(cfg.Alerting.Telegram.Proxy)
	if err != nil {
		return err
	}

	m := monitor.New(cfg, probe.NewHTTP(), monitor.WithLogger(log), monitor.WithNotifier(notifier))
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

func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, modified := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	switch {
	case rev == "":
		return "devel"
	case modified:
		return rev[:min(len(rev), 12)] + "-dirty"
	default:
		return rev[:min(len(rev), 12)]
	}
}
