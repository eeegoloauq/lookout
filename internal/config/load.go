package config

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
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
	State    *fileState    `yaml:"state"`
	Defaults *fileDefaults `yaml:"defaults"`
	Checks   []fileCheck   `yaml:"checks"`
}

type fileState struct {
	File *string `yaml:"file"`
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
	Group            string            `yaml:"group"`
	Type             string            `yaml:"type"`
	URL              string            `yaml:"url"`
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
	Status       any           `yaml:"status"`
	Body         yaml.MapSlice `yaml:"body"`
	BodyContains *string       `yaml:"body_contains"`
	ResponseTime *string       `yaml:"response_time"`
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
	cfg := &Config{StateFile: DefaultStateFile}
	if raw.State != nil && raw.State.File != nil {
		if v, ok := expand(c, "state.file", *raw.State.File); ok {
			if strings.TrimSpace(v) == "" {
				c.addf("state.file", "state file path is empty")
			} else {
				cfg.StateFile = v
			}
		}
	}

	def := defaultCheck()
	if raw.Defaults != nil {
		applyDefaults(c, "defaults", raw.Defaults, &def)
	}

	if len(raw.Checks) == 0 {
		c.addf("checks", "no checks defined: lookout would monitor nothing")
		return cfg
	}

	seen := make(map[string]int, len(raw.Checks))
	for i, rc := range raw.Checks {
		path := fmt.Sprintf("checks[%d]", i)
		chk := resolveCheck(c, path, rc, def)
		if chk.Name != "" {
			if first, dup := seen[chk.Name]; dup {
				c.addf(path+".name", "duplicate check name %q, already used by checks[%d]", chk.Name, first)
			} else {
				seen[chk.Name] = i
			}
		}
		cfg.Checks = append(cfg.Checks, chk)
	}
	return cfg
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

func resolveCheck(c *collector, path string, rc fileCheck, chk Check) Check {
	chk.Name = strings.TrimSpace(rc.Name)
	if chk.Name == "" {
		c.addf(path+".name", "name is required: it identifies the check in state, alerts and the API")
	} else if strings.ContainsAny(rc.Name, "\n\r") {
		c.addf(path+".name", "name must be a single line")
	}
	chk.Group = strings.TrimSpace(rc.Group)

	switch rc.Type {
	case "":
		c.addf(path+".type", "type is required, one of %q, %q, %q", TypeHTTP, TypeDNS, TypeDomain)
	case string(TypeHTTP):
		chk.Type = TypeHTTP
	case string(TypeDNS), string(TypeDomain):
		c.addf(path+".type", "check type %q is not implemented yet; this release probes %q checks only", rc.Type, TypeHTTP)
	default:
		c.addf(path+".type", "unknown check type %q, expected one of %q, %q, %q", rc.Type, TypeHTTP, TypeDNS, TypeDomain)
	}

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
		if v, ok := method(c, path+".method", *rc.Method); ok {
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

	chk.URL = resolveURL(c, path, rc)
	chk.Headers = resolveHeaders(c, path, rc.Headers)
	chk.Expect = resolveExpect(c, path, rc.Expect, chk.Timeout)
	return chk
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

func resolveExpect(c *collector, path string, in *fileExpect, timeout time.Duration) Expect {
	var exp Expect
	epath := path + ".expect"
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
	if in == nil {
		return exp
	}

	if in.BodyContains != nil {
		if *in.BodyContains == "" {
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
