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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

const (
	migrationLockRetrySeconds = 1
	migrationLockMaxRetries   = 120
	migrationLockPrefix       = "doorstep-migrations:"
	undefinedTable            = "42P01"
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

type SchemaAhead struct {
	DatabaseVersion int64
	BinaryVersion   int64
	MinBinarySchema int64
}

type migrateOptions struct {
	sessionLock bool
}

type MigrateOption func(*migrateOptions)

func WithoutSessionLock() MigrateOption {
	return func(o *migrateOptions) { o.sessionLock = false }
}

func (s *Store) Migrate(ctx context.Context, logger *slog.Logger, opts ...MigrateOption) (applied []AppliedMigration, err error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	settings := migrateOptions{sessionLock: true}
	for _, opt := range opts {
		opt(&settings)
	}

	provider, closeProvider, err := s.migrationProvider(ctx, logger, settings.sessionLock)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, closeProvider()) }()

	ahead, err := s.schemaAhead(ctx, provider)
	if err != nil {
		return nil, err
	}
	if ahead != nil {
		logger.WarnContext(ctx, "database schema is newer than this binary; running in one-release compatibility mode",
			slog.Int64("database_version", ahead.DatabaseVersion),
			slog.Int64("binary_version", ahead.BinaryVersion),
			slog.Int64("min_binary_schema", ahead.MinBinarySchema))
		return nil, nil
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

func (s *Store) MigrationStatus(ctx context.Context) (states []MigrationState, ahead *SchemaAhead, err error) {
	provider, closeProvider, err := s.migrationProvider(ctx, slog.New(slog.DiscardHandler), false)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeProvider()) }()

	ahead, err = s.schemaAhead(ctx, provider)
	if err != nil {
		return nil, nil, err
	}
	statuses, err := provider.Status(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read migration status: %w", err)
	}
	for _, st := range statuses {
		states = append(states, MigrationState{
			Version:   st.Source.Version,
			Name:      path.Base(st.Source.Path),
			Applied:   st.State == goose.StateApplied,
			AppliedAt: st.AppliedAt,
		})
	}
	return states, ahead, nil
}

func (s *Store) migrationProvider(ctx context.Context, logger *slog.Logger, sessionLock bool) (*goose.Provider, func() error, error) {
	if s.pool == nil {
		return nil, nil, fmt.Errorf("migrate: %w", errInTransaction)
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

func (s *Store) schemaAhead(ctx context.Context, provider *goose.Provider) (*SchemaAhead, error) {
	current, err := provider.GetDBVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("read schema version: %w", err)
	}
	latest := latestVersion(provider)
	if current <= latest {
		return nil, nil
	}
	var floor int64
	err = s.pool.QueryRow(ctx, "SELECT min_binary_schema FROM schema_compatibility").Scan(&floor)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); (ok && pgErr.Code == undefinedTable) || errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: database is at version %d, newest known migration is %d, and the database records no compatibility floor",
			ErrDatabaseNewer, current, latest)
	}
	if err != nil {
		return nil, fmt.Errorf("read schema compatibility floor: %w", err)
	}
	if latest < floor {
		return nil, fmt.Errorf("%w: database is at version %d, newest known migration is %d, and the database requires binaries that know migration %d or newer",
			ErrDatabaseNewer, current, latest, floor)
	}
	return &SchemaAhead{DatabaseVersion: current, BinaryVersion: latest, MinBinarySchema: floor}, nil
}

func latestVersion(provider *goose.Provider) int64 {
	var latest int64
	for _, src := range provider.ListSources() {
		latest = max(latest, src.Version)
	}
	return latest
}
