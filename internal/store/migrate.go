package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"hash/fnv"
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
	migrationLockPrefix       = "doorstep-migrations:"
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

	provider, closeProvider, err := s.migrationProvider(ctx, logger, settings.sessionLock)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, closeProvider()) }()

	if err := checkNotNewer(ctx, provider); err != nil {
		return nil, err
	}
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
	return applied, nil
}

func (s *Store) MigrationStatus(ctx context.Context) (states []MigrationState, err error) {
	provider, closeProvider, err := s.migrationProvider(ctx, slog.New(slog.DiscardHandler), false)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, closeProvider()) }()

	if err := checkNotNewer(ctx, provider); err != nil {
		return nil, err
	}
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

func (s *Store) migrationProvider(ctx context.Context, logger *slog.Logger, sessionLock bool) (*goose.Provider, func() error, error) {
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
		lockID, err := s.migrationLockID(ctx)
		if err != nil {
			return nil, nil, err
		}
		locker, err := lock.NewPostgresSessionLocker(
			lock.WithLockID(lockID),
			lock.WithLockTimeout(migrationLockRetrySeconds, migrationLockMaxRetries),
		)
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

func (s *Store) migrationLockID(ctx context.Context) (int64, error) {
	var schema *string
	if err := s.pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		return 0, fmt.Errorf("read current schema: %w", err)
	}
	if schema == nil {
		return 0, errors.New("migrate: no current schema: the search_path names no existing schema")
	}
	return MigrationLockID(*schema), nil
}

func MigrationLockID(schema string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(migrationLockPrefix + schema))
	return int64(h.Sum32())
}

func checkNotNewer(ctx context.Context, provider *goose.Provider) error {
	current, err := provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if latest := latestVersion(provider); current > latest {
		return fmt.Errorf("%w: database is at version %d, newest known migration is %d", ErrDatabaseNewer, current, latest)
	}
	return nil
}

func latestVersion(provider *goose.Provider) int64 {
	var latest int64
	for _, src := range provider.ListSources() {
		latest = max(latest, src.Version)
	}
	return latest
}
