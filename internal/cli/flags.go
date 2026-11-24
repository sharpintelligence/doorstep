package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/pflag"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const (
	flagListenAddr        = "listen-addr"
	flagMetricsListenAddr = "metrics-listen-addr"
	flagShutdownDelay     = "shutdown-delay"
	flagShutdownTimeout   = "shutdown-timeout"
	flagPublicURL         = "public-url"
	flagLogLevel          = "log-level"
	flagLogFormat         = "log-format"
	flagDatabaseURLFile   = "database-url-file"

	flagRegistryAuthFile = "registry-credentials-file"
)

func bindLogFlags(flags *pflag.FlagSet, overrides *config.Config) {
	defaults := config.Default()
	flags.StringVar(&overrides.Log.Level, flagLogLevel, defaults.Log.Level,
		"log level: debug, info, warn or error (env "+config.EnvLogLevel+")")
	flags.StringVar(&overrides.Log.Format, flagLogFormat, defaults.Log.Format,
		"log format: json or text (env "+config.EnvLogFormat+")")
}

func bindDatabaseFlags(flags *pflag.FlagSet, overrides *config.Config) {
	flags.StringVar(&overrides.Database.URLFile, flagDatabaseURLFile, "",
		"file holding the Postgres connection URL (env "+config.EnvDatabaseURLFile+", or the URL itself in "+config.EnvDatabaseURL+")")
}

func resolveConfig(changed func(name string) bool, overrides config.Config, file config.File, lookupEnv LookupEnv) (config.Config, error) {
	cfg, err := config.FromEnv(file.Apply(config.Default()), lookupEnv)
	if err != nil {
		return config.Config{}, fmt.Errorf("invalid environment: %w", err)
	}
	if changed(flagListenAddr) {
		cfg.Server.ListenAddr = overrides.Server.ListenAddr
	}
	if changed(flagMetricsListenAddr) {
		cfg.Server.MetricsListenAddr = overrides.Server.MetricsListenAddr
	}
	if changed(flagShutdownDelay) {
		cfg.Server.ShutdownDelay = overrides.Server.ShutdownDelay
	}
	if changed(flagShutdownTimeout) {
		cfg.Server.ShutdownTimeout = overrides.Server.ShutdownTimeout
	}
	if changed(flagPublicURL) {
		cfg.Server.PublicURL = overrides.Server.PublicURL
	}
	if changed(flagLogLevel) {
		cfg.Log.Level = overrides.Log.Level
	}
	if changed(flagLogFormat) {
		cfg.Log.Format = overrides.Log.Format
	}
	if changed(flagDatabaseURLFile) {
		cfg.Database = config.Database{URLFile: overrides.Database.URLFile}
	}
	return cfg, nil
}

func openStore(ctx context.Context, db config.Database) (*store.Store, error) {
	connString, err := db.ConnString(os.ReadFile)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return st, nil
}
