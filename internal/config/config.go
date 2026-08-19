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

// Config is a validated configuration.
type Config struct {
	StateFile string
	Checks    []Check
}

// Check is one resolved check (SPEC §4).
type Check struct {
	Name    string
	Group   string
	Type    Type
	URL     string
	Method  string
	Headers map[string]string

	Interval time.Duration
	Timeout  time.Duration

	Expect Expect

	Alert            bool
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
}

// BodyExpect is one "path in the body must equal this value" condition.
type BodyExpect struct {
	Path bodypath.Path
	Want any // bool, string, float64 or nil, as produced by the YAML decoder
}
