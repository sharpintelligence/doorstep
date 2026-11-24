package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sharpintelligence/doorstep/internal/store"
)

const (
	EnvDatabaseURL     = "DOORSTEP_TEST_DATABASE_URL"
	EnvRequireDatabase = "DOORSTEP_TEST_REQUIRE_DATABASE"

	cleanupTimeout = 30 * time.Second
	poolMaxConns   = "5"
)

func DatabaseURL(tb testing.TB) string {
	tb.Helper()
	base := os.Getenv(EnvDatabaseURL)
	if base == "" {
		if os.Getenv(EnvRequireDatabase) == "true" {
			tb.Fatalf("%s is unset but %s=true", EnvDatabaseURL, EnvRequireDatabase)
		}
		tb.Skipf("%s is unset; skipping Postgres integration test", EnvDatabaseURL)
	}

	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		tb.Fatalf("%s must be a postgres:// URL", EnvDatabaseURL)
	}

	schema := "t_" + randomHex(tb, 8)
	createSchema(tb, base, schema)

	query := u.Query()
	query.Set("search_path", schema)
	if !query.Has("pool_max_conns") {
		query.Set("pool_max_conns", poolMaxConns)
	}
	u.RawQuery = query.Encode()
	return u.String()
}

func Open(tb testing.TB) *store.Store {
	tb.Helper()
	databaseURL := DatabaseURL(tb)
	st, err := store.Open(tb.Context(), databaseURL)
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}
	tb.Cleanup(st.Close)
	if _, err := st.Migrate(tb.Context(), slog.New(slog.DiscardHandler)); err != nil {
		tb.Fatalf("migrate: %v", err)
	}
	return st
}

func Pool(tb testing.TB, databaseURL string) *pgxpool.Pool {
	tb.Helper()
	pool, err := pgxpool.New(tb.Context(), databaseURL)
	if err != nil {
		tb.Fatalf("open pool: %v", err)
	}
	tb.Cleanup(pool.Close)
	return pool
}

func createSchema(tb testing.TB, databaseURL, schema string) {
	tb.Helper()
	ident := pgx.Identifier{schema}.Sanitize()
	if err := execOnce(tb.Context(), databaseURL, "CREATE SCHEMA "+ident); err != nil {
		tb.Fatalf("create schema %s: %v", schema, err)
	}
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(tb.Context()), cleanupTimeout)
		defer cancel()
		if err := execOnce(ctx, databaseURL, "DROP SCHEMA "+ident+" CASCADE"); err != nil {
			tb.Errorf("drop schema %s: %v", schema, err)
		}
	})
}

func execOnce(ctx context.Context, databaseURL, sql string) error {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	_, err = conn.Exec(ctx, sql)
	return err
}

func randomHex(tb testing.TB, n int) string {
	tb.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		tb.Fatalf("random schema name: %v", err)
	}
	return hex.EncodeToString(b)
}
