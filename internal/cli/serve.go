package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/spf13/cobra"

	"github.com/sharpintelligence/doorstep/internal/buildinfo"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/logging"
	"github.com/sharpintelligence/doorstep/internal/server"
)

func newServeCommand(lookupEnv LookupEnv) *cobra.Command {
	defaults := config.Default()
	var overrides config.Config
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the doorstep server",
		Long:  "Run the doorstep server. Pending database migrations are applied before the listener opens.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveServeConfig(cmd.Flags().Changed, overrides, lookupEnv)
			if err != nil {
				return err
			}
			return serve(cmd.Context(), cmd.ErrOrStderr(), cfg)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&overrides.Server.ListenAddr, flagListenAddr, defaults.Server.ListenAddr,
		"address to listen on as host:port (env "+config.EnvListenAddr+")")
	flags.DurationVar(&overrides.Server.ShutdownDelay, flagShutdownDelay, defaults.Server.ShutdownDelay,
		"time to keep serving while reporting not ready before draining on shutdown (env "+config.EnvShutdownDelay+")")
	flags.DurationVar(&overrides.Server.ShutdownTimeout, flagShutdownTimeout, defaults.Server.ShutdownTimeout,
		"time allowed for in-flight requests on shutdown (env "+config.EnvShutdownTimeout+")")
	bindLogFlags(flags, &overrides)
	bindDatabaseFlags(flags, &overrides)
	return cmd
}

func resolveServeConfig(changed func(name string) bool, overrides config.Config, lookupEnv LookupEnv) (config.Config, error) {
	cfg, err := resolveConfig(changed, overrides, lookupEnv)
	if err != nil {
		return config.Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

func serve(ctx context.Context, logOutput io.Writer, cfg config.Config) error {
	logger, err := logging.New(logOutput, cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		return err
	}

	st, err := openStore(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer st.Close()
	applied, err := st.Migrate(ctx, logger)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	logger.InfoContext(ctx, "database ready", slog.Any("applied_migrations", migrationVersions(applied)))

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Server.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Server.ListenAddr, err)
	}
	logger.InfoContext(ctx, "listening",
		slog.String("addr", ln.Addr().String()),
		slog.Any("build", buildinfo.Read()),
	)

	srv := server.New(server.Options{
		Logger:          logger,
		ShutdownDelay:   cfg.Server.ShutdownDelay,
		ShutdownTimeout: cfg.Server.ShutdownTimeout,
		Checks:          map[string]server.Check{"postgres": st.Ping},
	})
	if err := srv.Run(ctx, ln); err != nil {
		return err
	}
	logger.InfoContext(ctx, "stopped")
	return nil
}
