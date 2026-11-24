package store_test

import (
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func openUnmigrated(t *testing.T, databaseURL string) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestMigrateFreshSchema(t *testing.T) {
	t.Parallel()

	st := openUnmigrated(t, storetest.DatabaseURL(t))

	pending, err := st.MigrationStatus(t.Context())
	if err != nil {
		t.Fatalf("MigrationStatus: %v", err)
	}
	if len(pending) == 0 {
		t.Fatal("MigrationStatus lists no migrations")
	}
	for _, m := range pending {
		if m.Applied || !m.AppliedAt.IsZero() {
			t.Errorf("migration %d is applied before Migrate: %+v", m.Version, m)
		}
	}

	applied, err := st.Migrate(t.Context(), discard(), store.WithoutSessionLock())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(applied) != len(pending) {
		t.Fatalf("applied %d migrations, want %d", len(applied), len(pending))
	}
	if applied[0].Version != 1 || applied[0].Name != "00001_initial_schema.sql" {
		t.Errorf("first migration = %+v", applied[0])
	}

	again, err := st.Migrate(t.Context(), discard(), store.WithoutSessionLock())
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second Migrate applied %+v, want nothing", again)
	}

	status, err := st.MigrationStatus(t.Context())
	if err != nil {
		t.Fatalf("MigrationStatus: %v", err)
	}
	for i, m := range status {
		if !m.Applied || m.AppliedAt.IsZero() {
			t.Errorf("migration %d not applied after Migrate: %+v", m.Version, m)
		}
		if m.Version != applied[i].Version || m.Name != applied[i].Name {
			t.Errorf("status[%d] = %+v, applied %+v", i, m, applied[i])
		}
	}
}

func TestMigrateNilLogger(t *testing.T) {
	t.Parallel()

	st := openUnmigrated(t, storetest.DatabaseURL(t))
	if _, err := st.Migrate(t.Context(), nil, store.WithoutSessionLock()); err != nil {
		t.Fatalf("Migrate with nil logger: %v", err)
	}
}

func TestMigrateConcurrentWithSessionLock(t *testing.T) {
	t.Parallel()

	databaseURL := storetest.DatabaseURL(t)
	const replicas = 3
	stores := make([]*store.Store, replicas)
	for i := range stores {
		stores[i] = openUnmigrated(t, databaseURL)
	}

	var wg sync.WaitGroup
	results := make([][]store.AppliedMigration, replicas)
	errs := make([]error, replicas)
	start := make(chan struct{})
	for i, st := range stores {
		wg.Go(func() {
			<-start
			results[i], errs[i] = st.Migrate(t.Context(), discard())
		})
	}
	close(start)
	wg.Wait()

	total := 0
	for i := range stores {
		if errs[i] != nil {
			t.Errorf("replica %d Migrate: %v", i, errs[i])
		}
		total += len(results[i])
	}
	status, err := stores[0].MigrationStatus(t.Context())
	if err != nil {
		t.Fatalf("MigrationStatus: %v", err)
	}
	if total != len(status) {
		t.Errorf("replicas applied %d migrations in total, want %d", total, len(status))
	}
}

func TestMigrateRefusesNewerDatabase(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	mustExec(t, st, "INSERT INTO goose_db_version (version_id, is_applied) VALUES (99999, true)")

	_, err := st.Migrate(t.Context(), discard(), store.WithoutSessionLock())
	if !errors.Is(err, store.ErrDatabaseNewer) {
		t.Fatalf("Migrate error = %v, want ErrDatabaseNewer", err)
	}
}
