package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func gateResult(id core.DeploymentID, name string, status core.GateStatus) core.GateResult {
	return core.GateResult{
		DeploymentID: id,
		Name:         name,
		Type:         "health",
		Phase:        core.GatePhaseAfter,
		Status:       status,
		Message:      name + " is " + string(status),
	}
}

func mustRecord(t *testing.T, st *store.Store, r core.GateResult) (core.GateResult, bool) {
	t.Helper()
	stored, changed, err := st.RecordGateResult(t.Context(), r)
	if err != nil {
		t.Fatalf("RecordGateResult(%s %s): %v", r.Name, r.Status, err)
	}
	return stored, changed
}

func countGateEvents(t *testing.T, st *store.Store, id core.DeploymentID) int {
	t.Helper()
	return queryInt(t, st, "SELECT count(*) FROM deployment_events WHERE deployment_id = $1 AND type = 'gate'", id)
}

func backdateGate(t *testing.T, st *store.Store, id core.DeploymentID, name string) {
	t.Helper()
	mustExec(t, st, "UPDATE gate_results SET started_at = started_at - interval '1 hour' WHERE deployment_id = $1 AND name = $2", id, name)
}

func TestRecordGateResultInsertsPending(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)

	stored, changed := mustRecord(t, st, gateResult(d.ID, "health", core.GatePending))
	if !changed {
		t.Error("first recording reported unchanged")
	}
	if stored.DeploymentID != d.ID || stored.Name != "health" || stored.Type != "health" || stored.Phase != core.GatePhaseAfter {
		t.Errorf("stored identity = %+v", stored)
	}
	if stored.Status != core.GatePending || stored.Message != "health is pending" {
		t.Errorf("stored status = %s %q", stored.Status, stored.Message)
	}
	if stored.FinishedAt != nil {
		t.Errorf("pending FinishedAt = %v, want nil", stored.FinishedAt)
	}
	if stored.StartedAt.IsZero() || stored.UpdatedAt.Before(stored.StartedAt) {
		t.Errorf("StartedAt = %v, UpdatedAt = %v", stored.StartedAt, stored.UpdatedAt)
	}
	if string(stored.Evidence) != "{}" {
		t.Errorf("default evidence = %s, want {}", stored.Evidence)
	}
	if n := countGateEvents(t, st, d.ID); n != 1 {
		t.Errorf("gate events = %d, want 1", n)
	}

	var message, data string
	if err := store.QueryRowSQL(t.Context(), st, "SELECT message, data::text FROM deployment_events WHERE deployment_id = $1 AND type = 'gate'", d.ID).Scan(&message, &data); err != nil {
		t.Fatalf("read gate event: %v", err)
	}
	if message != "health pending" {
		t.Errorf("event message = %q", message)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatalf("decode event data %s: %v", data, err)
	}
	want := map[string]string{"gate": "health", "type": "health", "phase": "after", "status": "pending"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("event data %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestRecordGateResultRepeatedPending(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)
	mustRecord(t, st, gateResult(d.ID, "health", core.GatePending))
	backdateGate(t, st, d.ID, "health")
	before, err := st.ListGateResults(t.Context(), d.ID)
	if err != nil {
		t.Fatalf("ListGateResults: %v", err)
	}

	again := gateResult(d.ID, "health", core.GatePending)
	again.Message = "still waiting"
	stored, changed := mustRecord(t, st, again)
	if changed {
		t.Error("repeated pending reported changed")
	}
	if !stored.StartedAt.Equal(before[0].StartedAt) {
		t.Errorf("StartedAt moved from %v to %v", before[0].StartedAt, stored.StartedAt)
	}
	if !stored.UpdatedAt.After(before[0].UpdatedAt) {
		t.Errorf("UpdatedAt %v did not advance past %v", stored.UpdatedAt, before[0].UpdatedAt)
	}
	if stored.UpdatedAt.Sub(stored.StartedAt) < time.Hour {
		t.Errorf("UpdatedAt - StartedAt = %v, want at least an hour", stored.UpdatedAt.Sub(stored.StartedAt))
	}
	if stored.Message != "still waiting" {
		t.Errorf("message = %q, want the latest", stored.Message)
	}
	if n := countGateEvents(t, st, d.ID); n != 1 {
		t.Errorf("gate events = %d, want 1", n)
	}
}

func TestRecordGateResultTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		from         core.GateStatus
		to           core.GateStatus
		changed      bool
		finished     bool
		keepFinished bool
		resetStart   bool
	}{
		{name: "pending to passed", from: core.GatePending, to: core.GatePassed, changed: true, finished: true},
		{name: "pending to failed", from: core.GatePending, to: core.GateFailed, changed: true, finished: true},
		{name: "pending to skipped", from: core.GatePending, to: core.GateSkipped, changed: true, finished: true},
		{name: "passed again", from: core.GatePassed, to: core.GatePassed, finished: true, keepFinished: true},
		{name: "failed again", from: core.GateFailed, to: core.GateFailed, finished: true, keepFinished: true},
		{name: "passed to failed", from: core.GatePassed, to: core.GateFailed, changed: true, finished: true},
		{name: "passed to pending", from: core.GatePassed, to: core.GatePending, changed: true, resetStart: true},
		{name: "failed to pending", from: core.GateFailed, to: core.GatePending, changed: true, resetStart: true},
		{name: "skipped to pending", from: core.GateSkipped, to: core.GatePending, changed: true, resetStart: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			d := mustCreate(t, st, newDeployment("web"), 0)
			mustRecord(t, st, gateResult(d.ID, "health", tt.from))
			backdateGate(t, st, d.ID, "health")
			before, err := st.ListGateResults(t.Context(), d.ID)
			if err != nil {
				t.Fatalf("ListGateResults: %v", err)
			}
			first := before[0]
			if tt.from.Terminal() != (first.FinishedAt != nil) {
				t.Fatalf("initial %s FinishedAt = %v", tt.from, first.FinishedAt)
			}

			stored, changed := mustRecord(t, st, gateResult(d.ID, "health", tt.to))
			if changed != tt.changed {
				t.Errorf("changed = %v, want %v", changed, tt.changed)
			}
			if stored.Status != tt.to {
				t.Errorf("status = %s, want %s", stored.Status, tt.to)
			}
			if (stored.FinishedAt != nil) != tt.finished {
				t.Fatalf("FinishedAt = %v, want set %v", stored.FinishedAt, tt.finished)
			}
			if tt.finished {
				kept := first.FinishedAt != nil && stored.FinishedAt.Equal(*first.FinishedAt)
				if kept != tt.keepFinished {
					t.Errorf("FinishedAt kept = %v, want %v (before %v, after %v)", kept, tt.keepFinished, first.FinishedAt, stored.FinishedAt)
				}
				if stored.FinishedAt.Before(stored.StartedAt) {
					t.Errorf("FinishedAt %v before StartedAt %v", stored.FinishedAt, stored.StartedAt)
				}
			}
			reset := !stored.StartedAt.Equal(first.StartedAt)
			if reset != tt.resetStart {
				t.Errorf("StartedAt reset = %v, want %v (before %v, after %v)", reset, tt.resetStart, first.StartedAt, stored.StartedAt)
			}
			if tt.resetStart && stored.UpdatedAt.Sub(stored.StartedAt) > time.Minute {
				t.Errorf("reset StartedAt %v is far from UpdatedAt %v", stored.StartedAt, stored.UpdatedAt)
			}
			wantEvents := 1
			if tt.changed {
				wantEvents = 2
			}
			if n := countGateEvents(t, st, d.ID); n != wantEvents {
				t.Errorf("gate events = %d, want %d", n, wantEvents)
			}
		})
	}
}

func TestRecordGateResultEvidence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		evidence json.RawMessage
		want     map[string]any
	}{
		{name: "nil", evidence: nil, want: map[string]any{}},
		{name: "empty", evidence: json.RawMessage{}, want: map[string]any{}},
		{name: "object", evidence: json.RawMessage(`{"job":"verify-1","attempt":2}`), want: map[string]any{"job": "verify-1", "attempt": float64(2)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			d := mustCreate(t, st, newDeployment("web"), 0)
			r := gateResult(d.ID, "health", core.GatePending)
			r.Evidence = tt.evidence
			stored, _ := mustRecord(t, st, r)
			listed, err := st.ListGateResults(t.Context(), d.ID)
			if err != nil {
				t.Fatalf("ListGateResults: %v", err)
			}
			for _, raw := range []json.RawMessage{stored.Evidence, listed[0].Evidence} {
				var got map[string]any
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatalf("decode evidence %s: %v", raw, err)
				}
				if len(got) != len(tt.want) {
					t.Errorf("evidence = %v, want %v", got, tt.want)
				}
				for k, v := range tt.want {
					if got[k] != v {
						t.Errorf("evidence %s = %v, want %v", k, got[k], v)
					}
				}
			}
		})
	}
}

func TestRecordGateResultRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)
	tests := []struct {
		name   string
		mutate func(*core.GateResult)
	}{
		{name: "zero deployment", mutate: func(r *core.GateResult) { r.DeploymentID = 0 }},
		{name: "negative deployment", mutate: func(r *core.GateResult) { r.DeploymentID = -1 }},
		{name: "blank name", mutate: func(r *core.GateResult) { r.Name = " " }},
		{name: "blank type", mutate: func(r *core.GateResult) { r.Type = "" }},
		{name: "unknown phase", mutate: func(r *core.GateResult) { r.Phase = "during" }},
		{name: "empty phase", mutate: func(r *core.GateResult) { r.Phase = "" }},
		{name: "unknown status", mutate: func(r *core.GateResult) { r.Status = "running" }},
		{name: "invalid evidence", mutate: func(r *core.GateResult) { r.Evidence = json.RawMessage(`{"job":`) }},
		{name: "oversized evidence", mutate: func(r *core.GateResult) {
			r.Evidence = json.RawMessage(`"` + strings.Repeat("x", core.MaxGateEvidenceBytes) + `"`)
		}},
		{name: "NUL in evidence", mutate: func(r *core.GateResult) { r.Evidence = json.RawMessage(`{"a":"\u0000"}`) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := gateResult(d.ID, "health-"+tt.name, core.GatePending)
			tt.mutate(&r)
			if _, _, err := st.RecordGateResult(t.Context(), r); err == nil {
				t.Fatal("RecordGateResult accepted invalid input")
			}
		})
	}
	if n := queryInt(t, st, "SELECT count(*) FROM gate_results"); n != 0 {
		t.Errorf("gate_results has %d rows, want 0", n)
	}
	if n := countGateEvents(t, st, d.ID); n != 0 {
		t.Errorf("gate events = %d, want 0", n)
	}
}

func TestRecordGateResultUnknownDeployment(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	_, _, err := st.RecordGateResult(t.Context(), gateResult(424242, "health", core.GatePending))
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGateResultsCascadeWithDeployment(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)
	mustRecord(t, st, gateResult(d.ID, "health", core.GatePassed))
	mustExec(t, st, "DELETE FROM deployments WHERE id = $1", d.ID)
	results, err := st.ListGateResults(t.Context(), d.ID)
	if err != nil {
		t.Fatalf("ListGateResults: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results after delete = %+v, want none", results)
	}
}

func TestListGateResultsOrderAndIsolation(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	web := mustCreate(t, st, newDeployment("web"), 0)
	api := mustCreate(t, st, newDeployment("api"), 0)

	checks := gateResult(web.ID, "checks", core.GatePassed)
	checks.Type = "checks"
	checks.Phase = core.GatePhaseBefore
	mustRecord(t, st, checks)
	mustRecord(t, st, gateResult(web.ID, "health", core.GatePending))
	mustRecord(t, st, gateResult(api.ID, "health", core.GateFailed))
	mustRecord(t, st, gateResult(web.ID, "checks", core.GatePassed))
	mustRecord(t, st, gateResult(web.ID, "soak", core.GatePending))

	results, err := st.ListGateResults(t.Context(), web.ID)
	if err != nil {
		t.Fatalf("ListGateResults: %v", err)
	}
	var names []string
	for _, r := range results {
		names = append(names, r.Name)
		if r.DeploymentID != web.ID {
			t.Errorf("result %s belongs to deployment %d", r.Name, r.DeploymentID)
		}
	}
	if want := []string{"checks", "health", "soak"}; !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if results[0].Phase != core.GatePhaseAfter || results[0].Type != "health" {
		t.Errorf("checks row = %+v, want the latest phase and type", results[0])
	}

	empty, err := st.ListGateResults(t.Context(), 424242)
	if err != nil {
		t.Fatalf("ListGateResults(unknown): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("unknown deployment results = %+v", empty)
	}
}

func TestRecordGateResultInTransaction(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)
	errRollback := errors.New("roll back")

	err := st.InTx(t.Context(), func(tx *store.Store) error {
		if _, _, err := tx.RecordGateResult(t.Context(), gateResult(d.ID, "health", core.GatePassed)); err != nil {
			return err
		}
		results, err := tx.ListGateResults(t.Context(), d.ID)
		if err != nil {
			return err
		}
		if len(results) != 1 {
			t.Errorf("results inside transaction = %d, want 1", len(results))
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("InTx = %v, want %v", err, errRollback)
	}
	results, err := st.ListGateResults(t.Context(), d.ID)
	if err != nil {
		t.Fatalf("ListGateResults: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results after rollback = %+v, want none", results)
	}
	if n := countGateEvents(t, st, d.ID); n != 0 {
		t.Errorf("gate events after rollback = %d, want 0", n)
	}

	err = st.InTx(t.Context(), func(tx *store.Store) error {
		_, _, err := tx.RecordGateResult(t.Context(), gateResult(d.ID, "health", core.GatePassed))
		return err
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}
	if n := countGateEvents(t, st, d.ID); n != 1 {
		t.Errorf("gate events after commit = %d, want 1", n)
	}
}

func TestRecordGateResultCleansMessage(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{name: "NUL removed", message: "a\x00b", want: "ab"},
		{name: "invalid UTF-8 replaced", message: "a\xffb", want: "a\uFFFDb"},
		{name: "truncated", message: strings.Repeat("a", core.MaxGateMessageBytes+5), want: strings.Repeat("a", core.MaxGateMessageBytes)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := gateResult(d.ID, "health-"+tt.name, core.GatePending)
			r.Message = tt.message
			stored, _ := mustRecord(t, st, r)
			if stored.Message != tt.want {
				t.Errorf("message = %q, want %q", stored.Message, tt.want)
			}
		})
	}
}

func TestRecordGateResultConcurrentWriters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		prepare  core.GateStatus
		statuses []core.GateStatus
		changes  int
	}{
		{name: "first pending", statuses: []core.GateStatus{core.GatePending}, changes: 1},
		{name: "first verdict", statuses: []core.GateStatus{core.GatePassed}, changes: 1},
		{name: "pending to verdict", prepare: core.GatePending, statuses: []core.GateStatus{core.GatePassed}, changes: 1},
		{name: "repeated pending", prepare: core.GatePending, statuses: []core.GateStatus{core.GatePending}, changes: 0},
		{name: "alternating", statuses: []core.GateStatus{core.GatePending, core.GatePassed}},
	}
	const gatesPerDeployment, writersPerGate = 10, 8
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			d := mustCreate(t, st, newDeployment("web"), 0)
			names := make([]string, gatesPerDeployment)
			for i := range names {
				names[i] = fmt.Sprintf("gate-%d", i)
				if tt.prepare != "" {
					mustRecord(t, st, gateResult(d.ID, names[i], tt.prepare))
				}
			}
			eventsBefore := countGateEvents(t, st, d.ID)

			var (
				wg      sync.WaitGroup
				mu      sync.Mutex
				changes = map[string]int{}
			)
			for _, name := range names {
				for w := range writersPerGate {
					status := tt.statuses[w%len(tt.statuses)]
					wg.Go(func() {
						_, changed, err := st.RecordGateResult(t.Context(), gateResult(d.ID, name, status))
						if err != nil {
							t.Errorf("RecordGateResult(%s %s): %v", name, status, err)
							return
						}
						if changed {
							mu.Lock()
							changes[name]++
							mu.Unlock()
						}
					})
				}
			}
			wg.Wait()

			total := 0
			for _, name := range names {
				total += changes[name]
				if len(tt.statuses) == 1 && changes[name] != tt.changes {
					t.Errorf("%s changed %d times, want %d", name, changes[name], tt.changes)
				}
			}
			if events := countGateEvents(t, st, d.ID) - eventsBefore; events != total {
				t.Errorf("gate events = %d, want one per reported change (%d)", events, total)
			}
			if n := queryInt(t, st, "SELECT count(*) FROM gate_results WHERE deployment_id = $1", d.ID); n != gatesPerDeployment {
				t.Errorf("gate_results rows = %d, want %d", n, gatesPerDeployment)
			}
		})
	}
}
