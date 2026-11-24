package storetest_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func TestDatabaseURLIsolatesSchemas(t *testing.T) {
	t.Parallel()

	first := storetest.DatabaseURL(t)
	second := storetest.DatabaseURL(t)

	firstSchema := searchPath(t, first)
	secondSchema := searchPath(t, second)
	if firstSchema == secondSchema {
		t.Fatalf("both URLs use schema %q", firstSchema)
	}

	exec(t, first, "CREATE TABLE only_here (id int)")

	conn := connect(t, second)
	var visible bool
	if err := conn.QueryRow(t.Context(), "SELECT to_regclass('only_here') IS NOT NULL").Scan(&visible); err != nil {
		t.Fatalf("query: %v", err)
	}
	if visible {
		t.Error("table created in the first schema is visible in the second")
	}
	var current string
	if err := conn.QueryRow(t.Context(), "SELECT current_schema()").Scan(&current); err != nil {
		t.Fatalf("query current_schema: %v", err)
	}
	if current != secondSchema {
		t.Errorf("current_schema() = %q, want %q", current, secondSchema)
	}
}

func TestOpenReturnsMigratedStore(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	status, ahead, err := st.MigrationStatus(t.Context())
	if err != nil {
		t.Fatalf("MigrationStatus: %v", err)
	}
	if ahead != nil {
		t.Errorf("MigrationStatus reports a newer database: %+v", ahead)
	}
	for _, m := range status {
		if !m.Applied {
			t.Errorf("migration %d not applied", m.Version)
		}
	}
	if err := st.Ping(t.Context()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func searchPath(t *testing.T, databaseURL string) string {
	t.Helper()
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	schema := u.Query().Get("search_path")
	if schema == "" {
		t.Fatal("URL has no search_path")
	}
	if u.Query().Get("pool_max_conns") == "" {
		t.Error("URL has no pool_max_conns")
	}
	return schema
}

func connect(t *testing.T, databaseURL string) *pgx.Conn {
	t.Helper()
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	query := u.Query()
	query.Del("pool_max_conns")
	u.RawQuery = query.Encode()
	conn, err := pgx.Connect(t.Context(), u.String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	return conn
}

func exec(t *testing.T, databaseURL, sql string) {
	t.Helper()
	if _, err := connect(t, databaseURL).Exec(t.Context(), sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func TestPoolUsesTheSchemaOfTheURL(t *testing.T) {
	t.Parallel()

	databaseURL := storetest.DatabaseURL(t)
	pool := storetest.Pool(t, databaseURL)
	var current string
	if err := pool.QueryRow(t.Context(), "SELECT current_schema()").Scan(&current); err != nil {
		t.Fatalf("query current_schema: %v", err)
	}
	if want := searchPath(t, databaseURL); current != want {
		t.Errorf("current_schema() = %q, want %q", current, want)
	}
}
