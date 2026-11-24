package config

import (
	"math"
	"strings"
	"testing"
	"time"
)

func prometheusCatalog(servers ...PrometheusServer) Catalog {
	if servers == nil {
		servers = []PrometheusServer{{Name: "main", URL: "http://prometheus.monitoring.svc:9090", BearerTokenFile: "/var/run/secrets/prometheus-token"}}
	}
	return File{
		Prometheus: servers,
		Apps: []App{{
			Name:   "api",
			Source: Source{Repo: "example-org/api"},
			Image:  Image{Repository: "ghcr.io/example-org/api"},
			GitOps: GitOps{Repo: "example-org/platform", Path: "apps/api"},
			Gates: Gates{After: []Gate{
				{Type: GateHealth},
				{Type: GatePrometheus, Name: "error-rate", Query: "sum(rate(errors[5m]))", Condition: "< 0.05"},
			}},
		}},
	}.Catalog()
}

func promGate(c *Catalog) *Gate {
	return &c.Apps[0].Gates.After[1]
}

func TestPrometheusValidate(t *testing.T) {
	t.Parallel()

	const gate = "apps[0] (api): gates.after[1]"
	tests := []struct {
		name    string
		mutate  func(*Catalog)
		wantErr []string
	}{
		{name: "valid", mutate: func(*Catalog) {}},
		{
			name: "valid with every setting",
			mutate: func(c *Catalog) {
				g := promGate(c)
				g.Server = "main"
				g.Interval = durPtr(30 * time.Second)
				g.InitialDelay = Duration(2 * time.Minute)
				g.SuccessThreshold = intPtr(10)
				g.FailureThreshold = intPtr(1)
				g.NoData = NoDataFail
				g.Baseline = &Baseline{Direction: BaselineDecrease, Ratio: 0.1, Window: durPtr(time.Hour), NoData: BaselineNoDataFail}
				g.Timeout = durPtr(time.Hour)
			},
		},
		{name: "baseline only", mutate: func(c *Catalog) { promGate(c).Condition = ""; promGate(c).Baseline = &Baseline{Absolute: 1} }},
		{
			name: "valid basic auth",
			mutate: func(c *Catalog) {
				c.Prometheus[0].BearerTokenFile = ""
				c.Prometheus[0].Username = "doorstep"
				c.Prometheus[0].PasswordEnv = "PROM_PASSWORD"
			},
		},
		{name: "no auth", mutate: func(c *Catalog) { c.Prometheus[0].BearerTokenFile = "" }},
		{name: "https with path prefix", mutate: func(c *Catalog) { c.Prometheus[0].URL = "https://mimir.example.com/prometheus" }},

		{name: "server name required", mutate: func(c *Catalog) { c.Prometheus[0].Name = "" }, wantErr: []string{"prometheus[0]: name: is required"}},
		{name: "server name not a DNS label", mutate: func(c *Catalog) { c.Prometheus[0].Name = "Main" }, wantErr: []string{"prometheus[0] (Main): name:", "DNS label"}},
		{
			name: "server name duplicate",
			mutate: func(c *Catalog) {
				c.Prometheus = append(c.Prometheus, PrometheusServer{Name: "main", URL: "http://other:9090"})
				promGate(c).Server = "main"
			},
			wantErr: []string{"prometheus[1] (main): name: duplicate of prometheus[0]"},
		},
		{name: "url required", mutate: func(c *Catalog) { c.Prometheus[0].URL = "" }, wantErr: []string{"prometheus[0] (main): url: is required"}},
		{name: "url scheme", mutate: func(c *Catalog) { c.Prometheus[0].URL = "ftp://prom" }, wantErr: []string{"url: must use http or https"}},
		{name: "url credentials", mutate: func(c *Catalog) { c.Prometheus[0].URL = "http://u:p@prom" }, wantErr: []string{"url: must not include credentials; use bearer_token_env, bearer_token_file or username with password_env or password_file"}},
		{name: "url query", mutate: func(c *Catalog) { c.Prometheus[0].URL = "http://prom?token=x" }, wantErr: []string{"url: must not include a query string or fragment"}},
		{name: "two bearer sources", mutate: func(c *Catalog) { c.Prometheus[0].BearerTokenEnv = "TOKEN" }, wantErr: []string{"bearer_token_env, bearer_token_file: set at most one"}},
		{
			name: "two password sources",
			mutate: func(c *Catalog) {
				c.Prometheus[0].BearerTokenFile = ""
				c.Prometheus[0].Username = "u"
				c.Prometheus[0].PasswordEnv = "P"
				c.Prometheus[0].PasswordFile = "/p"
			},
			wantErr: []string{"password_env, password_file: set at most one"},
		},
		{name: "bearer and basic", mutate: func(c *Catalog) { c.Prometheus[0].Username = "u" }, wantErr: []string{"bearer_token_*, username: use a bearer token or basic auth, not both"}},
		{name: "bearer and password", mutate: func(c *Catalog) { c.Prometheus[0].PasswordEnv = "P" }, wantErr: []string{"use a bearer token or basic auth, not both"}},
		{
			name:    "username without password",
			mutate:  func(c *Catalog) { c.Prometheus[0].BearerTokenFile = ""; c.Prometheus[0].Username = "u" },
			wantErr: []string{"password_env, password_file: set one when username is set"},
		},
		{
			name:    "password without username",
			mutate:  func(c *Catalog) { c.Prometheus[0].BearerTokenFile = ""; c.Prometheus[0].PasswordFile = "/p" },
			wantErr: []string{"username: is required with password_env or password_file"},
		},
		{
			name: "username with colon",
			mutate: func(c *Catalog) {
				c.Prometheus[0].BearerTokenFile = ""
				c.Prometheus[0].Username = "a:b"
				c.Prometheus[0].PasswordEnv = "P"
			},
			wantErr: []string{"username: must not contain ':'"},
		},
		{
			name: "username with control character",
			mutate: func(c *Catalog) {
				c.Prometheus[0].BearerTokenFile = ""
				c.Prometheus[0].Username = "a\nb"
				c.Prometheus[0].PasswordEnv = "P"
			},
			wantErr: []string{"username: must not contain control characters"},
		},
		{name: "bad env name", mutate: func(c *Catalog) { c.Prometheus[0].BearerTokenFile = ""; c.Prometheus[0].BearerTokenEnv = "1X" }, wantErr: []string{"bearer_token_env: \"1X\" is not a valid environment variable name"}},
		{name: "relative file", mutate: func(c *Catalog) { c.Prometheus[0].BearerTokenFile = "token" }, wantErr: []string{"bearer_token_file: \"token\" must be an absolute path"}},

		{name: "query required", mutate: func(c *Catalog) { promGate(c).Query = "  " }, wantErr: []string{gate + ".query: is required for type prometheus"}},
		{name: "query too long", mutate: func(c *Catalog) { promGate(c).Query = strings.Repeat("x", 4097) }, wantErr: []string{".query: is 4097 bytes, more than the limit of 4096"}},
		{name: "query at the limit", mutate: func(c *Catalog) { promGate(c).Query = strings.Repeat("x", 4096) }},
		{name: "query invalid UTF-8", mutate: func(c *Catalog) { promGate(c).Query = "up\xff" }, wantErr: []string{".query: must be valid UTF-8"}},
		{name: "query with NUL", mutate: func(c *Catalog) { promGate(c).Query = "up\x00" }, wantErr: []string{".query: must not contain NUL characters"}},
		{name: "condition or baseline required", mutate: func(c *Catalog) { promGate(c).Condition = "" }, wantErr: []string{".condition: set condition, baseline or both for type prometheus"}},
		{name: "condition invalid", mutate: func(c *Catalog) { promGate(c).Condition = "below 3" }, wantErr: []string{".condition: \"below 3\" must be an operator"}},
		{name: "interval too short", mutate: func(c *Catalog) { promGate(c).Interval = durPtr(5 * time.Second) }, wantErr: []string{".interval: 5s is out of range (want 10s to 1h0m0s)"}},
		{name: "interval too long", mutate: func(c *Catalog) { promGate(c).Interval = durPtr(2 * time.Hour) }, wantErr: []string{".interval: 2h0m0s is out of range"}},
		{name: "interval at the bounds", mutate: func(c *Catalog) { promGate(c).Interval = durPtr(10 * time.Second) }},
		{name: "negative initial delay", mutate: func(c *Catalog) { promGate(c).InitialDelay = Duration(-time.Second) }, wantErr: []string{".initial_delay: must not be negative"}},
		{name: "success threshold zero", mutate: func(c *Catalog) { promGate(c).SuccessThreshold = intPtr(0) }, wantErr: []string{".success_threshold: 0 is out of range (want 1 to 1000)"}},
		{name: "success threshold too high", mutate: func(c *Catalog) { promGate(c).SuccessThreshold = intPtr(1001) }, wantErr: []string{".success_threshold: 1001 is out of range"}},
		{name: "failure threshold zero", mutate: func(c *Catalog) { promGate(c).FailureThreshold = intPtr(0) }, wantErr: []string{".failure_threshold: 0 is out of range (want 1 to 100)"}},
		{name: "failure threshold too high", mutate: func(c *Catalog) { promGate(c).FailureThreshold = intPtr(101) }, wantErr: []string{".failure_threshold: 101 is out of range"}},
		{name: "unknown no_data", mutate: func(c *Catalog) { promGate(c).NoData = "pass" }, wantErr: []string{".no_data: unknown policy \"pass\" (want pending or fail)"}},
		{name: "baseline direction", mutate: func(c *Catalog) { promGate(c).Baseline = &Baseline{Direction: "up", Ratio: 1} }, wantErr: []string{".baseline.direction: unknown direction \"up\""}},
		{name: "baseline negative ratio", mutate: func(c *Catalog) { promGate(c).Baseline = &Baseline{Ratio: -1, Absolute: 1} }, wantErr: []string{".baseline.ratio: must be a finite number of at least 0"}},
		{name: "baseline infinite absolute", mutate: func(c *Catalog) { promGate(c).Baseline = &Baseline{Absolute: math.Inf(1)} }, wantErr: []string{".baseline.absolute: must be a finite number"}},
		{name: "baseline NaN ratio", mutate: func(c *Catalog) { promGate(c).Baseline = &Baseline{Ratio: math.NaN()} }, wantErr: []string{".baseline.ratio: must be a finite number"}},
		{name: "baseline without margin", mutate: func(c *Catalog) { promGate(c).Baseline = &Baseline{} }, wantErr: []string{gate + ".baseline.ratio, gates.after[1].baseline.absolute: set ratio, absolute or both above 0"}},
		{name: "baseline window below interval", mutate: func(c *Catalog) { promGate(c).Baseline = &Baseline{Ratio: 1, Window: durPtr(30 * time.Second)} }, wantErr: []string{".baseline.window: 30s must be at least the interval 1m0s"}},
		{name: "baseline window zero", mutate: func(c *Catalog) { promGate(c).Baseline = &Baseline{Ratio: 1, Window: durPtr(0)} }, wantErr: []string{".baseline.window: must be positive"}},
		{
			name:    "baseline window too many points",
			mutate:  func(c *Catalog) { promGate(c).Baseline = &Baseline{Ratio: 1, Window: durPtr(1001 * time.Minute)} },
			wantErr: []string{".baseline.window: 16h41m0s holds more than 1000 samples at interval 1m0s"},
		},
		{name: "baseline window at the limit", mutate: func(c *Catalog) { promGate(c).Baseline = &Baseline{Ratio: 1, Window: durPtr(1000 * time.Minute)} }},
		{name: "baseline no_data", mutate: func(c *Catalog) { promGate(c).Baseline = &Baseline{Ratio: 1, NoData: "pending"} }, wantErr: []string{".baseline.no_data: unknown policy \"pending\" (want ignore or fail)"}},
		{name: "unknown server", mutate: func(c *Catalog) { promGate(c).Server = "other" }, wantErr: []string{".server: unknown prometheus server \"other\" (want one of main)"}},
		{name: "no servers", mutate: func(c *Catalog) { c.Prometheus = nil }, wantErr: []string{".server: no prometheus servers are configured; add one under prometheus"}},
		{
			name: "server required with several",
			mutate: func(c *Catalog) {
				c.Prometheus = append(c.Prometheus, PrometheusServer{Name: "other", URL: "http://other:9090"})
				promGate(c).Server = ""
			},
			wantErr: []string{".server: is required when several prometheus servers are configured (want one of main, other)"},
		},
		{
			name:    "timeout too short to pass",
			mutate:  func(c *Catalog) { promGate(c).Timeout = durPtr(4 * time.Minute) },
			wantErr: []string{".timeout: 4m0s must be longer than initial_delay + (success_threshold - 1) x interval (4m0s)"},
		},
		{name: "timeout just long enough", mutate: func(c *Catalog) { promGate(c).Timeout = durPtr(4*time.Minute + time.Second) }},
		{
			name: "timeout counts the initial delay",
			mutate: func(c *Catalog) {
				promGate(c).InitialDelay = Duration(time.Minute)
				promGate(c).Timeout = durPtr(5 * time.Minute)
			},
			wantErr: []string{".timeout: 5m0s must be longer than initial_delay + (success_threshold - 1) x interval (5m0s)"},
		},
		{name: "timeout zero", mutate: func(c *Catalog) { promGate(c).Timeout = durPtr(0) }, wantErr: []string{".timeout: must be positive"}},
		{name: "duration not allowed", mutate: func(c *Catalog) { promGate(c).Duration = Duration(time.Minute) }, wantErr: []string{".duration: not allowed for type prometheus"}},
		{name: "windows not allowed", mutate: func(c *Catalog) { promGate(c).Windows = []Window{} }, wantErr: []string{".windows: not allowed for type prometheus"}},
		{
			name: "before phase rejected",
			mutate: func(c *Catalog) {
				c.Apps[0].Gates.Before = []Gate{{Type: GatePrometheus, Name: "early", Query: "up", Condition: "> 0", Server: "main", Timeout: durPtr(time.Hour)}}
			},
			wantErr: []string{"gates.before[0].type: prometheus gates run only in gates.after"},
		},
		{
			name: "prometheus fields rejected on other types",
			mutate: func(c *Catalog) {
				g := &c.Apps[0].Gates.After[0]
				g.Server = "main"
				g.Query = "up"
				g.Condition = "> 0"
				g.Interval = durPtr(time.Minute)
				g.InitialDelay = Duration(time.Second)
				g.SuccessThreshold = intPtr(1)
				g.FailureThreshold = intPtr(1)
				g.NoData = NoDataFail
				g.Baseline = &Baseline{}
			},
			wantErr: []string{
				"gates.after[0].server: not allowed for type health",
				"gates.after[0].query: not allowed for type health",
				"gates.after[0].condition: not allowed for type health",
				"gates.after[0].interval: not allowed for type health",
				"gates.after[0].initial_delay: not allowed for type health",
				"gates.after[0].success_threshold: not allowed for type health",
				"gates.after[0].failure_threshold: not allowed for type health",
				"gates.after[0].no_data: not allowed for type health",
				"gates.after[0].baseline: not allowed for type health",
			},
		},
		{
			name:    "zero-valued prometheus fields rejected on other types",
			mutate:  func(c *Catalog) { c.Apps[0].Gates.After[0].SuccessThreshold = intPtr(0) },
			wantErr: []string{"gates.after[0].success_threshold: not allowed for type health"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := prometheusCatalog()
			tt.mutate(&c)
			err := c.Validate()
			assertErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestPrometheusDefaults(t *testing.T) {
	t.Parallel()

	c := prometheusCatalog()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	g := *promGate(&c)
	if g.Server != "main" {
		t.Errorf("server = %q, want the only server", g.Server)
	}
	if *g.Interval != Duration(time.Minute) || *g.SuccessThreshold != 5 || *g.FailureThreshold != 3 || g.NoData != NoDataPending {
		t.Errorf("defaults = interval %s, thresholds %d/%d, no_data %q", g.Interval, *g.SuccessThreshold, *g.FailureThreshold, g.NoData)
	}
	if want := Duration(15 * time.Minute); *g.Timeout != want {
		t.Errorf("timeout = %s, want %s", g.Timeout, want)
	}

	tests := []struct {
		name        string
		gate        Gate
		wantTimeout time.Duration
	}{
		{name: "initial delay and thresholds", gate: Gate{InitialDelay: Duration(2 * time.Minute), SuccessThreshold: intPtr(3), Interval: durPtr(30 * time.Second)}, wantTimeout: 2*time.Minute + 90*time.Second + 10*time.Minute},
		{name: "explicit timeout kept", gate: Gate{Timeout: durPtr(time.Hour)}, wantTimeout: time.Hour},
		{name: "out of range values do not overflow", gate: Gate{SuccessThreshold: intPtr(math.MaxInt), Interval: durPtr(math.MaxInt64)}, wantTimeout: 1000*time.Hour + 10*time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.gate.Type = GatePrometheus
			got := Gates{After: []Gate{tt.gate}}.withDefaults().After[0]
			if got.Timeout.Std() != tt.wantTimeout {
				t.Errorf("timeout = %s, want %s", got.Timeout, tt.wantTimeout)
			}
		})
	}

	t.Run("baseline defaults", func(t *testing.T) {
		t.Parallel()
		got := Gates{After: []Gate{{Type: GatePrometheus, Baseline: &Baseline{Ratio: 0.5}}}}.withDefaults().After[0].Baseline
		if got.Direction != BaselineIncrease || *got.Window != Duration(30*time.Minute) || got.NoData != BaselineNoDataIgnore || got.Ratio != 0.5 {
			t.Errorf("baseline = %+v", got)
		}
	})

	t.Run("several servers are not defaulted", func(t *testing.T) {
		t.Parallel()
		c := prometheusCatalog(PrometheusServer{Name: "a", URL: "http://a"}, PrometheusServer{Name: "b", URL: "http://b"})
		if got := promGate(&c).Server; got != "" {
			t.Errorf("server = %q, want empty", got)
		}
	})

	t.Run("an explicit server is kept", func(t *testing.T) {
		t.Parallel()
		file := File{
			Prometheus: []PrometheusServer{{Name: "main", URL: "http://a"}},
			Apps:       []App{{Name: "api", Gates: Gates{After: []Gate{{Type: GatePrometheus, Server: "other"}, {Type: GateHealth}}}}},
		}
		c := file.Catalog()
		if got := c.Apps[0].Gates.After[0].Server; got != "other" {
			t.Errorf("server = %q, want other", got)
		}
		if got := c.Apps[0].Gates.After[1].Server; got != "" {
			t.Errorf("health gate server = %q, want empty", got)
		}
		if file.Apps[0].Gates.After[0].Server != "other" {
			t.Error("Catalog changed the file")
		}
	})
}

func TestPrometheusCatalogDoesNotAlias(t *testing.T) {
	t.Parallel()

	file := File{
		Prometheus: []PrometheusServer{{Name: "main", URL: "http://a"}},
		Apps: []App{{Name: "api", Gates: Gates{After: []Gate{
			{Type: GatePrometheus, Interval: durPtr(time.Minute), SuccessThreshold: intPtr(2), FailureThreshold: intPtr(2), Baseline: &Baseline{Ratio: 1, Window: durPtr(time.Hour)}},
			{Type: GateHealth, Interval: durPtr(time.Minute), SuccessThreshold: intPtr(2), Baseline: &Baseline{Window: durPtr(time.Hour)}},
		}}}},
	}
	c := file.Catalog()
	c.Prometheus[0].Name = "changed"
	for i := range c.Apps[0].Gates.After {
		g := &c.Apps[0].Gates.After[i]
		*g.Interval = 0
		*g.SuccessThreshold = 0
		*g.Baseline.Window = 0
		g.Baseline.Ratio = 9
	}
	*c.Apps[0].Gates.After[0].FailureThreshold = 0

	if file.Prometheus[0].Name != "main" {
		t.Error("Catalog shares the server list")
	}
	for i, g := range file.Apps[0].Gates.After {
		if *g.Interval != Duration(time.Minute) || *g.SuccessThreshold != 2 || *g.Baseline.Window != Duration(time.Hour) || g.Baseline.Ratio == 9 {
			t.Errorf("gate %d shares memory with the file: %+v", i, g)
		}
	}
	if *file.Apps[0].Gates.After[0].FailureThreshold != 2 {
		t.Error("failure threshold is shared")
	}
}

func TestParsePrometheus(t *testing.T) {
	t.Parallel()

	const input = `
prometheus:
  - name: main
    url: http://prometheus-operated.monitoring.svc:9090
    bearer_token_file: /var/run/secrets/doorstep/prometheus-token
apps:
  - name: api
    source: {repo: example-org/api}
    image: {repository: ghcr.io/example-org/api}
    gitops: {repo: example-org/platform, path: apps/api}
    gates:
      after:
        - type: prometheus
          name: error-rate
          query: sum(rate(http_requests_total{code=~"5.."}[5m])) / sum(rate(http_requests_total[5m]))
          condition: "< 0.05"
          interval: 30s
          initial_delay: 2m
          success_threshold: 4
          failure_threshold: 2
          no_data: fail
          baseline:
            direction: decrease
            ratio: 0.2
            absolute: 0.001
            window: 1h
            no_data: fail
          timeout: 20m
`
	file, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c := file.Catalog()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	q, err := c.Apps[0].Gates.After[0].PrometheusQuery()
	if err != nil {
		t.Fatalf("PrometheusQuery: %v", err)
	}
	want := PrometheusQuery{
		Server:           "main",
		Query:            `sum(rate(http_requests_total{code=~"5.."}[5m])) / sum(rate(http_requests_total[5m]))`,
		Condition:        &Condition{Operator: OpLess, Threshold: 0.05},
		Interval:         30 * time.Second,
		InitialDelay:     2 * time.Minute,
		SuccessThreshold: 4,
		FailureThreshold: 2,
		NoData:           NoDataFail,
		Baseline:         &PrometheusBaseline{Direction: BaselineDecrease, Ratio: 0.2, Absolute: 0.001, Window: time.Hour, NoData: BaselineNoDataFail},
	}
	if q.Server != want.Server || q.Query != want.Query || *q.Condition != *want.Condition || q.Interval != want.Interval ||
		q.InitialDelay != want.InitialDelay || q.SuccessThreshold != want.SuccessThreshold || q.FailureThreshold != want.FailureThreshold ||
		q.NoData != want.NoData || *q.Baseline != *want.Baseline {
		t.Errorf("PrometheusQuery() = %+v, want %+v", q, want)
	}

	for _, typo := range []string{
		"prometheus:\n  - name: main\n    url: http://a\n    bearer_token: x\n",
		"apps:\n  - name: api\n    gates:\n      after:\n        - type: prometheus\n          baseline:\n            ration: 1\n",
		"apps:\n  - name: api\n    gates:\n      after:\n        - type: prometheus\n          succes_threshold: 1\n",
		"apps:\n  - name: api\n    gates:\n      after:\n        - type: prometheus\n          interval: 60\n",
	} {
		if _, err := Parse(strings.NewReader(typo)); err == nil {
			t.Errorf("Parse accepted %q", typo)
		}
	}
}

func TestPrometheusQueryAccessor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		gate    Gate
		wantErr []string
		check   func(*testing.T, PrometheusQuery)
	}{
		{
			name: "defaults",
			gate: Gate{Type: GatePrometheus, Name: "p", Query: " up ", Condition: "> 0"},
			check: func(t *testing.T, q PrometheusQuery) {
				t.Helper()
				if q.Query != "up" || q.Interval != time.Minute || q.SuccessThreshold != 5 || q.FailureThreshold != 3 || q.NoData != NoDataPending || q.Baseline != nil {
					t.Errorf("PrometheusQuery() = %+v", q)
				}
			},
		},
		{
			name: "baseline defaults",
			gate: Gate{Type: GatePrometheus, Name: "p", Query: "up", Baseline: &Baseline{Absolute: 1}},
			check: func(t *testing.T, q PrometheusQuery) {
				t.Helper()
				if q.Condition != nil || *q.Baseline != (PrometheusBaseline{Direction: BaselineIncrease, Absolute: 1, Window: 30 * time.Minute, NoData: BaselineNoDataIgnore}) {
					t.Errorf("PrometheusQuery() = %+v", q)
				}
			},
		},
		{name: "wrong type", gate: Gate{Type: GateSoak, Name: "s"}, wantErr: []string{"gate s has type soak, not prometheus"}},
		{name: "bad condition", gate: Gate{Type: GatePrometheus, Name: "p", Query: "up", Condition: "~ 1"}, wantErr: []string{"prometheus gate p: condition:"}},
		{name: "no condition or baseline", gate: Gate{Type: GatePrometheus, Name: "p", Query: "up"}, wantErr: []string{"needs a condition, a baseline or both"}},
		{name: "no query", gate: Gate{Type: GatePrometheus, Name: "p", Condition: "> 1"}, wantErr: []string{"query: is required"}},
		{name: "zero threshold", gate: Gate{Type: GatePrometheus, Name: "p", Query: "up", Condition: "> 1", SuccessThreshold: intPtr(0)}, wantErr: []string{"interval and thresholds must be positive"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			q, err := tt.gate.PrometheusQuery()
			assertErrorContains(t, err, tt.wantErr)
			if tt.check != nil {
				tt.check(t, q)
			}
		})
	}
}

func TestParseCondition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input   string
		want    Condition
		wantStr string
		wantErr bool
	}{
		{input: "< 0.05", want: Condition{OpLess, 0.05}, wantStr: "< 0.05"},
		{input: "<0.05", want: Condition{OpLess, 0.05}, wantStr: "< 0.05"},
		{input: "  <=   1e3 ", want: Condition{OpLessEqual, 1000}, wantStr: "<= 1000"},
		{input: "> -2", want: Condition{OpGreater, -2}, wantStr: "> -2"},
		{input: ">= 0", want: Condition{OpGreaterEqual, 0}, wantStr: ">= 0"},
		{input: "== 1", want: Condition{OpEqual, 1}, wantStr: "== 1"},
		{input: "!= 0.5", want: Condition{OpNotEqual, 0.5}, wantStr: "!= 0.5"},
		{input: "", wantErr: true},
		{input: "<", wantErr: true},
		{input: "= 1", wantErr: true},
		{input: "=< 1", wantErr: true},
		{input: "0.05", wantErr: true},
		{input: "< NaN", wantErr: true},
		{input: "< Inf", wantErr: true},
		{input: "< -inf", wantErr: true},
		{input: "< 1e400", wantErr: true},
		{input: "< 1 or 2", wantErr: true},
		{input: "< 0.05)", wantErr: true},
		{input: "<< 1", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got, err := ParseCondition(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseCondition(%q) = %+v, want an error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCondition(%q): %v", tt.input, err)
			}
			if got != tt.want || got.String() != tt.wantStr {
				t.Errorf("ParseCondition(%q) = %+v (%s), want %+v (%s)", tt.input, got, got, tt.want, tt.wantStr)
			}
		})
	}
}

func TestConditionHolds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		op    ConditionOperator
		value float64
		want  bool
	}{
		{OpLess, 0.9, true},
		{OpLess, 1, false},
		{OpLess, 1.1, false},
		{OpLessEqual, 1, true},
		{OpLessEqual, 1.1, false},
		{OpGreater, 1.1, true},
		{OpGreater, 1, false},
		{OpGreaterEqual, 1, true},
		{OpGreaterEqual, 0.9, false},
		{OpEqual, 1, true},
		{OpEqual, 0.9, false},
		{OpNotEqual, 0.9, true},
		{OpNotEqual, 1, false},
		{OpLess, math.Inf(-1), true},
		{OpLess, math.Inf(1), false},
		{OpLess, math.NaN(), false},
		{OpGreater, math.NaN(), false},
		{OpNotEqual, math.NaN(), false},
		{"~", 1, false},
	}
	for _, tt := range tests {
		if got := (Condition{Operator: tt.op, Threshold: 1}).Holds(tt.value); got != tt.want {
			t.Errorf("%s 1 holds for %v = %t, want %t", tt.op, tt.value, got, tt.want)
		}
	}
}

func TestBaselineBounds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		baseline  PrometheusBaseline
		reference float64
		wantBound float64
		within    []float64
		outside   []float64
	}{
		{name: "increase ratio", baseline: PrometheusBaseline{Direction: BaselineIncrease, Ratio: 0.2}, reference: 10, wantBound: 12, within: []float64{0, 10, 12}, outside: []float64{12.01, math.Inf(1), math.NaN()}},
		{name: "increase ratio and absolute", baseline: PrometheusBaseline{Direction: BaselineIncrease, Ratio: 0.5, Absolute: 1}, reference: 4, wantBound: 7, within: []float64{7}, outside: []float64{7.5}},
		{name: "increase zero baseline uses absolute", baseline: PrometheusBaseline{Direction: BaselineIncrease, Ratio: 1.2, Absolute: 0.01}, reference: 0, wantBound: 0.01, within: []float64{0, 0.01}, outside: []float64{0.02}},
		{name: "increase negative baseline", baseline: PrometheusBaseline{Direction: BaselineIncrease, Ratio: 0.5}, reference: -10, wantBound: -5, within: []float64{-6, -5}, outside: []float64{-4}},
		{name: "decrease ratio", baseline: PrometheusBaseline{Direction: BaselineDecrease, Ratio: 0.1}, reference: 100, wantBound: 90, within: []float64{90, 200, math.Inf(1)}, outside: []float64{89, math.NaN()}},
		{name: "decrease absolute", baseline: PrometheusBaseline{Direction: BaselineDecrease, Absolute: 5}, reference: 0, wantBound: -5, within: []float64{-5}, outside: []float64{-6}},
		{name: "decrease negative baseline", baseline: PrometheusBaseline{Direction: BaselineDecrease, Ratio: 0.5}, reference: -10, wantBound: -15, within: []float64{-15}, outside: []float64{-16}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.baseline.Bound(tt.reference); math.Abs(got-tt.wantBound) > 1e-9 {
				t.Errorf("Bound(%v) = %v, want %v", tt.reference, got, tt.wantBound)
			}
			for _, v := range tt.within {
				if !tt.baseline.Within(v, tt.reference) {
					t.Errorf("Within(%v, %v) = false, want true", v, tt.reference)
				}
			}
			for _, v := range tt.outside {
				if tt.baseline.Within(v, tt.reference) {
					t.Errorf("Within(%v, %v) = true, want false", v, tt.reference)
				}
			}
		})
	}
}

func TestPrometheusSecretRefs(t *testing.T) {
	t.Parallel()

	c := Catalog{Prometheus: []PrometheusServer{
		{Name: "bearer", URL: "http://a", BearerTokenEnv: "PROM_TOKEN"},
		{Name: "basic", URL: "http://b", Username: "u", PasswordFile: "/run/secrets/prom-password"},
		{Name: "open", URL: "http://c"},
	}}
	refs := c.SecretRefs()
	if len(refs) != 2 {
		t.Fatalf("SecretRefs() = %+v, want 2", refs)
	}
	if refs[0] != (SecretRef{Label: "prometheus bearer bearer_token", Env: "PROM_TOKEN"}) {
		t.Errorf("bearer ref = %+v", refs[0])
	}
	if refs[1] != (SecretRef{Label: "prometheus basic password", File: "/run/secrets/prom-password"}) {
		t.Errorf("password ref = %+v", refs[1])
	}

	err := c.CheckSecrets(envFrom(nil), filesFrom(nil))
	assertErrorContains(t, err, []string{
		"prometheus bearer bearer_token: environment variable PROM_TOKEN is not set",
		"prometheus basic password: open /run/secrets/prom-password",
	})
	err = c.CheckSecrets(envFrom(map[string]string{"PROM_TOKEN": secretValue}), filesFrom(map[string]string{"/run/secrets/prom-password": secretValue}))
	assertErrorContains(t, err, nil)

	if s, ok := c.PrometheusServer("basic"); !ok || s.Username != "u" {
		t.Errorf("PrometheusServer(basic) = %+v, %t", s, ok)
	}
	if _, ok := c.PrometheusServer("missing"); ok {
		t.Error("PrometheusServer(missing) found a server")
	}
}
