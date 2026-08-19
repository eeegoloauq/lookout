package config

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/eeegoloauq/lookout/internal/bodypath"
	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/parser"
)

// The file* types mirror the YAML document one-to-one. Optional scalars are
// pointers so that "absent" is distinguishable from "set to the zero value",
// and every mini-language arrives as a plain string or scalar: converting it
// here, rather than in an UnmarshalYAML method, is what lets an error carry the
// line it came from.
type fileConfig struct {
	Listen            *string       `yaml:"listen"`
	Timezone          *string       `yaml:"timezone"`
	RegistrationGroup *string       `yaml:"registration_group"`
	State             *fileState    `yaml:"state"`
	Defaults          *fileDefaults `yaml:"defaults"`
	Alerting          *fileAlerting `yaml:"alerting"`
	Mute              []fileMute    `yaml:"mute"`
	Checks            []fileCheck   `yaml:"checks"`
}

type fileAlerting struct {
	Mode        *string       `yaml:"mode"`
	BatchWindow *string       `yaml:"batch_window"`
	Reminders   *[]string     `yaml:"reminders"`
	Telegram    *fileTelegram `yaml:"telegram"`
}

// token/chat_id fields exist so a misplaced secret gets a specific error
// instead of yaml.Strict()'s generic "unknown field".
type fileTelegram struct {
	Proxy    *string `yaml:"proxy"`
	Token    *string `yaml:"token"`
	BotToken *string `yaml:"bot_token"`
	ChatID   *string `yaml:"chat_id"`
}

type fileState struct {
	File    *string `yaml:"file"`
	History *string `yaml:"history"`
}

// fileMute is one static quiet window. Cron is accepted only so we can
// reject it with a specific error instead of yaml.Strict()'s generic
// "unknown field".
type fileMute struct {
	Every    []string `yaml:"every"`
	At       *string  `yaml:"at"`
	Duration *string  `yaml:"duration"`
	Timezone *string  `yaml:"timezone"`
	Group    *string  `yaml:"group"`
	Check    *string  `yaml:"check"`
	Cron     *string  `yaml:"cron"`
}

type fileDefaults struct {
	Interval         *string          `yaml:"interval"`
	Timeout          *string          `yaml:"timeout"`
	Method           *string          `yaml:"method"`
	Alert            *bool            `yaml:"alert"`
	FailureThreshold *int             `yaml:"failure_threshold"`
	SuccessThreshold *int             `yaml:"success_threshold"`
	Instability      *fileInstability `yaml:"instability"`
}

type fileInstability struct {
	Failures *int    `yaml:"failures"`
	Window   *int    `yaml:"window"`
	Cooldown *string `yaml:"cooldown"`
}

type fileCheck struct {
	Name             string            `yaml:"name"`
	Registration     *string           `yaml:"registration"`
	Group            string            `yaml:"group"`
	Type             string            `yaml:"type"`
	URL              string            `yaml:"url"`
	Host             string            `yaml:"host"`
	Domain           string            `yaml:"domain"`
	QueryType        string            `yaml:"query_type"`
	Resolver         *string           `yaml:"resolver"`
	Method           *string           `yaml:"method"`
	Headers          map[string]string `yaml:"headers"`
	Interval         *string           `yaml:"interval"`
	Timeout          *string           `yaml:"timeout"`
	Expect           *fileExpect       `yaml:"expect"`
	Alert            *bool             `yaml:"alert"`
	FailureThreshold *int              `yaml:"failure_threshold"`
	SuccessThreshold *int              `yaml:"success_threshold"`
	Instability      *fileInstability  `yaml:"instability"`
}

type fileExpect struct {
	Status         any           `yaml:"status"`
	Body           yaml.MapSlice `yaml:"body"`
	BodyContains   *string       `yaml:"body_contains"`
	ResponseTime   *string       `yaml:"response_time"`
	Rcode          *string       `yaml:"rcode"`
	AnswersContain *string       `yaml:"answers_contain"`
}

// LoadFile reads and validates a configuration file. On failure the error is an
// Errors value listing every problem found, each with a source position.
func LoadFile(name string) (*Config, error) {
	src, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return Load(name, src)
}

// Load validates configuration source that has already been read. name is used
// only to label error messages.
func Load(name string, src []byte) (*Config, error) {
	file, parseErr := parser.ParseBytes(src, 0)
	s := &source{name: name, file: file}

	var raw fileConfig
	// Strict rejects unknown fields and duplicate keys: a typo in a field name
	// must not silently disable a check.
	decErr := yaml.UnmarshalWithOptions(src, &raw, yaml.Strict())
	if decErr != nil {
		return nil, structuralError(name, decErr, parseErr)
	}

	c := &collector{src: s}
	cfg := resolve(c, &raw)
	if err := c.err(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// structuralError converts a parser or decoder failure into an Errors value.
// goccy attaches the offending token to its error types, which is where the
// line number comes from.
func structuralError(name string, errs ...error) error {
	out := Errors{}
	for _, err := range errs {
		if err == nil {
			continue
		}
		e := Error{File: name, Msg: err.Error()}
		var yerr yaml.Error
		if errors.As(err, &yerr) {
			e.Msg = yerr.GetMessage()
			if tk := yerr.GetToken(); tk != nil {
				e.Line = tk.Position.Line
				e.Column = tk.Position.Column
			}
		}
		out = append(out, e)
		break // the decoder stops at the first structural problem anyway
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func resolve(c *collector, raw *fileConfig) *Config {
	cfg := &Config{
		Listen:    DefaultListen,
		Location:  time.Local,
		TZName:    time.Local.String(),
		StateFile: DefaultStateFile,
		Alerting:  Alerting{Mode: ModeTelegram, BatchWindow: DefaultBatchWindow, Reminders: DefaultReminders()},
	}
	if raw.Listen != nil {
		cfg.Listen = resolveListen(c, *raw.Listen)
	}
	if raw.Timezone != nil {
		name := strings.TrimSpace(*raw.Timezone)
		if name == "" {
			c.addf("timezone", "timezone is empty")
		} else if loc, err := loadTZ(name); err != nil {
			c.addf("timezone", "unknown timezone %q: %v", name, err)
		} else {
			cfg.Location = loc
			cfg.TZName = name
		}
	}
	if raw.RegistrationGroup != nil {
		cfg.RegistrationGroup = strings.TrimSpace(*raw.RegistrationGroup)
	}
	if raw.State != nil && raw.State.File != nil {
		if v, ok := expand(c, "state.file", *raw.State.File); ok {
			if strings.TrimSpace(v) == "" {
				c.addf("state.file", "state file path is empty")
			} else {
				cfg.StateFile = v
			}
		}
	}
	cfg.HistoryFile = defaultHistoryFile(cfg.StateFile)
	if raw.State != nil && raw.State.History != nil {
		if v, ok := expand(c, "state.history", *raw.State.History); ok {
			if strings.TrimSpace(v) == "" {
				c.addf("state.history", "history file path is empty")
			} else {
				cfg.HistoryFile = v
			}
		}
	}

	if raw.Alerting != nil {
		resolveAlerting(c, raw.Alerting, &cfg.Alerting)
	}

	def := defaultCheck()
	origin := defaultOrigin{}
	if raw.Defaults != nil {
		applyDefaults(c, "defaults", raw.Defaults, &def)
		origin.interval = raw.Defaults.Interval != nil
		origin.timeout = raw.Defaults.Timeout != nil
	}

	if len(raw.Checks) == 0 {
		c.addf("checks", "no checks defined: lookout would monitor nothing")
		return cfg
	}

	seen := make(map[string]int, len(raw.Checks))
	for i, rc := range raw.Checks {
		path := fmt.Sprintf("checks[%d]", i)
		chk := resolveCheck(c, path, rc, def, origin)
		if chk.Name != "" {
			if first, dup := seen[chk.Name]; dup {
				c.addf(path+".name", "duplicate check name %q, already used by checks[%d]", chk.Name, first)
			} else {
				seen[chk.Name] = i
			}
		}
		cfg.Checks = append(cfg.Checks, chk)
	}
	// Every site implies watching the name it lives on (registration.go).
	// Derived checks are appended last so an explicit one always wins.
	cfg.Checks = append(cfg.Checks, derivedRegistrations(cfg.Checks, def, cfg.RegistrationGroup)...)

	cfg.Mute = resolveMute(c, raw.Mute, seen)
	return cfg
}

func defaultHistoryFile(stateFile string) string {
	dir := filepath.Dir(stateFile)
	if dir == "." || dir == "" {
		return "history.jsonl"
	}
	return filepath.Join(dir, "history.jsonl")
}

// defaultOrigin records which scalars the defaults block actually set, so a
// dns/domain check can keep its type-specific interval when the operator
// never asked for a global one.
type defaultOrigin struct {
	interval bool
	timeout  bool
}

func resolveMute(c *collector, in []fileMute, checks map[string]int) []MuteWindow {
	if len(in) == 0 {
		return nil
	}
	out := make([]MuteWindow, 0, len(in))
	for i, raw := range in {
		path := fmt.Sprintf("mute[%d]", i)
		w, ok := resolveMuteWindow(c, path, raw, checks)
		if ok {
			out = append(out, w)
		}
	}
	return out
}

func resolveMuteWindow(c *collector, path string, raw fileMute, checks map[string]int) (MuteWindow, bool) {
	if raw.Cron != nil {
		c.addf(path+".cron", "cron expressions are not accepted: use every: [weekday] and at: HH:MM. A homelab window is a weekday and a clock; cron is how Gatus grew timezone bugs")
		return MuteWindow{}, false
	}
	w := MuteWindow{Location: time.UTC, TZName: "UTC"}
	ok := true
	if raw.At == nil {
		c.addf(path+".at", "at is required (24h clock, for example %q)", "02:00")
		ok = false
	} else if at, parsed := parseClock(c, path+".at", *raw.At); parsed {
		w.At = at
	} else {
		ok = false
	}
	if raw.Duration == nil {
		c.addf(path+".duration", "duration is required (how long after at: the window stays quiet)")
		ok = false
	} else if d, parsed := duration(c, path+".duration", *raw.Duration); parsed {
		w.Duration = d
	} else {
		ok = false
	}
	if !ok {
		return MuteWindow{}, false
	}

	if raw.Timezone != nil {
		name := strings.TrimSpace(*raw.Timezone)
		if name == "" {
			c.addf(path+".timezone", "timezone is empty")
			return MuteWindow{}, false
		}
		loc, err := loadTZ(name)
		if err != nil {
			c.addf(path+".timezone", "unknown timezone %q: %v", name, err)
			return MuteWindow{}, false
		}
		w.Location = loc
		w.TZName = name
	}

	if len(raw.Every) > 0 {
		seen := map[time.Weekday]bool{}
		for _, name := range raw.Every {
			day, err := parseWeekday(name)
			if err != nil {
				c.addf(path+".every", "%v", err)
				continue
			}
			if seen[day] {
				c.addf(path+".every", "duplicate weekday %q", name)
				continue
			}
			seen[day] = true
			w.Every = append(w.Every, day)
		}
		if len(w.Every) == 0 {
			return MuteWindow{}, false
		}
	}

	if raw.Group != nil {
		w.Group = strings.TrimSpace(*raw.Group)
	}
	if raw.Check != nil {
		w.Check = strings.TrimSpace(*raw.Check)
		if w.Check != "" {
			if _, ok := checks[w.Check]; !ok {
				c.addf(path+".check", "no check named %q", w.Check)
			}
		}
	}
	return w, true
}

func loadTZ(name string) (*time.Location, error) {
	if strings.EqualFold(name, "local") {
		return time.Local, nil
	}
	if strings.EqualFold(name, "utc") || name == "GMT" {
		return time.UTC, nil
	}
	return time.LoadLocation(name)
}

func parseClock(c *collector, path, raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	parts := strings.Split(raw, ":")
	if len(parts) != 2 && len(parts) != 3 {
		c.addf(path, "%q is not a 24h clock time (expected HH:MM)", raw)
		return 0, false
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		if p == "" || !clockDigits(p) {
			c.addf(path, "%q is not a 24h clock time (expected HH:MM)", raw)
			return 0, false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			c.addf(path, "%q is not a 24h clock time (expected HH:MM)", raw)
			return 0, false
		}
		nums[i] = n
	}
	h, m := nums[0], nums[1]
	s := 0
	if len(nums) == 3 {
		s = nums[2]
	}
	if h < 0 || h > 23 || m < 0 || m > 59 || s < 0 || s > 59 {
		c.addf(path, "%q is not a 24h clock time (expected HH:MM)", raw)
		return 0, false
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second, true
}

func clockDigits(s string) bool {
	if len(s) == 0 || len(s) > 2 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func parseWeekday(raw string) (time.Weekday, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "sunday", "sun", "0":
		return time.Sunday, nil
	case "monday", "mon", "1":
		return time.Monday, nil
	case "tuesday", "tue", "tues", "2":
		return time.Tuesday, nil
	case "wednesday", "wed", "3":
		return time.Wednesday, nil
	case "thursday", "thu", "thur", "thurs", "4":
		return time.Thursday, nil
	case "friday", "fri", "5":
		return time.Friday, nil
	case "saturday", "sat", "6":
		return time.Saturday, nil
	}
	return 0, fmt.Errorf("unknown weekday %q (use Monday … Sunday)", raw)
}

func resolveAlerting(c *collector, in *fileAlerting, into *Alerting) {
	if in.Mode != nil {
		switch Mode(*in.Mode) {
		case ModeTelegram, ModeNone:
			into.Mode = Mode(*in.Mode)
		default:
			c.addf("alerting.mode", "unknown alerting mode %q: use %q or %q", *in.Mode, ModeTelegram, ModeNone)
		}
	}
	if in.BatchWindow != nil {
		if v, ok := duration(c, "alerting.batch_window", *in.BatchWindow); ok {
			into.BatchWindow = v
		}
	}
	if in.Reminders != nil {
		into.Reminders = resolveReminders(c, *in.Reminders)
	}
	if in.Telegram == nil {
		return
	}
	if into.Mode == ModeNone {
		// Half-configured alerting is worse than either extreme: the operator
		// believes a channel exists and it does not.
		c.addf("alerting.telegram", "alerting.mode is %q, so a telegram section cannot take effect", ModeNone)
		return
	}
	tg := in.Telegram
	if tg.Token != nil {
		c.addf("alerting.telegram.token", "telegram bot token must come from the %s environment variable, not the config file", EnvTelegramToken)
	}
	if tg.BotToken != nil {
		c.addf("alerting.telegram.bot_token", "telegram bot token must come from the %s environment variable, not the config file", EnvTelegramToken)
	}
	if tg.ChatID != nil {
		c.addf("alerting.telegram.chat_id", "telegram chat id must come from the %s environment variable, not the config file", EnvTelegramChatID)
	}
	if tg.Proxy != nil {
		into.Telegram.Proxy = resolveProxy(c, "alerting.telegram.proxy", *tg.Proxy)
	}
}

// resolveReminders compiles alerting.reminders. An explicitly empty list is
// the way to say "state changes only" — silence has to be asked for.
func resolveReminders(c *collector, raw []string) []time.Duration {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > MaxReminders {
		c.addf("alerting.reminders", "%d reminder steps is more than the %d allowed", len(raw), MaxReminders)
		return DefaultReminders()
	}
	out := make([]time.Duration, 0, len(raw))
	for i, item := range raw {
		field := fmt.Sprintf("alerting.reminders[%d]", i)
		v, ok := duration(c, field, item)
		if !ok {
			continue
		}
		if v < time.Minute {
			c.addf(field, "a reminder gap of %s would page on every probe: use a minute or more", v)
			continue
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return DefaultReminders()
	}
	return out
}

func resolveListen(c *collector, raw string) string {
	expanded, ok := expand(c, "listen", raw)
	if !ok {
		return DefaultListen
	}
	expanded = strings.TrimSpace(expanded)
	if expanded == "" {
		c.addf("listen", "listen address is empty")
		return DefaultListen
	}
	host, port, err := net.SplitHostPort(expanded)
	if err != nil {
		c.addf("listen", "listen address %q is not host:port (for example %q)", expanded, DefaultListen)
		return DefaultListen
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		c.addf("listen", "listen port %q is not a valid TCP port", port)
		return DefaultListen
	}
	return net.JoinHostPort(host, port)
}

func resolveProxy(c *collector, path, raw string) string {
	expanded, ok := expand(c, path, raw)
	if !ok {
		return ""
	}
	expanded = strings.TrimSpace(expanded)
	if expanded == "" {
		return ""
	}
	u, err := url.Parse(expanded)
	if err != nil {
		c.addf(path, "proxy URL is not valid: %v", err)
		return ""
	}
	switch u.Scheme {
	case "socks5", "socks5h":
	case "":
		c.addf(path, "proxy URL %q has no scheme, expected %q", expanded, "socks5://")
		return ""
	default:
		c.addf(path, "proxy scheme %q is not supported, expected %q (Telegram is reached through SOCKS5)", u.Scheme, "socks5")
		return ""
	}
	if u.Host == "" {
		c.addf(path, "proxy URL %q has no host", expanded)
		return ""
	}
	return expanded
}

func defaultCheck() Check {
	return Check{
		Type:             TypeHTTP,
		Method:           DefaultMethod,
		Interval:         DefaultInterval,
		Timeout:          DefaultTimeout,
		Alert:            DefaultAlert,
		FailureThreshold: DefaultFailureThreshold,
		SuccessThreshold: DefaultSuccessThreshold,
		Instability: Instability{
			Failures: DefaultInstabilityFails,
			Window:   DefaultInstabilityWin,
			Cooldown: DefaultInstabilityCool,
		},
	}
}

func applyDefaults(c *collector, path string, d *fileDefaults, into *Check) {
	if d.Interval != nil {
		if v, ok := duration(c, path+".interval", *d.Interval); ok {
			into.Interval = v
		}
	}
	if d.Timeout != nil {
		if v, ok := duration(c, path+".timeout", *d.Timeout); ok {
			into.Timeout = v
		}
	}
	if d.Method != nil {
		if v, ok := method(c, path+".method", *d.Method); ok {
			into.Method = v
		}
	}
	if d.Alert != nil {
		into.Alert = *d.Alert
	}
	if d.FailureThreshold != nil {
		if v, ok := positive(c, path+".failure_threshold", *d.FailureThreshold); ok {
			into.FailureThreshold = v
		}
	}
	if d.SuccessThreshold != nil {
		if v, ok := positive(c, path+".success_threshold", *d.SuccessThreshold); ok {
			into.SuccessThreshold = v
		}
	}
	if d.Instability != nil {
		applyInstability(c, path+".instability", d.Instability, &into.Instability)
	}
}

func applyInstability(c *collector, path string, in *fileInstability, into *Instability) {
	if in.Window != nil {
		// The window is a uint64 bitmask of recent results, so 64 is a hard cap.
		if *in.Window < 1 || *in.Window > 64 {
			c.addf(path+".window", "window must be between 1 and 64, got %d", *in.Window)
		} else {
			into.Window = *in.Window
		}
	}
	if in.Failures != nil {
		if v, ok := positive(c, path+".failures", *in.Failures); ok {
			into.Failures = v
		}
	}
	if in.Cooldown != nil {
		if v, ok := duration(c, path+".cooldown", *in.Cooldown); ok {
			into.Cooldown = v
		}
	}
	if into.Failures > into.Window {
		c.addf(path+".failures", "failures (%d) cannot exceed window (%d): the condition would never be met", into.Failures, into.Window)
	}
}

func resolveCheck(c *collector, path string, rc fileCheck, chk Check, origin defaultOrigin) Check {
	chk.Name = strings.TrimSpace(rc.Name)
	if chk.Name == "" {
		c.addf(path+".name", "name is required: it identifies the check in state, alerts and the API")
	} else if strings.ContainsAny(rc.Name, "\n\r") {
		c.addf(path+".name", "name must be a single line")
	}
	chk.Group = strings.TrimSpace(rc.Group)

	if rc.Registration != nil {
		v := strings.ToLower(strings.TrimSpace(*rc.Registration))
		switch {
		case v == "" || v == "auto":
			// The default: work the name out from the host.
		case v == RegistrationOff || v == "false" || v == "no":
			chk.Registration = RegistrationOff
		case rc.Type == string(TypeDomain):
			c.addf(path+".registration", "a domain check already is the registration of a name")
		case registrable(v) == "":
			c.addf(path+".registration", "%q is not a name a registry would know: use a registrable domain, or %q", v, RegistrationOff)
		default:
			chk.Registration = registrable(v)
		}
	}

	switch rc.Type {
	case "":
		c.addf(path+".type", "type is required, one of %q, %q, %q", TypeHTTP, TypeDNS, TypeDomain)
	case string(TypeHTTP):
		chk.Type = TypeHTTP
	case string(TypeDNS):
		chk.Type = TypeDNS
	case string(TypeDomain):
		chk.Type = TypeDomain
	default:
		c.addf(path+".type", "unknown check type %q, expected one of %q, %q, %q", rc.Type, TypeHTTP, TypeDNS, TypeDomain)
	}

	applyTypeDefaults(&chk, origin, rc)

	if rc.Interval != nil {
		if v, ok := duration(c, path+".interval", *rc.Interval); ok {
			chk.Interval = v
		}
	}
	if rc.Timeout != nil {
		if v, ok := duration(c, path+".timeout", *rc.Timeout); ok {
			chk.Timeout = v
		}
	}
	if chk.Type == TypeDomain && chk.Interval < DomainMinInterval {
		where := path + ".interval"
		if rc.Interval == nil {
			where = path
		}
		c.addf(where, "domain checks must run at most once per hour (got interval %s): registries are not liveness endpoints", chk.Interval)
	}
	// Probes must not overlap, or consecutive results stop being independent
	// samples and every rate in the system is computed over a wrong denominator.
	if chk.Timeout >= chk.Interval {
		where := path + ".timeout"
		if rc.Timeout == nil {
			where = path
		}
		c.addf(where, "timeout (%s) must be shorter than interval (%s), otherwise probes overlap", chk.Timeout, chk.Interval)
	}

	if rc.Method != nil {
		if chk.Type != TypeHTTP {
			c.addf(path+".method", "method is only valid on %q checks", TypeHTTP)
		} else if v, ok := method(c, path+".method", *rc.Method); ok {
			chk.Method = v
		}
	}
	if rc.Alert != nil {
		chk.Alert = *rc.Alert
	}
	if rc.FailureThreshold != nil {
		if v, ok := positive(c, path+".failure_threshold", *rc.FailureThreshold); ok {
			chk.FailureThreshold = v
		}
	}
	if rc.SuccessThreshold != nil {
		if v, ok := positive(c, path+".success_threshold", *rc.SuccessThreshold); ok {
			chk.SuccessThreshold = v
		}
	}
	if rc.Instability != nil {
		applyInstability(c, path+".instability", rc.Instability, &chk.Instability)
	}

	switch chk.Type {
	case TypeHTTP:
		if strings.TrimSpace(rc.Host) != "" {
			c.addf(path+".host", "host is for %q and %q checks; %q checks use url", TypeDNS, TypeDomain, TypeHTTP)
		}
		if strings.TrimSpace(rc.Domain) != "" {
			c.addf(path+".domain", "domain is for %q checks; %q checks use url", TypeDomain, TypeHTTP)
		}
		if rc.QueryType != "" {
			c.addf(path+".query_type", "query_type is only valid on %q checks", TypeDNS)
		}
		if rc.Resolver != nil {
			c.addf(path+".resolver", "resolver is only valid on %q checks", TypeDNS)
		}
		chk.URL = resolveURL(c, path, rc)
		chk.Headers = resolveHeaders(c, path, rc.Headers)
	case TypeDNS:
		rejectHTTPFields(c, path, rc)
		chk.Host = resolveHost(c, path, rc, true)
		chk.QueryType = resolveQueryType(c, path, rc)
		if rc.Resolver != nil {
			chk.Resolver = resolveResolver(c, path+".resolver", *rc.Resolver)
		}
	case TypeDomain:
		rejectHTTPFields(c, path, rc)
		if rc.QueryType != "" {
			c.addf(path+".query_type", "query_type is only valid on %q checks", TypeDNS)
		}
		if rc.Resolver != nil {
			c.addf(path+".resolver", "resolver is only valid on %q checks", TypeDNS)
		}
		chk.Host = resolveHost(c, path, rc, false)
	}

	chk.Expect = resolveExpect(c, path, rc.Expect, chk.Timeout, chk.Type)
	return chk
}

func applyTypeDefaults(chk *Check, origin defaultOrigin, rc fileCheck) {
	if rc.Interval != nil {
		return
	}
	if origin.interval {
		return
	}
	switch chk.Type {
	case TypeDNS:
		chk.Interval = DefaultDNSInterval
	case TypeDomain:
		chk.Interval = DefaultDomainInterval
		if rc.Timeout == nil && !origin.timeout {
			chk.Timeout = DefaultDomainTimeout
		}
	}
}

func rejectHTTPFields(c *collector, path string, rc fileCheck) {
	if strings.TrimSpace(rc.URL) != "" {
		c.addf(path+".url", "url is for %q checks; use host or domain", TypeHTTP)
	}
	if rc.Method != nil {
		// already reported in resolveCheck
	}
	if len(rc.Headers) > 0 {
		c.addf(path+".headers", "headers are only valid on %q checks", TypeHTTP)
	}
}

func resolveQueryType(c *collector, path string, rc fileCheck) QueryType {
	raw := strings.ToUpper(strings.TrimSpace(rc.QueryType))
	if raw == "" {
		c.addf(path+".query_type", "query_type is required for %q checks, one of %s", TypeDNS, joinQueryTypes())
		return ""
	}
	for _, known := range QueryTypes {
		if raw == string(known) {
			return known
		}
	}
	c.addf(path+".query_type", "unknown query_type %q, expected one of %s", rc.QueryType, joinQueryTypes())
	return ""
}

func joinQueryTypes() string {
	parts := make([]string, len(QueryTypes))
	for i, q := range QueryTypes {
		parts[i] = string(q)
	}
	return strings.Join(parts, ", ")
}

func resolveHost(c *collector, path string, rc fileCheck, dns bool) string {
	host := strings.TrimSpace(rc.Host)
	domain := strings.TrimSpace(rc.Domain)
	field := path + ".host"
	raw := host
	switch {
	case dns:
		if host == "" && domain != "" {
			raw = domain
			field = path + ".domain"
		}
		if raw == "" {
			c.addf(path+".host", "host is required for %q checks (the name to query)", TypeDNS)
			return ""
		}
		if host != "" && domain != "" && host != domain {
			c.addf(path+".domain", "host and domain both set and differ (%q vs %q); use one", host, domain)
		}
	default:
		if domain != "" {
			raw = domain
			field = path + ".domain"
		} else if host != "" {
			raw = host
		}
		if raw == "" {
			c.addf(path+".domain", "domain is required for %q checks (the registered name to watch)", TypeDomain)
			return ""
		}
		if host != "" && domain != "" && host != domain {
			c.addf(path+".host", "host and domain both set and differ (%q vs %q); use one", host, domain)
		}
	}
	expanded, ok := expand(c, field, raw)
	if !ok {
		return ""
	}
	expanded = strings.TrimSpace(expanded)
	ascii, err := canonicalName(expanded)
	if err != nil {
		c.addf(field, "%v", err)
		return ""
	}
	return ascii
}

func canonicalName(raw string) (string, error) {
	s := strings.TrimSuffix(strings.TrimSpace(raw), ".")
	if s == "" {
		return "", fmt.Errorf("name is empty")
	}
	if strings.Contains(s, "://") {
		return "", fmt.Errorf("name %q looks like a URL; use a hostname", raw)
	}
	if strings.ContainsAny(s, " \t\r\n/") {
		return "", fmt.Errorf("name %q is not a hostname", raw)
	}
	for _, r := range s {
		if r > 127 {
			return "", fmt.Errorf("name %q contains non-ASCII; write the punycode form (xn--…)", raw)
		}
	}
	ascii := strings.ToLower(s)
	if !strings.Contains(ascii, ".") {
		return "", fmt.Errorf("name %q must be a FQDN (need at least two labels)", raw)
	}
	for _, label := range strings.Split(ascii, ".") {
		if label == "" {
			return "", fmt.Errorf("name %q contains an empty label", raw)
		}
	}
	return ascii, nil
}

func resolveResolver(c *collector, path, raw string) string {
	expanded, ok := expand(c, path, raw)
	if !ok {
		return ""
	}
	expanded = strings.TrimSpace(expanded)
	if expanded == "" {
		return ""
	}
	host, port, err := splitHostPortDefault(expanded, "53")
	if err != nil {
		c.addf(path, "resolver %q is not a host or host:port: %v", expanded, err)
		return ""
	}
	if host == "" {
		c.addf(path, "resolver %q has no host", expanded)
		return ""
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		c.addf(path, "resolver port %q is not a valid UDP port", port)
		return ""
	}
	return net.JoinHostPort(host, port)
}

func splitHostPortDefault(addr, defaultPort string) (host, port string, err error) {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return net.SplitHostPort(addr)
	}
	// net.SplitHostPort rejects a bare IPv6 address. Try wrapping it.
	if strings.Count(addr, ":") >= 2 && !strings.HasPrefix(addr, "[") {
		if h, p, err := net.SplitHostPort("[" + addr + "]:" + defaultPort); err == nil {
			return h, p, nil
		}
	}
	return addr, defaultPort, nil
}

func resolveURL(c *collector, path string, rc fileCheck) string {
	raw := strings.TrimSpace(rc.URL)
	if raw == "" {
		if rc.Type == string(TypeHTTP) {
			c.addf(path+".url", "url is required for %q checks", TypeHTTP)
		}
		return ""
	}
	expanded, ok := expand(c, path+".url", raw)
	if !ok {
		return ""
	}
	u, err := url.Parse(expanded)
	if err != nil {
		c.addf(path+".url", "url is not valid: %v", err)
		return ""
	}
	switch u.Scheme {
	case "http", "https":
	case "":
		c.addf(path+".url", "url %q has no scheme, expected %q or %q", expanded, "http://", "https://")
		return ""
	default:
		c.addf(path+".url", "url scheme %q is not supported, expected %q or %q", u.Scheme, "http", "https")
		return ""
	}
	if u.Host == "" {
		c.addf(path+".url", "url %q has no host", expanded)
		return ""
	}
	return expanded
}

var headerNameRe = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]+$`)

func resolveHeaders(c *collector, path string, in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for name, value := range in {
		hpath := path + ".headers"
		if !headerNameRe.MatchString(name) {
			c.addKeyf(hpath, name, "%q is not a valid HTTP header name", name)
			continue
		}
		v, ok := expandAt(c, hpath, name, value)
		if !ok {
			continue
		}
		if strings.ContainsAny(v, "\r\n") {
			c.addKeyf(hpath, name, "header %q value must not contain a line break", name)
			continue
		}
		out[http.CanonicalHeaderKey(name)] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func resolveExpect(c *collector, path string, in *fileExpect, timeout time.Duration, typ Type) Expect {
	var exp Expect
	epath := path + ".expect"

	httpOnly := typ == TypeHTTP || typ == ""
	dnsOnly := typ == TypeDNS

	if httpOnly {
		if in == nil || in.Status == nil {
			// Documented default: without it a check would report "up" on a 500.
			m, err := ParseStatus(DefaultStatus)
			if err != nil {
				panic("config: built-in default status does not parse: " + err.Error())
			}
			exp.Status = m
		} else if m, err := ParseStatus(in.Status); err != nil {
			c.addf(epath+".status", "%v", err)
		} else {
			exp.Status = m
		}
	} else if in != nil && in.Status != nil {
		c.addf(epath+".status", "status is only valid on %q checks", TypeHTTP)
	}

	if in == nil {
		if dnsOnly {
			exp.Rcode = DefaultRcode
		}
		return exp
	}

	if in.BodyContains != nil {
		if !httpOnly {
			c.addf(epath+".body_contains", "body_contains is only valid on %q checks", TypeHTTP)
		} else if *in.BodyContains == "" {
			c.addf(epath+".body_contains", "body_contains is empty: it would match every response")
		} else {
			exp.BodyContains = *in.BodyContains
		}
	}

	if in.ResponseTime != nil {
		m, err := ParseDurationMatcher(*in.ResponseTime)
		if err != nil {
			c.addf(epath+".response_time", "response_time %v", err)
		} else {
			// An upper bound at or above the timeout can never fail: the probe
			// aborts first, so the condition is decoration.
			if (m.op == "<" || m.op == "<=") && m.d >= timeout {
				c.addf(epath+".response_time", "bound %s is not below the timeout (%s), so this condition can never fail", m.d, timeout)
			} else {
				exp.ResponseTime = m
			}
		}
	}

	if in.Rcode != nil {
		if !dnsOnly {
			c.addf(epath+".rcode", "rcode is only valid on %q checks", TypeDNS)
		} else if code, err := ParseRcode(*in.Rcode); err != nil {
			c.addf(epath+".rcode", "%v", err)
		} else {
			exp.Rcode = code
		}
	} else if dnsOnly {
		exp.Rcode = DefaultRcode
	}

	if in.AnswersContain != nil {
		if !dnsOnly {
			c.addf(epath+".answers_contain", "answers_contain is only valid on %q checks", TypeDNS)
		} else if strings.TrimSpace(*in.AnswersContain) == "" {
			c.addf(epath+".answers_contain", "answers_contain is empty: it would match every response")
		} else {
			exp.AnswersContain = *in.AnswersContain
		}
	}

	if len(in.Body) > 0 && !httpOnly {
		c.addf(epath+".body", "body is only valid on %q checks", TypeHTTP)
		return exp
	}

	seen := make(map[string]bool, len(in.Body))
	for _, item := range in.Body {
		key, ok := item.Key.(string)
		if !ok {
			c.addf(epath+".body", "body keys must be paths like %q, got %v", ".result.online", item.Key)
			continue
		}
		p, err := bodypath.Parse(key)
		if err != nil {
			c.addKeyf(epath+".body", key, "%v", err)
			continue
		}
		if seen[key] {
			c.addKeyf(epath+".body", key, "duplicate body path %q", key)
			continue
		}
		seen[key] = true
		want, ok := scalar(item.Value)
		if !ok {
			c.addKeyf(epath+".body", key, "expected value must be a string, number, boolean or null, got %T", item.Value)
			continue
		}
		exp.Body = append(exp.Body, BodyExpect{Path: p, Want: want})
	}
	return exp
}

// scalar normalises a decoded YAML scalar to the shapes a JSON body can hold.
func scalar(v any) (any, bool) {
	switch t := v.(type) {
	case nil:
		return nil, true
	case bool, string:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint64:
		return float64(t), true
	case float64:
		return t, true
	default:
		return nil, false
	}
}

func duration(c *collector, path, raw string) (time.Duration, bool) {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		c.addf(path, "%q is not a duration (expected forms like %q, %q, %q)", raw, "30s", "5m", "1h30m")
		return 0, false
	}
	if d <= 0 {
		c.addf(path, "%q must be positive", raw)
		return 0, false
	}
	return d, true
}

func positive(c *collector, path string, n int) (int, bool) {
	if n < 1 {
		c.addf(path, "must be at least 1, got %d", n)
		return 0, false
	}
	return n, true
}

var methods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

func method(c *collector, path, raw string) (string, bool) {
	m := strings.ToUpper(strings.TrimSpace(raw))
	for _, known := range methods {
		if m == known {
			return m, true
		}
	}
	c.addf(path, "unknown HTTP method %q, expected one of %s", raw, strings.Join(methods, ", "))
	return "", false
}

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expand substitutes ${VAR} from the environment. Secrets live in the
// environment, never in the config file (SPEC §3, §11), so an unset variable is
// a hard error: silently sending an empty Authorization header would turn a
// deployment mistake into a permanently failing check.
func expand(c *collector, path, s string) (string, bool) {
	return expandAt(c, path, "", s)
}

func expandAt(c *collector, path, key, s string) (string, bool) {
	ok := true
	out := envRe.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		v, found := os.LookupEnv(name)
		if !found {
			ok = false
			if key != "" {
				c.addKeyf(path, key, "environment variable %s is not set", name)
			} else {
				c.addf(path, "environment variable %s is not set", name)
			}
			return ""
		}
		return v
	})
	return out, ok
}
