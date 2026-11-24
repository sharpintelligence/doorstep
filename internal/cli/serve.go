package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"

	"github.com/spf13/cobra"

	"github.com/sharpintelligence/doorstep/internal/buildinfo"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/logging"
	"github.com/sharpintelligence/doorstep/internal/metrics"
	"github.com/sharpintelligence/doorstep/internal/registry"
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
	flags.StringVar(&overrides.Server.MetricsListenAddr, flagMetricsListenAddr, defaults.Server.MetricsListenAddr,
		"address for a separate /metrics listener as host:port; empty serves /metrics on the main listener (env "+config.EnvMetricsListenAddr+")")
	flags.DurationVar(&overrides.Server.ShutdownDelay, flagShutdownDelay, defaults.Server.ShutdownDelay,
		"time to keep serving while reporting not ready before draining on shutdown (env "+config.EnvShutdownDelay+")")
	flags.DurationVar(&overrides.Server.ShutdownTimeout, flagShutdownTimeout, defaults.Server.ShutdownTimeout,
		"time allowed for in-flight requests on shutdown (env "+config.EnvShutdownTimeout+")")
	bindLogFlags(flags, &overrides)
	bindDatabaseFlags(flags, &overrides)
	addRegistryCredentialsFlag(flags, &overrides.Registry.CredentialsFile)
	return cmd
}

func resolveServeConfig(changed func(name string) bool, overrides config.Config, lookupEnv LookupEnv) (config.Config, error) {
	cfg, err := resolveConfig(changed, overrides, lookupEnv)
	if err != nil {
		return config.Config{}, err
	}
	if changed(flagRegistryAuthFile) {
		cfg.Registry.CredentialsFile = overrides.Registry.CredentialsFile
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
	githubApp, err := newGitHubApp(cfg.GitHub, os.ReadFile, logger)
	if err != nil {
		return err
	}
	logGitHubApp(ctx, logger, cfg.GitHub, githubApp)

	if cfg.Registry.CredentialsFile != "" {
		creds, err := registry.LoadDockerConfigFile(cfg.Registry.CredentialsFile)
		if err != nil {
			return fmt.Errorf("registry credentials: %w", err)
		}
		logger.InfoContext(ctx, "registry credentials loaded", slog.Int("entries", creds.Len()))
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

	build := buildinfo.Read()
	m := metrics.New(metrics.Options{Logger: logger, Build: build})
	routes := map[string]http.Handler{"GET /metrics": m.Handler()}
	newServer := func(routes map[string]http.Handler) *server.Server {
		return server.New(server.Options{
			Logger:          logger,
			ShutdownDelay:   cfg.Server.ShutdownDelay,
			ShutdownTimeout: cfg.Server.ShutdownTimeout,
			Checks:          map[string]server.Check{"postgres": st.Ping},
			Routes:          routes,
		})
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Server.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Server.ListenAddr, err)
	}
	logger.InfoContext(ctx, "listening",
		slog.String("addr", ln.Addr().String()),
		slog.Any("build", build),
	)

	if cfg.Server.MetricsListenAddr == "" {
		if err := newServer(routes).Run(ctx, ln); err != nil {
			return err
		}
		logger.InfoContext(ctx, "stopped")
		return nil
	}

	metricsLn, err := lc.Listen(ctx, "tcp", cfg.Server.MetricsListenAddr)
	if err != nil {
		return errors.Join(fmt.Errorf("listen on %s: %w", cfg.Server.MetricsListenAddr, err), ln.Close())
	}
	logger.InfoContext(ctx, "metrics listening", slog.String("addr", metricsLn.Addr().String()))

	mainServer := newServer(nil)
	metricsServer := newServer(routes)
	err = runServers(ctx,
		func(ctx context.Context) error { return mainServer.Run(ctx, ln) },
		func(ctx context.Context) error { return metricsServer.Run(ctx, metricsLn) },
	)
	if err != nil {
		return err
	}
	logger.InfoContext(ctx, "stopped")
	return nil
}

func runServers(ctx context.Context, runs ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make([]error, len(runs))
	var wg sync.WaitGroup
	for i, run := range runs {
		wg.Go(func() {
			defer cancel()
			errs[i] = run(ctx)
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
