package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/sharpintelligence/doorstep/internal/logging"
)

const (
	EnvListenAddr      = "DOORSTEP_LISTEN_ADDR"
	EnvShutdownTimeout = "DOORSTEP_SHUTDOWN_TIMEOUT"
	EnvShutdownDelay   = "DOORSTEP_SHUTDOWN_DELAY"
	EnvLogLevel        = "DOORSTEP_LOG_LEVEL"
	EnvLogFormat       = "DOORSTEP_LOG_FORMAT"
)

type Config struct {
	Server Server
	Log    Log
}

type Server struct {
	ListenAddr      string
	ShutdownDelay   time.Duration
	ShutdownTimeout time.Duration
}

type Log struct {
	Level  string
	Format string
}

func Default() Config {
	return Config{
		Server: Server{
			ListenAddr:      ":8080",
			ShutdownDelay:   5 * time.Second,
			ShutdownTimeout: 15 * time.Second,
		},
		Log: Log{
			Level:  "info",
			Format: logging.FormatJSON,
		},
	}
}

func FromEnv(base Config, lookup func(string) (string, bool)) (Config, error) {
	cfg := base
	var errs []error
	if v, ok := lookup(EnvListenAddr); ok {
		cfg.Server.ListenAddr = v
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
	if c.Server.ShutdownDelay < 0 {
		errs = append(errs, fmt.Errorf("shutdown delay: must not be negative, got %s", c.Server.ShutdownDelay))
	}
	if c.Server.ShutdownTimeout <= 0 {
		errs = append(errs, fmt.Errorf("shutdown timeout: must be positive, got %s", c.Server.ShutdownTimeout))
	}
	if _, err := logging.ParseLevel(c.Log.Level); err != nil {
		errs = append(errs, fmt.Errorf("log level: %w", err))
	}
	if _, err := logging.ParseFormat(c.Log.Format); err != nil {
		errs = append(errs, fmt.Errorf("log format: %w", err))
	}
	return errors.Join(errs...)
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
