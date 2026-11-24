package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"sync"

	"github.com/spf13/cobra"

	"github.com/sharpintelligence/doorstep/internal/api"
	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/buildinfo"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/locks"
	"github.com/sharpintelligence/doorstep/internal/logging"
	"github.com/sharpintelligence/doorstep/internal/metrics"
	"github.com/sharpintelligence/doorstep/internal/queue"
	"github.com/sharpintelligence/doorstep/internal/registry"
	"github.com/sharpintelligence/doorstep/internal/rollback"
	"github.com/sharpintelligence/doorstep/internal/server"
	"github.com/sharpintelligence/doorstep/internal/store"
)

func newServeCommand(lookupEnv LookupEnv) *cobra.Command {
	defaults := config.Default()
	var overrides config.Config
	var configFlag string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the doorstep server",
		Long:  "Run the doorstep server. Pending database migrations are applied before the listener opens.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := resolveConfigPath(cmd.Flags().Changed, configFlag, lookupEnv)
			if err != nil {
				return err
			}
			cfg, catalog, err := resolveServeConfig(cmd.Flags().Changed, overrides, path, lookupEnv)
			if err != nil {
				return err
			}
			return serve(cmd.Context(), cmd.ErrOrStderr(), serveInput{
				config:     cfg,
				catalog:    catalog,
				configPath: path,
				lookupEnv:  lookupEnv,
			})
		},
	}

	addConfigFlag(cmd.Flags(), &configFlag)
	flags := cmd.Flags()
	flags.StringVar(&overrides.Server.ListenAddr, flagListenAddr, defaults.Server.ListenAddr,
		"address to listen on as host:port (env "+config.EnvListenAddr+")")
	flags.StringVar(&overrides.Server.MetricsListenAddr, flagMetricsListenAddr, defaults.Server.MetricsListenAddr,
		"address for the /metrics listener as host:port; empty serves /metrics on the main listener instead (env "+config.EnvMetricsListenAddr+")")
	flags.DurationVar(&overrides.Server.ShutdownDelay, flagShutdownDelay, defaults.Server.ShutdownDelay,
		"time to keep serving while reporting not ready before draining on shutdown (env "+config.EnvShutdownDelay+")")
	flags.DurationVar(&overrides.Server.ShutdownTimeout, flagShutdownTimeout, defaults.Server.ShutdownTimeout,
		"time allowed for in-flight requests on shutdown (env "+config.EnvShutdownTimeout+")")
	flags.StringVar(&overrides.Server.PublicURL, flagPublicURL, defaults.Server.PublicURL,
		"external base URL of doorstep, used for links in notifications and GitHub deployment statuses, and required for sign-in (https unless loopback); links are left out when empty (env "+config.EnvPublicURL+")")
	bindLogFlags(flags, &overrides)
	bindDatabaseFlags(flags, &overrides)
	addRegistryCredentialsFlag(flags, &overrides.Registry.CredentialsFile)
	return cmd
}

func resolveServeConfig(changed func(name string) bool, overrides config.Config, configPath string, lookupEnv LookupEnv) (config.Config, config.Catalog, error) {
	file, err := loadConfigFileAt(configPath)
	if err != nil {
		return config.Config{}, config.Catalog{}, err
	}
	cfg, err := resolveConfig(changed, overrides, file, lookupEnv)
	if err != nil {
		return config.Config{}, config.Catalog{}, err
	}
	if changed(flagRegistryAuthFile) {
		cfg.Registry.CredentialsFile = overrides.Registry.CredentialsFile
	}
	catalog := file.Catalog()
	if err := errors.Join(cfg.Validate(), catalog.Validate(), catalog.ValidateWith(cfg)); err != nil {
		return config.Config{}, config.Catalog{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, catalog, nil
}

type serveInput struct {
	config     config.Config
	catalog    config.Catalog
	configPath string
	lookupEnv  LookupEnv
}

func serve(ctx context.Context, logOutput io.Writer, in serveInput) error {
	cfg := in.config
	logger, err := logging.New(logOutput, cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		return err
	}
	if err := in.catalog.CheckSecrets(in.lookupEnv, config.ReadSecretFile); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	if in.configPath != "" {
		logger.InfoContext(ctx, "configuration loaded",
			slog.String("path", in.configPath),
			slog.Int("apps", len(in.catalog.Apps)),
			slog.Int("notifications", len(in.catalog.Notifications)),
		)
	}
	if len(in.catalog.Apps) == 0 {
		logger.WarnContext(ctx, "no apps configured")
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

	apps := newCatalogApps(in.catalog)
	q := queue.New(queue.Options{Store: st, MaxBatch: apps.MaxBatch, Logger: logger})
	locksService, reminder, err := newLocks(st, cfg.Locks, logger)
	if err != nil {
		return err
	}
	authService, err := newAuthService(cfg, in.catalog.Auth, auth.NewRepository(st), in.lookupEnv, logger)
	if err != nil {
		return fmt.Errorf("authentication: %w", err)
	}
	var background sync.WaitGroup
	defer background.Wait()
	probeCtx, stopProbe := context.WithCancel(ctx)
	defer stopProbe()
	probeAuth(probeCtx, &background, logger, authService)

	build := buildinfo.Read()
	m := metrics.New(metrics.Options{Logger: logger, Build: build, QueueDepth: q.Depth})
	rollbacks, err := rollback.New(rollback.Options{
		Repository: rollback.NewRepository(st),
		Observer:   rollbackMetrics{recorder: m.Recorder()},
		Logger:     logger,
	})
	if err != nil {
		return err
	}

	intakeWiring, err := newIntake(ctx, intakeInput{
		config:    cfg,
		catalog:   in.catalog,
		githubApp: githubApp,
		store:     st,
		maxBatch:  apps.MaxBatch,
		readFile:  config.ReadSecretFile,
		recorder:  m.Recorder(),
		logger:    logger,
	})
	if err != nil {
		return fmt.Errorf("intake: %w", err)
	}

	authenticator := authService.Authenticator()
	apiHandler := api.New(api.Options{
		Logger:        logger,
		History:       st,
		Queue:         q,
		Apps:          apps,
		Locks:         locksService,
		Rollbacks:     rollbacks,
		Blocks:        rollbacks,
		KnownApp:      knownApp(in.catalog),
		Authenticator: principalAuthenticator{},
		Middleware:    authenticator.RequireRole(core.RoleViewer),
	})

	metricsRoutes := map[string]http.Handler{"GET /metrics": m.Handler()}
	appRoutes := map[string]http.Handler{api.Prefix: apiHandler}
	if h := authService.Handler(); h != nil {
		maps.Copy(appRoutes, h.Routes())
	}
	mainRoutes := wrapRoutes(appRoutes, authenticator.Middleware)
	maps.Copy(mainRoutes, intakeWiring.routes)
	checks := map[string]server.Check{"postgres": st.Ping}
	newServer := func(routes map[string]http.Handler) *server.Server {
		return server.New(server.Options{
			Checks:          checks,
			Logger:          logger,
			ShutdownDelay:   cfg.Server.ShutdownDelay,
			ShutdownTimeout: cfg.Server.ShutdownTimeout,
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

	runs := []func(context.Context) error{
		func(ctx context.Context) error { return reminder.Run(ctx, locks.SweepInterval) },
	}
	runs = append(runs, intakeWiring.runs...)
	if cfg.Server.MetricsListenAddr == "" {
		maps.Copy(mainRoutes, metricsRoutes)
	} else {
		metricsLn, err := lc.Listen(ctx, "tcp", cfg.Server.MetricsListenAddr)
		if err != nil {
			return errors.Join(fmt.Errorf("listen on %s: %w", cfg.Server.MetricsListenAddr, err), ln.Close())
		}
		logger.InfoContext(ctx, "metrics listening", slog.String("addr", metricsLn.Addr().String()))
		metricsServer := newServer(metricsRoutes)
		runs = append(runs, func(ctx context.Context) error { return metricsServer.Run(ctx, metricsLn) })
	}
	mainServer := newServer(mainRoutes)
	runs = append(runs, func(ctx context.Context) error { return mainServer.Run(ctx, ln) })

	if err := runServers(ctx, runs...); err != nil {
		return err
	}
	logger.InfoContext(ctx, "stopped")
	return nil
}

func newLocks(st *store.Store, cfg config.Locks, logger *slog.Logger) (*locks.Service, *locks.Reminder, error) {
	service, err := locks.New(locks.Options{Repository: st, Logger: logger})
	if err != nil {
		return nil, nil, err
	}
	reminder, err := locks.NewReminder(locks.ReminderOptions{Repository: st, Logger: logger, Every: cfg.ReminderInterval})
	if err != nil {
		return nil, nil, err
	}
	return service, reminder, nil
}

type principalAuthenticator struct{}

func (principalAuthenticator) Authenticate(r *http.Request, required core.Role) (api.Caller, error) {
	principal, ok := auth.PrincipalFrom(r.Context())
	switch {
	case !ok:
		return api.Caller{}, api.ErrUnauthenticated
	case !principal.Allows(required):
		return api.Caller{}, api.ErrForbidden
	}
	return api.Caller{Actor: principal.Actor, Role: principal.Role}, nil
}

type rollbackMetrics struct {
	recorder *metrics.Recorder
}

func (o rollbackMetrics) RollbackRequested(app string) {
	o.recorder.Rollback(app, metrics.RollbackReasonManual)
}

func knownApp(catalog config.Catalog) api.KnownApp {
	return func(app string) bool {
		_, ok := catalog.App(app)
		return ok
	}
}

func wrapRoutes(routes map[string]http.Handler, middleware func(http.Handler) http.Handler) map[string]http.Handler {
	wrapped := make(map[string]http.Handler, len(routes))
	for pattern, handler := range routes {
		wrapped[pattern] = middleware(handler)
	}
	return wrapped
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
