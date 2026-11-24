package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/sharpintelligence/doorstep/internal/logging"
)

const (
	EnvListenAddr        = "DOORSTEP_LISTEN_ADDR"
	EnvMetricsListenAddr = "DOORSTEP_METRICS_LISTEN_ADDR"
	EnvShutdownTimeout   = "DOORSTEP_SHUTDOWN_TIMEOUT"
	EnvShutdownDelay     = "DOORSTEP_SHUTDOWN_DELAY"
	EnvLogLevel          = "DOORSTEP_LOG_LEVEL"
	EnvLogFormat         = "DOORSTEP_LOG_FORMAT"
	EnvDatabaseURL       = "DOORSTEP_DATABASE_URL"
	EnvDatabaseURLFile   = "DOORSTEP_DATABASE_URL_FILE"

	EnvRegistryCredentialsFile = "DOORSTEP_REGISTRY_CREDENTIALS_FILE"
)

var ErrDatabaseSource = errors.New("set exactly one of " + EnvDatabaseURL + " and " + EnvDatabaseURLFile)

type Config struct {
	Server   Server
	Log      Log
	Database Database
	GitHub   GitHub
	Registry Registry
}

type Server struct {
	ListenAddr        string
	MetricsListenAddr string
	ShutdownDelay     time.Duration
	ShutdownTimeout   time.Duration
}

type Log struct {
	Level  string
	Format string
}

type Database struct {
	URL     string
	URLFile string
}

type Registry struct {
	CredentialsFile string
}

func Default() Config {
	return Config{
		Server: Server{
			ListenAddr:        ":8080",
			MetricsListenAddr: ":9090",
			ShutdownDelay:     5 * time.Second,
			ShutdownTimeout:   15 * time.Second,
		},
		Log: Log{
			Level:  "info",
			Format: logging.FormatJSON,
		},
		GitHub: defaultGitHub(),
	}
}

func FromEnv(base Config, lookup func(string) (string, bool)) (Config, error) {
	cfg := base
	var errs []error
	if v, ok := lookup(EnvListenAddr); ok {
		cfg.Server.ListenAddr = v
	}
	if v, ok := lookup(EnvMetricsListenAddr); ok {
		cfg.Server.MetricsListenAddr = v
	}
	errs = append(errs,
		lookupDuration(lookup, EnvShutdownDelay, &cfg.Server.ShutdownDelay),
		lookupDuration(lookup, EnvShutdownTimeout, &cfg.Server.ShutdownTimeout),
	)
	if v, ok := lookup(EnvLogLevel); ok {
		cfg.Log.Level = v
	}
	if v, ok := lookup(EnvLogFormat); ok {
		cfg.Log.Format = v
	}
	if v, ok := lookup(EnvDatabaseURL); ok {
		cfg.Database.URL = v
	}
	if v, ok := lookup(EnvDatabaseURLFile); ok {
		cfg.Database.URLFile = v
	}
	cfg.GitHub = githubFromEnv(cfg.GitHub, lookup)
	if v, ok := lookup(EnvRegistryCredentialsFile); ok {
		cfg.Registry.CredentialsFile = v
	}
	return cfg, errors.Join(errs...)
}

func lookupDuration(lookup func(string) (string, bool), key string, dst *time.Duration) error {
	v, ok := lookup(key)
	if !ok {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	*dst = d
	return nil
}

func (c Config) Validate() error {
	var errs []error
	if err := validateListenAddr(c.Server.ListenAddr); err != nil {
		errs = append(errs, fmt.Errorf("listen address: %w", err))
	}
	if c.Server.MetricsListenAddr != "" {
		if err := validateListenAddr(c.Server.MetricsListenAddr); err != nil {
			errs = append(errs, fmt.Errorf("metrics listen address: %w", err))
		} else if sharesFixedPort(c.Server.ListenAddr, c.Server.MetricsListenAddr) {
			errs = append(errs, fmt.Errorf("metrics listen address: %q must differ from the listen address; leave it empty to serve /metrics on the main listener", c.Server.MetricsListenAddr))
		}
	}
	if c.Server.ShutdownDelay < 0 {
		errs = append(errs, fmt.Errorf("shutdown delay: must not be negative, got %s", c.Server.ShutdownDelay))
	}
	if c.Server.ShutdownTimeout <= 0 {
		errs = append(errs, fmt.Errorf("shutdown timeout: must be positive, got %s", c.Server.ShutdownTimeout))
	}
	errs = append(errs, c.Log.Validate())
	if err := c.Database.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("database: %w", err))
	}
	errs = append(errs, c.GitHub.validate()...)
	return errors.Join(errs...)
}

func (l Log) Validate() error {
	var errs []error
	if _, err := logging.ParseLevel(l.Level); err != nil {
		errs = append(errs, fmt.Errorf("log level: %w", err))
	}
	if _, err := logging.ParseFormat(l.Format); err != nil {
		errs = append(errs, fmt.Errorf("log format: %w", err))
	}
	return errors.Join(errs...)
}

func (d Database) Validate() error {
	if (d.URL == "") == (d.URLFile == "") {
		return ErrDatabaseSource
	}
	return nil
}

func (d Database) ConnString(readFile func(string) ([]byte, error)) (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	if d.URL != "" {
		return d.URL, nil
	}
	content, err := readFile(d.URLFile)
	if err != nil {
		return "", fmt.Errorf("read database URL file: %w", err)
	}
	connString := strings.TrimSpace(string(content))
	if connString == "" {
		return "", fmt.Errorf("database URL file %s is empty", d.URLFile)
	}
	return connString, nil
}

func validateListenAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("port %q must be a number between 0 and 65535", port)
	}
	return nil
}

func sharesFixedPort(a, b string) bool {
	hostA, portA, errA := net.SplitHostPort(a)
	hostB, portB, errB := net.SplitHostPort(b)
	if errA != nil || errB != nil {
		return false
	}
	if portA == "0" || portB == "0" {
		return false
	}
	if portA != portB {
		return false
	}
	return hostA == hostB || isWildcardHost(hostA) || isWildcardHost(hostB)
}

func isWildcardHost(host string) bool {
	switch host {
	case "", "0.0.0.0", "::":
		return true
	default:
		return false
	}
}
