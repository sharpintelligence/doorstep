package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

const (
	migrationLockRetrySeconds = 1
	migrationLockMaxRetries   = 120
)

var ErrDatabaseNewer = errors.New("database schema is newer than this doorstep binary")

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

type AppliedMigration struct {
	Version  int64
	Name     string
	Duration time.Duration
}

type MigrationState struct {
	Version   int64
	Name      string
	Applied   bool
	AppliedAt time.Time
}

type migrateOptions struct {
	sessionLock bool
}

type MigrateOption func(*migrateOptions)

func WithoutSessionLock() MigrateOption {
	return func(o *migrateOptions) { o.sessionLock = false }
}

func (s *Store) Migrate(ctx context.Context, logger *slog.Logger, opts ...MigrateOption) (applied []AppliedMigration, err error) {
	settings := migrateOptions{sessionLock: true}
	for _, opt := range opts {
		opt(&settings)
	}

	provider, closeProvider, err := s.migrationProvider(logger, settings.sessionLock)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, closeProvider()) }()

	results, err := provider.Up(ctx)
	if err != nil {
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	for _, r := range results {
		applied = append(applied, AppliedMigration{
			Version:  r.Source.Version,
			Name:     path.Base(r.Source.Path),
			Duration: r.Duration,
		})
	}

	current, err := provider.GetDBVersion(ctx)
	if err != nil {
		return applied, fmt.Errorf("read schema version: %w", err)
	}
	if latest := latestVersion(provider); current > latest {
		return applied, fmt.Errorf("%w: database is at version %d, newest known migration is %d", ErrDatabaseNewer, current, latest)
	}
	return applied, nil
}

func (s *Store) MigrationStatus(ctx context.Context) (states []MigrationState, err error) {
	provider, closeProvider, err := s.migrationProvider(slog.New(slog.DiscardHandler), false)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, closeProvider()) }()

	statuses, err := provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("read migration status: %w", err)
	}
	for _, st := range statuses {
		states = append(states, MigrationState{
			Version:   st.Source.Version,
			Name:      path.Base(st.Source.Path),
			Applied:   st.State == goose.StateApplied,
			AppliedAt: st.AppliedAt,
		})
	}
	return states, nil
}

func (s *Store) migrationProvider(logger *slog.Logger, sessionLock bool) (*goose.Provider, func() error, error) {
	if s.pool == nil {
		return nil, nil, fmt.Errorf("migrate: %w", errInTransaction)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	migrations, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		return nil, nil, fmt.Errorf("load migrations: %w", err)
	}
	options := []goose.ProviderOption{
		goose.WithDisableGlobalRegistry(true),
		goose.WithSlog(logger),
	}
	if sessionLock {
		locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(migrationLockRetrySeconds, migrationLockMaxRetries))
		if err != nil {
			return nil, nil, fmt.Errorf("create migration lock: %w", err)
		}
		options = append(options, goose.WithSessionLocker(locker))
	}

	db := stdlib.OpenDBFromPool(s.pool)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations, options...)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("create migration provider: %w", err), db.Close())
	}
	return provider, provider.Close, nil
}

func latestVersion(provider *goose.Provider) int64 {
	var latest int64
	for _, src := range provider.ListSources() {
		latest = max(latest, src.Version)
	}
	return latest
}
