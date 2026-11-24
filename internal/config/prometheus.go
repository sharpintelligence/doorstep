package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	DefaultPrometheusInterval         = time.Minute
	MinPrometheusInterval             = 10 * time.Second
	MaxPrometheusInterval             = time.Hour
	MaxPrometheusInitialDelay         = 24 * time.Hour
	DefaultPrometheusSuccessThreshold = 5
	DefaultPrometheusFailureThreshold = 3
	MaxPrometheusSuccessThreshold     = 1000
	MaxPrometheusFailureThreshold     = 100
	MaxPrometheusQueryBytes           = 4096
	DefaultBaselineWindow             = 30 * time.Minute
	MaxBaselinePoints                 = 1000

	prometheusServerList = "prometheus"
)

type PrometheusServer struct {
	Name            string `yaml:"name"`
	URL             string `yaml:"url"`
	BearerTokenEnv  string `yaml:"bearer_token_env"`
	BearerTokenFile string `yaml:"bearer_token_file"`
	Username        string `yaml:"username"`
	PasswordEnv     string `yaml:"password_env"`
	PasswordFile    string `yaml:"password_file"`
	AllowHTTPAuth   bool   `yaml:"allow_http_auth"`
}

func (s PrometheusServer) BearerToken() (SecretRef, bool) {
	ref := SecretRef{Label: s.label("bearer_token"), Env: s.BearerTokenEnv, File: s.BearerTokenFile}
	return ref, ref.Env != "" || ref.File != ""
}

func (s PrometheusServer) Password() (SecretRef, bool) {
	ref := SecretRef{Label: s.label("password"), Env: s.PasswordEnv, File: s.PasswordFile}
	return ref, ref.Env != "" || ref.File != ""
}

func (s PrometheusServer) sendsCredentialsInCleartext() bool {
	_, bearer := s.BearerToken()
	_, password := s.Password()
	if s.AllowHTTPAuth || (!bearer && !password && s.Username == "") {
		return false
	}
	u, err := url.Parse(s.URL)
	return err == nil && u.Scheme == "http" && !isLoopbackHost(u.Hostname())
}

func (s PrometheusServer) label(field string) string {
	return "prometheus " + s.Name + " " + field
}

func (c Catalog) PrometheusServer(name string) (PrometheusServer, bool) {
	for _, s := range c.Prometheus {
		if s.Name == name {
			return s, true
		}
	}
	return PrometheusServer{}, false
}

type NoDataPolicy string

const (
	NoDataPending NoDataPolicy = "pending"
	NoDataFail    NoDataPolicy = "fail"
)

type BaselineDirection string

const (
	BaselineIncrease BaselineDirection = "increase"
	BaselineDecrease BaselineDirection = "decrease"
)

type BaselineNoData string

const (
	BaselineNoDataIgnore BaselineNoData = "ignore"
	BaselineNoDataFail   BaselineNoData = "fail"
)

type Baseline struct {
	Direction BaselineDirection `yaml:"direction"`
	Ratio     float64           `yaml:"ratio"`
	Absolute  float64           `yaml:"absolute"`
	Window    *Duration         `yaml:"window"`
	NoData    BaselineNoData    `yaml:"no_data"`
}

func (b Baseline) withDefaults() Baseline {
	b.Direction = orDefault(b.Direction, BaselineIncrease)
	b.NoData = orDefault(b.NoData, BaselineNoDataIgnore)
	if b.Window == nil {
		b.Window = new(Duration(DefaultBaselineWindow))
	} else {
		b.Window = new(*b.Window)
	}
	return b
}

type ConditionOperator string

const (
	OpLess         ConditionOperator = "<"
	OpLessEqual    ConditionOperator = "<="
	OpGreater      ConditionOperator = ">"
	OpGreaterEqual ConditionOperator = ">="
	OpEqual        ConditionOperator = "=="
	OpNotEqual     ConditionOperator = "!="
)

type Condition struct {
	Operator  ConditionOperator
	Threshold float64
}

func ParseCondition(s string) (Condition, error) {
	text := strings.TrimSpace(s)
	var op ConditionOperator
	for _, candidate := range []ConditionOperator{OpLessEqual, OpGreaterEqual, OpEqual, OpNotEqual, OpLess, OpGreater} {
		if strings.HasPrefix(text, string(candidate)) {
			op = candidate
			break
		}
	}
	if op == "" {
		return Condition{}, fmt.Errorf("%q must be an operator (<, <=, >, >=, == or !=) followed by a number, such as \"< 0.05\"", s)
	}
	number := strings.TrimSpace(strings.TrimPrefix(text, string(op)))
	threshold, err := strconv.ParseFloat(number, 64)
	if err != nil || math.IsNaN(threshold) || math.IsInf(threshold, 0) {
		return Condition{}, fmt.Errorf("%q must compare with a finite number, such as \"< 0.05\"", s)
	}
	return Condition{Operator: op, Threshold: threshold}, nil
}

func (c Condition) Holds(v float64) bool {
	if math.IsNaN(v) {
		return false
	}
	switch c.Operator {
	case OpLess:
		return v < c.Threshold
	case OpLessEqual:
		return v <= c.Threshold
	case OpGreater:
		return v > c.Threshold
	case OpGreaterEqual:
		return v >= c.Threshold
	case OpEqual:
		return v == c.Threshold
	case OpNotEqual:
		return v != c.Threshold
	default:
		return false
	}
}

func (c Condition) String() string {
	return string(c.Operator) + " " + strconv.FormatFloat(c.Threshold, 'g', -1, 64)
}

type PrometheusQuery struct {
	Server           string
	Query            string
	Condition        *Condition
	Interval         time.Duration
	InitialDelay     time.Duration
	SuccessThreshold int
	FailureThreshold int
	NoData           NoDataPolicy
	Baseline         *PrometheusBaseline
}

type PrometheusBaseline struct {
	Direction BaselineDirection
	Ratio     float64
	Absolute  float64
	Window    time.Duration
	NoData    BaselineNoData
}

func (b PrometheusBaseline) Bound(baseline float64) float64 {
	margin := math.Abs(baseline)*b.Ratio + b.Absolute
	if b.Direction == BaselineDecrease {
		return baseline - margin
	}
	return baseline + margin
}

func (b PrometheusBaseline) Within(value, baseline float64) bool {
	if math.IsNaN(value) {
		return false
	}
	if b.Direction == BaselineDecrease {
		return value >= b.Bound(baseline)
	}
	return value <= b.Bound(baseline)
}

func (g Gate) PrometheusQuery() (PrometheusQuery, error) {
	if g.Type != GatePrometheus {
		return PrometheusQuery{}, fmt.Errorf("gate %s has type %s, not %s", g.Name, g.Type, GatePrometheus)
	}
	g = g.withPrometheusDefaults()
	q := PrometheusQuery{
		Server:           g.Server,
		Query:            strings.TrimSpace(g.Query),
		Interval:         g.Interval.Std(),
		InitialDelay:     g.InitialDelay.Std(),
		SuccessThreshold: *g.SuccessThreshold,
		FailureThreshold: *g.FailureThreshold,
		NoData:           g.NoData,
	}
	var errs []error
	if err := validatePromQL(g.Query); err != nil {
		errs = append(errs, fmt.Errorf("query: %w", err))
	}
	if g.Condition != "" {
		c, err := ParseCondition(g.Condition)
		if err != nil {
			errs = append(errs, fmt.Errorf("condition: %w", err))
		}
		q.Condition = &c
	}
	if g.Baseline != nil {
		q.Baseline = &PrometheusBaseline{
			Direction: g.Baseline.Direction,
			Ratio:     g.Baseline.Ratio,
			Absolute:  g.Baseline.Absolute,
			Window:    g.Baseline.Window.Std(),
			NoData:    g.Baseline.NoData,
		}
	}
	if q.Condition == nil && q.Baseline == nil {
		errs = append(errs, errors.New("needs a condition, a baseline or both"))
	}
	if q.Interval <= 0 || q.SuccessThreshold < 1 || q.FailureThreshold < 1 {
		errs = append(errs, errors.New("interval and thresholds must be positive"))
	}
	if err := errors.Join(errs...); err != nil {
		return PrometheusQuery{}, fmt.Errorf("prometheus gate %s: %w", g.Name, err)
	}
	return q, nil
}

func (g Gate) withPrometheusDefaults() Gate {
	if g.Interval == nil {
		g.Interval = new(Duration(DefaultPrometheusInterval))
	} else {
		g.Interval = new(*g.Interval)
	}
	g.SuccessThreshold = intOrDefault(g.SuccessThreshold, DefaultPrometheusSuccessThreshold)
	g.FailureThreshold = intOrDefault(g.FailureThreshold, DefaultPrometheusFailureThreshold)
	g.NoData = orDefault(g.NoData, NoDataPending)
	if g.Baseline != nil {
		b := g.Baseline.withDefaults()
		g.Baseline = &b
	}
	return g
}

func intOrDefault(v *int, fallback int) *int {
	if v == nil {
		return &fallback
	}
	return new(*v)
}

func prometheusTimeout(g Gate) time.Duration {
	g = g.withPrometheusDefaults()
	interval := min(max(g.Interval.Std(), 0), MaxPrometheusInterval)
	samples := min(max(*g.SuccessThreshold, 0), MaxPrometheusSuccessThreshold)
	delay := min(max(g.InitialDelay.Std(), 0), MaxPrometheusInitialDelay)
	return delay + time.Duration(samples)*interval + DefaultTimeoutAllowance
}

func prometheusMinimumTimeout(g Gate) time.Duration {
	return g.InitialDelay.Std() + time.Duration(*g.SuccessThreshold-1)*g.Interval.Std()
}

func validatePromQL(q string) error {
	switch {
	case strings.TrimSpace(q) == "":
		return fmt.Errorf("is required for type %s", GatePrometheus)
	case len(q) > MaxPrometheusQueryBytes:
		return fmt.Errorf("is %d bytes, more than the limit of %d", len(q), MaxPrometheusQueryBytes)
	case !utf8.ValidString(q):
		return errors.New("must be valid UTF-8")
	case strings.ContainsRune(q, 0):
		return errors.New("must not contain NUL characters")
	}
	return nil
}

func validatePrometheusServer(r reporter, s PrometheusServer) {
	r.check("name", validateName(s.Name))
	if s.URL == "" {
		r.addf("url", "is required")
	} else if err := validateURL(s.URL, "bearer_token_env, bearer_token_file or username with password_env or password_file"); err != nil {
		r.check("url", err)
	} else if s.sendsCredentialsInCleartext() {
		r.addf("url", "plain http would send the credentials unencrypted; use https, or set allow_http_auth: true if the network path is trusted")
	}
	if s.BearerTokenEnv != "" && s.BearerTokenFile != "" {
		r.addf("bearer_token_env, bearer_token_file", "set at most one")
	}
	if s.PasswordEnv != "" && s.PasswordFile != "" {
		r.addf("password_env, password_file", "set at most one")
	}
	_, bearer := s.BearerToken()
	_, password := s.Password()
	switch {
	case bearer && (s.Username != "" || password):
		r.addf("bearer_token_*, username", "use a bearer token or basic auth, not both")
	case s.Username != "" && !password:
		r.addf("password_env, password_file", "set one when username is set")
	case s.Username == "" && password:
		r.addf("username", "is required with password_env or password_file")
	}
	if s.Username != "" {
		r.check("username", validateUsername(s.Username))
	}
	for _, f := range []field{{"bearer_token_env", s.BearerTokenEnv}, {"password_env", s.PasswordEnv}} {
		if f.value != "" {
			r.check(f.name, validateEnvName(f.value))
		}
	}
	for _, f := range []field{{"bearer_token_file", s.BearerTokenFile}, {"password_file", s.PasswordFile}} {
		if f.value != "" {
			r.check(f.name, validateSecretFile(f.value))
		}
	}
}

func validateUsername(s string) error {
	switch {
	case !utf8.ValidString(s):
		return errors.New("must be valid UTF-8")
	case strings.ContainsRune(s, ':'):
		return errors.New("must not contain ':'")
	case strings.ContainsFunc(s, unicode.IsControl):
		return errors.New("must not contain control characters")
	}
	return nil
}

func validatePrometheusGate(r reporter, field string, g Gate, servers []string) {
	r.check(field+".query", validatePromQL(g.Query))
	if g.Condition == "" && g.Baseline == nil {
		r.addf(field+".condition", "set condition, baseline or both for type %s", g.Type)
	}
	if g.Condition != "" {
		_, err := ParseCondition(g.Condition)
		r.check(field+".condition", err)
	}
	validatePrometheusServerRef(r, field+".server", g.Server, servers)

	g = g.withPrometheusDefaults()
	interval := g.Interval.Std()
	intervalOK := interval >= MinPrometheusInterval && interval <= MaxPrometheusInterval
	if !intervalOK {
		r.addf(field+".interval", "%s is out of range (want %s to %s)", interval, MinPrometheusInterval, MaxPrometheusInterval)
	}
	delayOK := g.InitialDelay >= 0 && g.InitialDelay.Std() <= MaxPrometheusInitialDelay
	if !delayOK {
		r.addf(field+".initial_delay", "%s is out of range (want 0s to %s)", g.InitialDelay, MaxPrometheusInitialDelay)
	}
	successOK := *g.SuccessThreshold >= 1 && *g.SuccessThreshold <= MaxPrometheusSuccessThreshold
	if !successOK {
		r.addf(field+".success_threshold", "%d is out of range (want 1 to %d)", *g.SuccessThreshold, MaxPrometheusSuccessThreshold)
	}
	if n := *g.FailureThreshold; n < 1 || n > MaxPrometheusFailureThreshold {
		r.addf(field+".failure_threshold", "%d is out of range (want 1 to %d)", n, MaxPrometheusFailureThreshold)
	}
	switch g.NoData {
	case NoDataPending, NoDataFail:
	default:
		r.addf(field+".no_data", "unknown policy %q (want pending or fail)", g.NoData)
	}
	if g.Baseline != nil {
		validateBaseline(r, field+".baseline", *g.Baseline, interval, intervalOK)
	}
	if intervalOK && successOK && delayOK && g.Timeout != nil && *g.Timeout > 0 {
		if minimum := prometheusMinimumTimeout(g); g.Timeout.Std() <= minimum {
			r.addf(field+".timeout", "%s must be longer than initial_delay + (success_threshold - 1) x interval (%s)", g.Timeout, minimum)
		}
	}
}

func validatePrometheusServerRef(r reporter, field, name string, servers []string) {
	known := strings.Join(servers, ", ")
	switch {
	case len(servers) == 0:
		r.addf(field, "no prometheus servers are configured; add one under %s", prometheusServerList)
	case name == "" && len(servers) > 1:
		r.addf(field, "is required when several prometheus servers are configured (want one of %s)", known)
	case name != "" && !slices.Contains(servers, name):
		r.addf(field, "unknown prometheus server %q (want one of %s)", name, known)
	}
}

func validateBaseline(r reporter, field string, b Baseline, interval time.Duration, intervalOK bool) {
	switch b.Direction {
	case BaselineIncrease, BaselineDecrease:
	default:
		r.addf(field+".direction", "unknown direction %q (want increase or decrease)", b.Direction)
	}
	finite := true
	for _, f := range []struct {
		name  string
		value float64
	}{{"ratio", b.Ratio}, {"absolute", b.Absolute}} {
		if math.IsNaN(f.value) || math.IsInf(f.value, 0) || f.value < 0 {
			r.addf(field+"."+f.name, "must be a finite number of at least 0")
			finite = false
		}
	}
	if finite && b.Ratio == 0 && b.Absolute == 0 {
		r.addf(field+".ratio, "+field+".absolute", "set ratio, absolute or both above 0")
	}
	window := b.Window.Std()
	switch {
	case window <= 0:
		r.addf(field+".window", "must be positive")
	case intervalOK && window < interval:
		r.addf(field+".window", "%s must be at least the interval %s", window, interval)
	case intervalOK && window/interval > MaxBaselinePoints:
		r.addf(field+".window", "%s holds more than %d samples at interval %s", window, MaxBaselinePoints, interval)
	}
	switch b.NoData {
	case BaselineNoDataIgnore, BaselineNoDataFail:
	default:
		r.addf(field+".no_data", "unknown policy %q (want ignore or fail)", b.NoData)
	}
}

func forbidPrometheusFields(r reporter, field string, g Gate) {
	forbidden := []struct {
		name string
		set  bool
	}{
		{"server", g.Server != ""},
		{"query", g.Query != ""},
		{"condition", g.Condition != ""},
		{"interval", g.Interval != nil},
		{"initial_delay", g.InitialDelay != 0},
		{"success_threshold", g.SuccessThreshold != nil},
		{"failure_threshold", g.FailureThreshold != nil},
		{"no_data", g.NoData != ""},
		{"baseline", g.Baseline != nil},
	}
	for _, f := range forbidden {
		if f.set {
			r.addf(field+"."+f.name, "not allowed for type %s", g.Type)
		}
	}
}

func (g Gate) withCopiedPointers() Gate {
	g.Interval = copyOf(g.Interval)
	g.SuccessThreshold = copyOf(g.SuccessThreshold)
	g.FailureThreshold = copyOf(g.FailureThreshold)
	if g.Baseline != nil {
		b := *g.Baseline
		b.Window = copyOf(b.Window)
		g.Baseline = &b
	}
	return g
}

func copyOf[T any](p *T) *T {
	if p == nil {
		return nil
	}
	return new(*p)
}

func (c Catalog) fillPrometheusServer(name string) {
	for _, a := range c.Apps {
		for _, phase := range [][]Gate{a.Gates.Before, a.Gates.After} {
			for i := range phase {
				if phase[i].Type == GatePrometheus && phase[i].Server == "" {
					phase[i].Server = name
				}
			}
		}
	}
}
