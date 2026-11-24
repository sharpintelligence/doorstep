package store_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

var (
	alice = core.Actor{Kind: core.ActorUser, ID: "alice", Name: "Alice"}
	ciBot = core.Actor{Kind: core.ActorToken, ID: "ci", Name: "ci"}
)

func mustRecordAudit(t *testing.T, st *store.Store, e core.AuditEntry) {
	t.Helper()
	if err := st.RecordAudit(t.Context(), e); err != nil {
		t.Fatalf("RecordAudit(%s): %v", e.Action, err)
	}
}

func TestRecordAuditRoundTrip(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	deploymentID := core.DeploymentID(42)
	full := core.AuditEntry{
		Actor:        alice,
		Role:         core.RoleDeployer,
		Action:       core.AuditDeployRequest,
		Target:       &core.Target{App: "web", Environment: "prod"},
		DeploymentID: &deploymentID,
		Detail:       json.RawMessage(`{"commit":"abc"}`),
	}
	minimal := core.AuditEntry{Actor: core.Actor{Kind: core.ActorSystem}, Action: "freeze.set"}
	before := time.Now().Add(-time.Minute)
	mustRecordAudit(t, st, full)
	mustRecordAudit(t, st, minimal)

	entries, err := st.ListAudit(t.Context(), store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ListAudit returned %d entries, want 2", len(entries))
	}
	gotMinimal, gotFull := entries[0], entries[1]
	if gotFull.ID <= 0 || gotMinimal.ID <= gotFull.ID {
		t.Errorf("ids = %d, %d, want newest first", gotMinimal.ID, gotFull.ID)
	}
	if gotFull.OccurredAt.Before(before) {
		t.Errorf("OccurredAt = %v", gotFull.OccurredAt)
	}
	if gotFull.Actor != alice || gotFull.Role != core.RoleDeployer || gotFull.Action != core.AuditDeployRequest {
		t.Errorf("full entry = %+v", gotFull)
	}
	if gotFull.Target == nil || *gotFull.Target != *full.Target || gotFull.DeploymentID == nil || *gotFull.DeploymentID != deploymentID {
		t.Errorf("full entry target %v deployment %v", gotFull.Target, gotFull.DeploymentID)
	}
	var detail map[string]string
	if err := json.Unmarshal(gotFull.Detail, &detail); err != nil || detail["commit"] != "abc" {
		t.Errorf("detail = %s, %v", gotFull.Detail, err)
	}
	if gotMinimal.Role != "" || gotMinimal.Target != nil || gotMinimal.DeploymentID != nil || string(gotMinimal.Detail) != "{}" {
		t.Errorf("minimal entry = %+v detail %s", gotMinimal, gotMinimal.Detail)
	}
}

func TestRecordAuditValidates(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	tests := []struct {
		name  string
		entry core.AuditEntry
	}{
		{name: "bad action", entry: core.AuditEntry{Actor: alice, Action: "Deploy"}},
		{name: "missing actor id", entry: core.AuditEntry{Actor: core.Actor{Kind: core.ActorUser}, Action: core.AuditLogin}},
		{name: "unknown role", entry: core.AuditEntry{Actor: alice, Role: "root", Action: core.AuditLogin}},
		{name: "detail array", entry: core.AuditEntry{Actor: alice, Action: core.AuditLogin, Detail: json.RawMessage(`[]`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := st.RecordAudit(t.Context(), tt.entry); err == nil {
				t.Error("RecordAudit accepted an invalid entry")
			}
		})
	}
	if n := queryInt(t, st, "SELECT count(*) FROM audit_log"); n != 0 {
		t.Errorf("%d rows written for invalid entries", n)
	}
}

func TestRecordAuditInTransaction(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	errBoom := errors.New("boom")
	err := st.InTx(t.Context(), func(tx *store.Store) error {
		mustRecordAudit(t, tx, core.AuditEntry{Actor: alice, Action: core.AuditLockAcquire})
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("InTx = %v", err)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM audit_log"); n != 0 {
		t.Errorf("rolled back transaction left %d audit rows", n)
	}

	if err := st.InTx(t.Context(), func(tx *store.Store) error {
		return tx.RecordAudit(t.Context(), core.AuditEntry{Actor: alice, Action: core.AuditLockAcquire})
	}); err != nil {
		t.Fatalf("InTx: %v", err)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM audit_log"); n != 1 {
		t.Errorf("committed transaction left %d audit rows, want 1", n)
	}
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sql  string
	}{
		{name: "update", sql: "UPDATE audit_log SET actor_id = 'mallory'"},
		{name: "delete", sql: "DELETE FROM audit_log"},
		{name: "truncate", sql: "TRUNCATE audit_log"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			mustRecordAudit(t, st, core.AuditEntry{Actor: alice, Action: core.AuditLogin})
			err := store.ExecSQL(t.Context(), st, tt.sql)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23001" {
				t.Fatalf("%s error = %v, want restrict_violation", tt.name, err)
			}
			if n := queryInt(t, st, "SELECT count(*) FROM audit_log WHERE actor_id = 'alice'"); n != 1 {
				t.Errorf("%d original rows left, want 1", n)
			}
		})
	}
}

func TestListAuditFilters(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	web := core.Target{App: "web", Environment: "prod"}
	api := core.Target{App: "api", Environment: "prod"}
	mustRecordAudit(t, st, core.AuditEntry{Actor: alice, Action: core.AuditLogin})
	mustRecordAudit(t, st, core.AuditEntry{Actor: alice, Action: core.AuditDeployRequest, Target: &web})
	mustRecordAudit(t, st, core.AuditEntry{Actor: ciBot, Action: core.AuditDeployRequest, Target: &web})
	mustRecordAudit(t, st, core.AuditEntry{Actor: ciBot, Action: core.AuditDeployRequest, Target: &api})
	mustRecordAudit(t, st, core.AuditEntry{Actor: alice, Action: core.AuditLockAcquire, Target: &api})

	all, err := st.ListAudit(t.Context(), store.AuditFilter{})
	if err != nil || len(all) != 5 {
		t.Fatalf("ListAudit = %d entries, %v", len(all), err)
	}
	ids := func(entries []core.AuditEntry) []int64 {
		out := make([]int64, 0, len(entries))
		for _, e := range entries {
			out = append(out, e.ID)
		}
		return out
	}
	id := func(i int) int64 { return all[len(all)-1-i].ID }

	tests := []struct {
		name   string
		filter store.AuditFilter
		want   []int64
	}{
		{name: "target", filter: store.AuditFilter{Target: &web}, want: []int64{id(2), id(1)}},
		{name: "actor", filter: store.AuditFilter{Actor: &ciBot}, want: []int64{id(3), id(2)}},
		{name: "action", filter: store.AuditFilter{Action: core.AuditDeployRequest}, want: []int64{id(3), id(2), id(1)}},
		{name: "target and actor", filter: store.AuditFilter{Target: &api, Actor: &alice}, want: []int64{id(4)}},
		{name: "limit", filter: store.AuditFilter{Limit: 2}, want: []int64{id(4), id(3)}},
		{name: "before id", filter: store.AuditFilter{BeforeID: id(3), Limit: 2}, want: []int64{id(2), id(1)}},
		{name: "last page", filter: store.AuditFilter{BeforeID: id(1)}, want: []int64{id(0)}},
		{name: "no match", filter: store.AuditFilter{Action: "freeze.set"}, want: []int64{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := st.ListAudit(t.Context(), tt.filter)
			if err != nil {
				t.Fatalf("ListAudit: %v", err)
			}
			if gotIDs := ids(got); !slices.Equal(gotIDs, tt.want) {
				t.Errorf("ids = %v, want %v", gotIDs, tt.want)
			}
		})
	}
}

func TestListAuditClampsLimit(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	mustExec(t, st, "INSERT INTO audit_log (actor_kind, actor_id, action) SELECT 'system', '', 'test.bulk' FROM generate_series(1, $1::int)", store.MaxAuditLimit+5)
	tests := []struct {
		limit int
		want  int
	}{
		{limit: 0, want: store.DefaultAuditLimit},
		{limit: -1, want: store.DefaultAuditLimit},
		{limit: 7, want: 7},
		{limit: store.MaxAuditLimit + 1, want: store.MaxAuditLimit},
	}
	for _, tt := range tests {
		got, err := st.ListAudit(t.Context(), store.AuditFilter{Limit: tt.limit})
		if err != nil {
			t.Fatalf("ListAudit: %v", err)
		}
		if len(got) != tt.want {
			t.Errorf("limit %d returned %d entries, want %d", tt.limit, len(got), tt.want)
		}
	}
}
