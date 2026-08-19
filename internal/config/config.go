// Package config loads, validates and resolves the lookout configuration.
//
// The types in this file are the *resolved* model: every optional field has
// already been filled in from the defaults block, and every mini-language
// (status matchers, durations, body paths) has already been compiled. Nothing
// downstream re-parses a string at probe time — a configuration that loads is a
// configuration that runs.
package config

import (
	"time"

	"github.com/eeegoloauq/lookout/internal/bodypath"
)

// Built-in defaults. A configuration that sets nothing still behaves sanely,
// and alerting is on unless a check opts out (SPEC §1.1).
const (
	DefaultInterval         = 60 * time.Second
	DefaultTimeout          = 5 * time.Second
	DefaultMethod           = "GET"
	DefaultFailureThreshold = 3
	DefaultSuccessThreshold = 2
	DefaultInstabilityFails = 5
	DefaultInstabilityWin   = 20
	DefaultInstabilityCool  = time.Hour
	DefaultAlert            = true
	DefaultStateFile        = "state.json"
	DefaultBatchWindow      = 45 * time.Second
	// 5665 rather than 8080: 8080 is every tool's default and collides on
	// any busy host, the 9090/9115 pair belongs to Prometheus and its
	// blackbox exporter by convention, and anything above 32768 can clash
	// with the kernel's ephemeral source ports (32768-60999 on Linux), so
	// the service occasionally fails to bind after a reboot. 5665 is
	// unassigned by IANA and sits next to Nagios NRPE's 5666 — the
	// monitoring neighbourhood, without taking anyone's port.
	//
	// DefaultListen is loopback only. The status page and API are for the
	// machine that runs lookout, not the network; publishing them is a
	// deliberate listen: override (SPEC §11, and the task's stricter
	// reading of "LAN" as the loopback default).
	DefaultListen = "127.0.0.1:5665"

	// MaxReminders bounds the still-down schedule. A schedule longer than
	// this is a misconfiguration, not a plan.
	MaxReminders = 8

	// MaxAdhocMute is the longest lookout mute --for will accept. A
	// forgotten mute is how a real outage stays silent; seven days is
	// already a vacation, not a maintenance window.
	MaxAdhocMute = 7 * 24 * time.Hour

	// Type-specific intervals (SPEC §5). HTTP keeps DefaultInterval;
	// DNS and domain checks are cheaper to run slowly than to hammer
	// a resolver or a registry.
	DefaultDNSInterval    = 5 * time.Minute
	DefaultDomainInterval = 24 * time.Hour
	DefaultDomainTimeout  = 15 * time.Second
	// DomainMinInterval is how often a registry may be asked. Daily
	// is the intended cadence; anything under an hour is a misconfig
	// that would look like a scraper.
	DomainMinInterval = time.Hour

	// Telegram credentials live in the environment, never in the config file
	// (SPEC §6, §11). Empty values are treated as missing.
	EnvTelegramToken  = "LOOKOUT_TELEGRAM_TOKEN"
	EnvTelegramChatID = "LOOKOUT_TELEGRAM_CHAT_ID"

	// DefaultStatus is applied when a check declares no status expectation. A
	// check that asserts nothing would report "up" for a 500, which is the
	// silent-failure mode SPEC §1.1 exists to prevent.
	DefaultStatus = "200-299"
)

// Type is the kind of probe a check performs.
type Type string

const (
	TypeHTTP   Type = "http"
	TypeDNS    Type = "dns"
	TypeDomain Type = "domain"
)

// QueryType is a DNS resource record type a dns check will ask for.
type QueryType string

const (
	QueryA    QueryType = "A"
	QueryAAAA QueryType = "AAAA"
	QueryMX   QueryType = "MX"
	QueryNS   QueryType = "NS"
	QueryTXT  QueryType = "TXT"
)

// QueryTypes is the set a dns check may name.
var QueryTypes = []QueryType{QueryA, QueryAAAA, QueryMX, QueryNS, QueryTXT}

// Config is a validated configuration.
type Config struct {
	Listen string
	// Location is the timezone the status page renders clocks in. Logs and
	// the JSON API stay UTC: a machine-readable timestamp with a local
	// offset is how two systems end up disagreeing about an outage.
	Location *time.Location
	// TZName is Location as written in the config, for display.
	TZName      string
	StateFile   string
	HistoryFile string
	Alerting    Alerting
	Mute        []MuteWindow
	Checks      []Check
}

// MuteWindow is a recurring quiet period (SPEC §8). Probes keep running;
// only delivery is suppressed. The spec names a cron expression; we take
// a weekday list and a clock instead — see the series notes for why.
type MuteWindow struct {
	Every    []time.Weekday // empty means every day
	At       time.Duration  // from local midnight
	Duration time.Duration
	Location *time.Location
	TZName   string
	Group    string
	Check    string
}

// Alerting is the notification pipeline (SPEC §6, §7). Token and chat id are
// deliberately not here: they come from the environment so a copied config
// cannot leak them.
type Alerting struct {
	// Mode selects the transport. "none" is the only way to run without
	// notifications: silence has to be asked for, never inherited from a
	// missing section (SPEC §1.1). It exists so that a monitor whose
	// credentials went missing is a configuration error, while a monitor
	// that is deliberately page-only still starts.
	Mode        Mode
	BatchWindow time.Duration
	// Reminders is the escalating gap between notices about one open
	// incident: the first value is measured from the DOWN alert, each
	// following one from the previous reminder, and the last value repeats
	// forever. Empty means only state changes ever notify.
	Reminders []time.Duration
	Telegram  Telegram
}

// DefaultReminders repeats an open outage after an hour, then four hours
// later, then daily. An outage that starts at 03:00 is worth one more line
// in the morning; it is not worth one every minute.
func DefaultReminders() []time.Duration {
	return []time.Duration{time.Hour, 4 * time.Hour, 24 * time.Hour}
}

// Mode is the alerting transport.
type Mode string

const (
	// ModeTelegram delivers alerts to Telegram. It is the default.
	ModeTelegram Mode = "telegram"
	// ModeNone delivers nothing: state, page and metrics only.
	ModeNone Mode = "none"
)

// Telegram is the Bot API transport. Proxy is a SOCKS5 URL; empty means
// dial api.telegram.org directly.
type Telegram struct {
	Proxy string
}

// Check is one resolved check (SPEC §4).
type Check struct {
	Name    string
	Group   string
	Type    Type
	URL     string
	Method  string
	Headers map[string]string

	// Host is the DNS name a dns check queries, or the registered
	// name a domain check watches. Empty on http checks.
	Host string
	// QueryType is set on dns checks only.
	QueryType QueryType
	// Resolver is host:port of the nameserver a dns check talks to.
	// Empty means "look the system resolver up at probe time".
	Resolver string

	Interval time.Duration
	Timeout  time.Duration

	Expect Expect

	Alert            bool
	// Registration names the domain whose expiry this check implies, or
	// RegistrationOff to imply none. Empty means "work it out from the
	// host", which is what almost every check wants.
	Registration string
	// Implicit marks a check lookout synthesised rather than one the
	// operator wrote: the registration behind a site. It probes, alerts and
	// is scraped like any other, but it is not a row on the board — the
	// site it belongs to shows its expiry instead.
	Implicit         bool
	FailureThreshold int
	SuccessThreshold int
	Instability      Instability
}

// Instability configures the "N of the last M" detector (SPEC §6). A single
// success resets a consecutive-failure counter, so alternating up/down never
// reaches FailureThreshold no matter how bad availability is; this window is
// what catches that pattern.
type Instability struct {
	Failures int           // N
	Window   int           // M
	Cooldown time.Duration // minimum gap between two instability notices
}

// Expect holds the conditions a response must satisfy. All of them must hold.
type Expect struct {
	Status       StatusMatcher
	Body         []BodyExpect
	BodyContains string
	ResponseTime DurationMatcher

	// Rcode is the expected DNS response code (NOERROR, NXDOMAIN, …).
	// Empty on non-dns checks; dns checks default to NOERROR.
	Rcode string
	// AnswersContain is a case-insensitive substring that must appear
	// in the rendered DNS answers.
	AnswersContain string
}

// BodyExpect is one "path in the body must equal this value" condition.
type BodyExpect struct {
	Path bodypath.Path
	Want any // bool, string, float64 or nil, as produced by the YAML decoder
}
