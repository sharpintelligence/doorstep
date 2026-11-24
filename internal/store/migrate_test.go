package store_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

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

	pending, _, err := st.MigrationStatus(t.Context())
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
	wantNames := []string{
		"00001_initial_schema.sql", "00002_queue_notify.sql", "00003_lock_scope_and_events.sql", "00004_deployment_lineage.sql",
		"00005_tracked_commits.sql", "00006_gate_result_phase.sql", "00007_auth.sql", "00008_blocks.sql", "00009_webhook_deliveries.sql",
		"00010_gate_reports.sql",
		"00011_deployment_migration.sql", "00012_schema_compatibility.sql",
	}
	for i, name := range wantNames {
		if i >= len(applied) || applied[i].Version != int64(i+1) || applied[i].Name != name {
			t.Errorf("migration %d = %+v, want version %d %s", i, applied[i:min(i+1, len(applied))], i+1, name)
		}
	}

	again, err := st.Migrate(t.Context(), discard(), store.WithoutSessionLock())
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second Migrate applied %+v, want nothing", again)
	}

	status, ahead, err := st.MigrationStatus(t.Context())
	if err != nil {
		t.Fatalf("MigrationStatus: %v", err)
	}
	if ahead != nil {
		t.Errorf("MigrationStatus reports a newer database after Migrate: %+v", ahead)
	}
	if floor := queryInt(t, st, "SELECT min_binary_schema FROM schema_compatibility"); floor != 12 {
		t.Errorf("min_binary_schema = %d, want 12", floor)
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
	status, _, err := stores[0].MigrationStatus(t.Context())
	if err != nil {
		t.Fatalf("MigrationStatus: %v", err)
	}
	if total != len(status) {
		t.Errorf("replicas applied %d migrations in total, want %d", total, len(status))
	}
}

func TestMigrateAgainstNewerDatabase(t *testing.T) {
	t.Parallel()

	const newerVersion = 99999
	tests := []struct {
		name       string
		migrated   bool
		setup      []string
		compatible bool
	}{
		{name: "floor at this binary", migrated: true, setup: []string{"UPDATE schema_compatibility SET min_binary_schema = 12"}, compatible: true},
		{name: "floor below this binary", migrated: true, setup: []string{"UPDATE schema_compatibility SET min_binary_schema = 1"}, compatible: true},
		{name: "floor above this binary", migrated: true, setup: []string{"UPDATE schema_compatibility SET min_binary_schema = 13"}},
		{name: "floor row missing", migrated: true, setup: []string{"DELETE FROM schema_compatibility"}},
		{name: "floor table missing", migrated: true, setup: []string{"DROP TABLE schema_compatibility"}},
		{name: "empty schema", setup: []string{"CREATE TABLE goose_db_version (id serial PRIMARY KEY, version_id bigint NOT NULL, is_applied boolean NOT NULL, tstamp timestamp NOT NULL DEFAULT now())"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := openUnmigrated(t, storetest.DatabaseURL(t))
			if tt.migrated {
				if _, err := st.Migrate(t.Context(), discard(), store.WithoutSessionLock()); err != nil {
					t.Fatalf("Migrate: %v", err)
				}
			}
			for _, sql := range tt.setup {
				mustExec(t, st, sql)
			}
			mustExec(t, st, "INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)", newerVersion)
			rowsBefore := queryInt(t, st, "SELECT count(*) FROM goose_db_version")

			applied, err := st.Migrate(t.Context(), discard(), store.WithoutSessionLock())
			if tt.compatible && err != nil {
				t.Fatalf("Migrate error = %v, want compatibility mode", err)
			}
			if !tt.compatible && !errors.Is(err, store.ErrDatabaseNewer) {
				t.Fatalf("Migrate error = %v, want ErrDatabaseNewer", err)
			}
			if len(applied) != 0 {
				t.Errorf("Migrate applied %+v against a newer database", applied)
			}
			if rows := queryInt(t, st, "SELECT count(*) FROM goose_db_version"); rows != rowsBefore {
				t.Errorf("goose_db_version has %d rows after Migrate, want %d", rows, rowsBefore)
			}
			if exists := queryInt(t, st, "SELECT count(*) FROM pg_tables WHERE schemaname = current_schema() AND tablename = 'apps'"); exists != boolInt(tt.migrated) {
				t.Errorf("apps table count = %d after Migrate, want %d", exists, boolInt(tt.migrated))
			}

			states, ahead, err := st.MigrationStatus(t.Context())
			if !tt.compatible {
				if !errors.Is(err, store.ErrDatabaseNewer) {
					t.Errorf("MigrationStatus error = %v, want ErrDatabaseNewer", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("MigrationStatus: %v", err)
			}
			if ahead == nil || ahead.DatabaseVersion != newerVersion || ahead.BinaryVersion != int64(len(states)) {
				t.Errorf("MigrationStatus ahead = %+v, want database %d and binary %d", ahead, newerVersion, len(states))
			}
			if ahead != nil && int64(queryInt(t, st, "SELECT min_binary_schema FROM schema_compatibility")) != ahead.MinBinarySchema {
				t.Errorf("MigrationStatus floor = %d, want the stored floor", ahead.MinBinarySchema)
			}
			for _, m := range states {
				if !m.Applied {
					t.Errorf("migration %d reported pending in compatibility mode", m.Version)
				}
			}
		})
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestMigrationLockID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a, b string
		same bool
	}{
		{a: "public", b: "public", same: true},
		{a: "public", b: "doorstep"},
		{a: "t_0011", b: "t_0012"},
		{a: "", b: "public"},
	}
	for _, tt := range tests {
		if got := store.MigrationLockID(tt.a) == store.MigrationLockID(tt.b); got != tt.same {
			t.Errorf("MigrationLockID(%q) == MigrationLockID(%q) is %v, want %v", tt.a, tt.b, got, tt.same)
		}
	}
}

func TestMigrateSessionLockIsPerSchema(t *testing.T) {
	t.Parallel()

	lockedURL := storetest.DatabaseURL(t)
	freeURL := storetest.DatabaseURL(t)
	holder := connectRaw(t, lockedURL)
	lockID := store.MigrationLockID(schemaOf(t, lockedURL))
	if _, err := holder.Exec(t.Context(), "SELECT pg_advisory_lock($1)", lockID); err != nil {
		t.Fatalf("take migration lock: %v", err)
	}

	freeCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := openUnmigrated(t, freeURL).Migrate(freeCtx, discard()); err != nil {
		t.Fatalf("Migrate of another schema while the lock is held: %v", err)
	}

	locked := openUnmigrated(t, lockedURL)
	done := make(chan error, 1)
	go func() {
		_, err := locked.Migrate(t.Context(), discard())
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Migrate finished while its schema's lock was held: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	if _, err := holder.Exec(t.Context(), "SELECT pg_advisory_unlock($1)", lockID); err != nil {
		t.Fatalf("release migration lock: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Migrate after the lock was released: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Migrate did not finish after the lock was released")
	}
}
