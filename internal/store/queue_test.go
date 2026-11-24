package store_test

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func mustEnqueue(t *testing.T, st *store.Store, d core.Deployment, priority core.Priority) core.Deployment {
	t.Helper()
	got, _, err := st.EnqueueDeployment(t.Context(), d, priority)
	if err != nil {
		t.Fatalf("EnqueueDeployment: %v", err)
	}
	return got
}

func entryPriority(t *testing.T, st *store.Store, id core.DeploymentID) core.Priority {
	t.Helper()
	return core.Priority(queryInt(t, st, "SELECT priority FROM queue_entries WHERE deployment_id = $1", id))
}

func noteCount(t *testing.T, st *store.Store, id core.DeploymentID) int {
	t.Helper()
	return queryInt(t, st, "SELECT count(*) FROM deployment_events WHERE deployment_id = $1 AND type = 'note'", id)
}

func TestEnqueueDeployment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		first        core.Priority
		firstState   core.State
		second       core.Priority
		sameCommit   bool
		otherEnv     bool
		wantCreated  bool
		wantPriority core.Priority
		wantNotes    int
	}{
		{name: "new commit is created", first: core.PriorityNormal, second: core.PriorityNormal, wantCreated: true, wantPriority: core.PriorityNormal},
		{name: "same commit is deduplicated", first: core.PriorityNormal, second: core.PriorityNormal, sameCommit: true, wantPriority: core.PriorityNormal},
		{name: "urgent request promotes a queued deploy", first: core.PriorityNormal, second: core.PriorityUrgent, sameCommit: true, wantPriority: core.PriorityUrgent, wantNotes: 1},
		{name: "normal request never demotes", first: core.PriorityUrgent, second: core.PriorityNormal, sameCommit: true, wantPriority: core.PriorityUrgent},
		{name: "in-flight deploy is returned without promotion", first: core.PriorityNormal, firstState: core.StateResolving, second: core.PriorityUrgent, sameCommit: true, wantPriority: core.PriorityNormal},
		{name: "finished deploy does not block a new one", first: core.PriorityNormal, firstState: core.StateSucceeded, second: core.PriorityNormal, sameCommit: true, wantCreated: true, wantPriority: core.PriorityNormal},
		{name: "superseded deploy does not block a new one", first: core.PriorityNormal, firstState: core.StateSuperseded, second: core.PriorityUrgent, sameCommit: true, wantCreated: true, wantPriority: core.PriorityUrgent},
		{name: "same commit in another environment is created", first: core.PriorityNormal, second: core.PriorityNormal, sameCommit: true, otherEnv: true, wantCreated: true, wantPriority: core.PriorityNormal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			sha := randomSHA(t)
			first, created, err := st.EnqueueDeployment(t.Context(), deploymentOf("web", sha), tt.first)
			if err != nil || !created {
				t.Fatalf("first EnqueueDeployment = %v, %v", created, err)
			}
			if tt.firstState != "" {
				mustExec(t, st, "UPDATE deployments SET state = $2 WHERE id = $1", first.ID, tt.firstState)
				if tt.firstState.Terminal() {
					mustExec(t, st, completeEntry, first.ID)
				}
			}

			d := deploymentOf("web", sha)
			if !tt.sameCommit {
				d = deploymentOf("web", randomSHA(t))
			}
			if tt.otherEnv {
				d.Target.Environment = "staging"
			}
			d.RequestedBy = "hubot"
			got, created, err := st.EnqueueDeployment(t.Context(), d, tt.second)
			if err != nil {
				t.Fatalf("second EnqueueDeployment: %v", err)
			}
			if created != tt.wantCreated {
				t.Errorf("created = %v, want %v", created, tt.wantCreated)
			}
			if !tt.wantCreated && got.ID != first.ID {
				t.Errorf("returned deployment %d, want existing %d", got.ID, first.ID)
			}
			if tt.wantCreated && got.ID == first.ID {
				t.Errorf("returned the existing deployment %d, want a new one", got.ID)
			}
			if tt.firstState == "" || !tt.firstState.Terminal() || tt.wantCreated {
				if p := entryPriority(t, st, got.ID); p != tt.wantPriority {
					t.Errorf("priority = %s, want %s", p, tt.wantPriority)
				}
			}
			if n := noteCount(t, st, got.ID); n != tt.wantNotes {
				t.Errorf("%d note events, want %d", n, tt.wantNotes)
			}
			if tt.wantNotes > 0 {
				var actor, message string
				if err := store.QueryRowSQL(t.Context(), st, "SELECT actor, message FROM deployment_events WHERE deployment_id = $1 AND type = 'note'", got.ID).Scan(&actor, &message); err != nil {
					t.Fatalf("read note: %v", err)
				}
				if actor != "hubot" || message != "priority raised to urgent" {
					t.Errorf("note = %q by %q", message, actor)
				}
			}
		})
	}
}

func TestEnqueueDeploymentNeverDeduplicatesRollbacks(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	sha := randomSHA(t)
	deploy := mustEnqueue(t, st, deploymentOf("web", sha), core.PriorityNormal)
	rollback := deploymentOf("web", sha)
	rollback.Kind = core.KindRollback
	rollback.RollbackOfID = &deploy.ID
	for range 2 {
		got, created, err := st.EnqueueDeployment(t.Context(), rollback, core.PriorityRollback)
		if err != nil || !created || got.Kind != core.KindRollback {
			t.Fatalf("EnqueueDeployment(rollback) = %+v, %v, %v", got, created, err)
		}
	}
}

func TestEnqueueDeploymentRejectsInvalid(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	tests := []struct {
		name   string
		mutate func(*core.Deployment)
	}{
		{name: "missing app", mutate: func(d *core.Deployment) { d.Target.App = "" }},
		{name: "bad commit", mutate: func(d *core.Deployment) { d.Commit.SHA = "abc" }},
		{name: "not queued", mutate: func(d *core.Deployment) { d.State = core.StateResolving }},
	}
	for _, tt := range tests {
		d := newDeployment("web")
		tt.mutate(&d)
		if _, created, err := st.EnqueueDeployment(t.Context(), d, core.PriorityNormal); err == nil || created {
			t.Errorf("%s: EnqueueDeployment = %v, %v; want error", tt.name, created, err)
		}
	}
	if n := queryInt(t, st, "SELECT count(*) FROM deployments"); n != 0 {
		t.Errorf("rejected enqueues created %d deployments", n)
	}
}

func TestEnqueueDeploymentConcurrentSameCommit(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	sha := randomSHA(t)
	const callers = 8
	ids := make([]core.DeploymentID, callers)
	created := make([]bool, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Go(func() {
			<-start
			priority := core.PriorityNormal
			if i == callers-1 {
				priority = core.PriorityUrgent
			}
			d, ok, err := st.EnqueueDeployment(t.Context(), deploymentOf("web", sha), priority)
			if err != nil {
				t.Errorf("EnqueueDeployment: %v", err)
				return
			}
			ids[i], created[i] = d.ID, ok
		})
	}
	close(start)
	wg.Wait()

	n := 0
	for i := range callers {
		if created[i] {
			n++
		}
		if ids[i] != ids[0] {
			t.Errorf("caller %d got deployment %d, caller 0 got %d", i, ids[i], ids[0])
		}
	}
	if n != 1 {
		t.Errorf("%d callers created a deployment, want 1", n)
	}
	if rows := queryInt(t, st, "SELECT count(*) FROM deployments"); rows != 1 {
		t.Errorf("%d deployments, want 1", rows)
	}
	if p := entryPriority(t, st, ids[0]); p != core.PriorityUrgent {
		t.Errorf("priority = %s, want urgent", p)
	}
}

func TestExpediteDeployment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		setup        func(t *testing.T, st *store.Store) core.DeploymentID
		wantErr      error
		wantPriority core.Priority
		wantNotes    int
	}{
		{
			name: "queued deploy is raised",
			setup: func(t *testing.T, st *store.Store) core.DeploymentID {
				t.Helper()
				return mustEnqueue(t, st, deploymentOf("web", randomSHA(t)), core.PriorityNormal).ID
			},
			wantPriority: core.PriorityUrgent,
			wantNotes:    1,
		},
		{
			name: "already urgent is unchanged",
			setup: func(t *testing.T, st *store.Store) core.DeploymentID {
				t.Helper()
				return mustEnqueue(t, st, deploymentOf("web", randomSHA(t)), core.PriorityUrgent).ID
			},
			wantPriority: core.PriorityUrgent,
		},
		{
			name: "leased queued deploy is raised",
			setup: func(t *testing.T, st *store.Store) core.DeploymentID {
				t.Helper()
				d := mustEnqueue(t, st, deploymentOf("web", randomSHA(t)), core.PriorityNormal)
				if _, _, _, err := st.Claim(t.Context(), "w1", testLease); err != nil {
					t.Fatalf("Claim: %v", err)
				}
				return d.ID
			},
			wantPriority: core.PriorityUrgent,
			wantNotes:    1,
		},
		{
			name: "in-flight deploy is not found",
			setup: func(t *testing.T, st *store.Store) core.DeploymentID {
				t.Helper()
				d := mustEnqueue(t, st, deploymentOf("web", randomSHA(t)), core.PriorityNormal)
				mustExec(t, st, startEntry, d.ID)
				return d.ID
			},
			wantErr:      core.ErrNotFound,
			wantPriority: core.PriorityNormal,
		},
		{
			name: "rollback is not found",
			setup: func(t *testing.T, st *store.Store) core.DeploymentID {
				t.Helper()
				d := mustEnqueue(t, st, deploymentOf("web", randomSHA(t)), core.PriorityNormal)
				r := deploymentOf("web", randomSHA(t))
				r.Kind = core.KindRollback
				r.RollbackOfID = &d.ID
				return mustCreate(t, st, r, int(core.PriorityRollback)).ID
			},
			wantErr:      core.ErrNotFound,
			wantPriority: core.PriorityRollback,
		},
		{
			name: "finished deploy is not found",
			setup: func(t *testing.T, st *store.Store) core.DeploymentID {
				t.Helper()
				d := mustEnqueue(t, st, deploymentOf("web", randomSHA(t)), core.PriorityNormal)
				mustExec(t, st, completeEntry, d.ID)
				return d.ID
			},
			wantErr: core.ErrNotFound,
		},
		{
			name:    "unknown deployment is not found",
			setup:   func(*testing.T, *store.Store) core.DeploymentID { return 424242 },
			wantErr: core.ErrNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			id := tt.setup(t, st)
			err := st.ExpediteDeployment(t.Context(), id, core.PriorityUrgent, "alice")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ExpediteDeployment = %v, want %v", err, tt.wantErr)
			}
			if tt.wantPriority != 0 || tt.wantErr == nil {
				if p := entryPriority(t, st, id); p != tt.wantPriority {
					t.Errorf("priority = %s, want %s", p, tt.wantPriority)
				}
			}
			if n := queryInt(t, st, "SELECT count(*) FROM deployment_events WHERE deployment_id = $1 AND type = 'note' AND actor = 'alice'", id); n != tt.wantNotes {
				t.Errorf("%d note events, want %d", n, tt.wantNotes)
			}
		})
	}
}

func TestQueueEntryLease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare string
		worker  string
		op      func(t *testing.T, st *store.Store, id core.DeploymentID, worker string) error
		wantErr error
		check   func(t *testing.T, st *store.Store, id core.DeploymentID)
	}{
		{
			name:   "heartbeat extends a live lease",
			worker: "w1",
			op: func(t *testing.T, st *store.Store, id core.DeploymentID, worker string) error {
				t.Helper()
				mustExec(t, st, "UPDATE queue_entries SET lease_expires_at = now() + interval '1 second' WHERE deployment_id = $1", id)
				expires, err := st.HeartbeatQueueEntry(t.Context(), id, worker, time.Hour)
				if err == nil && time.Until(expires) < 50*time.Minute {
					t.Errorf("lease expires in %s, want about an hour", time.Until(expires))
				}
				return err
			},
			check: func(t *testing.T, st *store.Store, id core.DeploymentID) {
				t.Helper()
				if n := queryInt(t, st, "SELECT count(*) FROM queue_entries WHERE deployment_id = $1 AND lease_expires_at > now() + interval '50 minutes'", id); n != 1 {
					t.Error("lease was not extended")
				}
			},
		},
		{
			name:    "heartbeat of an expired lease is lost",
			prepare: expireLease,
			worker:  "w1",
			op: func(t *testing.T, st *store.Store, id core.DeploymentID, worker string) error {
				t.Helper()
				_, err := st.HeartbeatQueueEntry(t.Context(), id, worker, time.Hour)
				return err
			},
			wantErr: core.ErrLeaseLost,
		},
		{
			name:   "heartbeat by another worker is lost",
			worker: "w2",
			op: func(t *testing.T, st *store.Store, id core.DeploymentID, worker string) error {
				t.Helper()
				_, err := st.HeartbeatQueueEntry(t.Context(), id, worker, time.Hour)
				return err
			},
			wantErr: core.ErrLeaseLost,
		},
		{
			name:    "heartbeat of a completed entry is lost",
			prepare: completeEntry,
			worker:  "w1",
			op: func(t *testing.T, st *store.Store, id core.DeploymentID, worker string) error {
				t.Helper()
				_, err := st.HeartbeatQueueEntry(t.Context(), id, worker, time.Hour)
				return err
			},
			wantErr: core.ErrLeaseLost,
		},
		{
			name:   "release clears the lease and delays the entry",
			worker: "w1",
			op: func(t *testing.T, st *store.Store, id core.DeploymentID, worker string) error {
				t.Helper()
				return st.ReleaseQueueEntry(t.Context(), id, worker, time.Hour)
			},
			check: func(t *testing.T, st *store.Store, id core.DeploymentID) {
				t.Helper()
				if n := queryInt(t, st, `SELECT count(*) FROM queue_entries WHERE deployment_id = $1
					AND lease_owner IS NULL AND lease_expires_at IS NULL AND available_at > now() + interval '50 minutes'`, id); n != 1 {
					t.Error("entry was not released with a delay")
				}
				if _, _, ok, err := st.Claim(t.Context(), "w3", testLease); ok || err != nil {
					t.Errorf("Claim of a delayed entry = %v, %v", ok, err)
				}
			},
		},
		{
			name:   "release without delay is claimable at once",
			worker: "w1",
			op: func(t *testing.T, st *store.Store, id core.DeploymentID, worker string) error {
				t.Helper()
				return st.ReleaseQueueEntry(t.Context(), id, worker, 0)
			},
			check: func(t *testing.T, st *store.Store, id core.DeploymentID) {
				t.Helper()
				d, c, ok, err := st.Claim(t.Context(), "w3", testLease)
				if err != nil || !ok || d.ID != id || c.Attempt != 2 {
					t.Errorf("Claim after release = %d, %+v, %v, %v", d.ID, c, ok, err)
				}
			},
		},
		{
			name:    "release of an expired lease that nobody took over",
			prepare: expireLease,
			worker:  "w1",
			op: func(t *testing.T, st *store.Store, id core.DeploymentID, worker string) error {
				t.Helper()
				return st.ReleaseQueueEntry(t.Context(), id, worker, 0)
			},
		},
		{
			name:    "release of a finished deployment drops its entry",
			prepare: succeedDeployment,
			worker:  "w1",
			op: func(t *testing.T, st *store.Store, id core.DeploymentID, worker string) error {
				t.Helper()
				return st.ReleaseQueueEntry(t.Context(), id, worker, time.Hour)
			},
			check: func(t *testing.T, st *store.Store, id core.DeploymentID) {
				t.Helper()
				if n := queryInt(t, st, "SELECT count(*) FROM queue_entries WHERE deployment_id = $1", id); n != 0 {
					t.Error("entry of a finished deployment was kept")
				}
			},
		},
		{
			name:    "release of a cancelled deployment by another worker is lost",
			prepare: cancelEntry,
			worker:  "w2",
			op: func(t *testing.T, st *store.Store, id core.DeploymentID, worker string) error {
				t.Helper()
				return st.ReleaseQueueEntry(t.Context(), id, worker, 0)
			},
			wantErr: core.ErrLeaseLost,
			check: func(t *testing.T, st *store.Store, id core.DeploymentID) {
				t.Helper()
				if n := queryInt(t, st, "SELECT count(*) FROM queue_entries WHERE deployment_id = $1 AND lease_owner = 'w1'", id); n != 1 {
					t.Error("foreign release dropped the entry")
				}
			},
		},
		{
			name:    "release of a completed entry is lost",
			prepare: completeEntry,
			worker:  "w1",
			op: func(t *testing.T, st *store.Store, id core.DeploymentID, worker string) error {
				t.Helper()
				return st.ReleaseQueueEntry(t.Context(), id, worker, 0)
			},
			wantErr: core.ErrLeaseLost,
		},
		{
			name:   "release by another worker is lost",
			worker: "w2",
			op: func(t *testing.T, st *store.Store, id core.DeploymentID, worker string) error {
				t.Helper()
				return st.ReleaseQueueEntry(t.Context(), id, worker, 0)
			},
			wantErr: core.ErrLeaseLost,
			check: func(t *testing.T, st *store.Store, id core.DeploymentID) {
				t.Helper()
				if n := queryInt(t, st, "SELECT count(*) FROM queue_entries WHERE deployment_id = $1 AND lease_owner = 'w1'", id); n != 1 {
					t.Error("foreign release changed the lease")
				}
			},
		},
		{
			name:   "complete deletes the entry",
			worker: "w1",
			op: func(t *testing.T, st *store.Store, id core.DeploymentID, worker string) error {
				t.Helper()
				return st.CompleteQueueEntry(t.Context(), id, worker)
			},
			check: func(t *testing.T, st *store.Store, id core.DeploymentID) {
				t.Helper()
				if n := queryInt(t, st, "SELECT count(*) FROM queue_entries WHERE deployment_id = $1", id); n != 0 {
					t.Error("entry was not deleted")
				}
			},
		},
		{
			name:   "complete by another worker is lost",
			worker: "w2",
			op: func(t *testing.T, st *store.Store, id core.DeploymentID, worker string) error {
				t.Helper()
				return st.CompleteQueueEntry(t.Context(), id, worker)
			},
			wantErr: core.ErrLeaseLost,
		},
		{
			name:    "complete of a completed entry is lost",
			prepare: completeEntry,
			worker:  "w1",
			op: func(t *testing.T, st *store.Store, id core.DeploymentID, worker string) error {
				t.Helper()
				return st.CompleteQueueEntry(t.Context(), id, worker)
			},
			wantErr: core.ErrLeaseLost,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			d := mustEnqueue(t, st, deploymentOf("web", randomSHA(t)), core.PriorityNormal)
			if _, _, ok, err := st.Claim(t.Context(), "w1", testLease); err != nil || !ok {
				t.Fatalf("Claim = %v, %v", ok, err)
			}
			if tt.prepare != "" {
				mustExec(t, st, tt.prepare, d.ID)
			}
			if err := tt.op(t, st, d.ID, tt.worker); !errors.Is(err, tt.wantErr) {
				t.Fatalf("operation error = %v, want %v", err, tt.wantErr)
			}
			if tt.check != nil {
				tt.check(t, st, d.ID)
			}
		})
	}
}

func TestQueueEntryLeaseRejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustEnqueue(t, st, deploymentOf("web", randomSHA(t)), core.PriorityNormal)
	tests := []struct {
		name string
		op   func() error
	}{
		{name: "heartbeat without worker", op: func() error {
			_, err := st.HeartbeatQueueEntry(t.Context(), d.ID, " ", time.Minute)
			return err
		}},
		{name: "heartbeat with zero lease", op: func() error {
			_, err := st.HeartbeatQueueEntry(t.Context(), d.ID, "w1", 0)
			return err
		}},
		{name: "release without worker", op: func() error { return st.ReleaseQueueEntry(t.Context(), d.ID, "", 0) }},
		{name: "release with negative delay", op: func() error { return st.ReleaseQueueEntry(t.Context(), d.ID, "w1", -time.Second) }},
		{name: "complete without worker", op: func() error { return st.CompleteQueueEntry(t.Context(), d.ID, "") }},
	}
	for _, tt := range tests {
		err := tt.op()
		if err == nil || errors.Is(err, core.ErrLeaseLost) {
			t.Errorf("%s: error = %v, want a validation error", tt.name, err)
		}
	}
}

func TestQueueEntries(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	first := mustEnqueue(t, st, deploymentOf("web", randomSHA(t)), core.PriorityNormal)
	second := mustEnqueue(t, st, deploymentOf("web", randomSHA(t)), core.PriorityNormal)
	urgent := mustEnqueue(t, st, deploymentOf("web", randomSHA(t)), core.PriorityUrgent)
	staging := deploymentOf("web", randomSHA(t))
	staging.Target.Environment = "staging"
	stagingDeploy := mustEnqueue(t, st, staging, core.PriorityNormal)
	mustEnqueue(t, st, deploymentOf("api", randomSHA(t)), core.PriorityNormal)
	cancelled := mustEnqueue(t, st, deploymentOf("web", randomSHA(t)), core.PriorityNormal)
	mustExec(t, st, cancelEntry, cancelled.ID)
	mustExec(t, st, startEntry, first.ID)
	mustExec(t, st, "UPDATE queue_entries SET lease_owner = 'w1', lease_expires_at = now() + interval '1 minute', attempts = 1 WHERE deployment_id = $1", first.ID)

	entries, err := st.QueueEntries(t.Context(), "web")
	if err != nil {
		t.Fatalf("QueueEntries: %v", err)
	}
	type row struct {
		id       core.DeploymentID
		env      string
		position int
		priority core.Priority
		inFlight bool
		leased   bool
		attempts int
	}
	want := []row{
		{first.ID, "prod", 1, core.PriorityNormal, true, true, 1},
		{urgent.ID, "prod", 2, core.PriorityUrgent, false, false, 0},
		{second.ID, "prod", 3, core.PriorityNormal, false, false, 0},
		{stagingDeploy.ID, "staging", 1, core.PriorityNormal, false, false, 0},
	}
	if len(entries) != len(want) {
		t.Fatalf("QueueEntries returned %d entries, want %d: %+v", len(entries), len(want), entries)
	}
	for i, w := range want {
		e := entries[i]
		got := row{e.Deployment.ID, e.Deployment.Target.Environment, e.Position, e.Priority, e.InFlight(), e.LeaseExpiresAt != nil, e.Attempts}
		if got != w {
			t.Errorf("entry %d = %+v, want %+v", i, got, w)
		}
		if e.Deployment.Target.App != "web" || e.EnqueuedAt.IsZero() || e.AvailableAt.IsZero() || e.Deployment.Commit.SHA == "" {
			t.Errorf("entry %d is incomplete: %+v", i, e)
		}
	}

	mustExec(t, st, expireLease, first.ID)
	entries, err = st.QueueEntries(t.Context(), "web")
	if err != nil {
		t.Fatalf("QueueEntries: %v", err)
	}
	if entries[0].LeaseExpiresAt != nil || !entries[0].InFlight() {
		t.Errorf("entry with an expired lease = %+v, want no lease but still in flight", entries[0])
	}

	none, err := st.QueueEntries(t.Context(), "unknown")
	if err != nil || len(none) != 0 {
		t.Errorf("QueueEntries(unknown) = %v, %v; want empty", none, err)
	}

	all, err := st.QueueEntries(t.Context(), "")
	if err != nil {
		t.Fatalf("QueueEntries(all): %v", err)
	}
	type placed struct {
		target   core.Target
		position int
	}
	var got []placed
	for _, e := range all {
		got = append(got, placed{e.Deployment.Target, e.Position})
	}
	webProd := core.Target{App: "web", Environment: "prod"}
	wantAll := []placed{
		{core.Target{App: "api", Environment: "prod"}, 1},
		{webProd, 1},
		{webProd, 2},
		{webProd, 3},
		{core.Target{App: "web", Environment: "staging"}, 1},
	}
	if !slices.Equal(got, wantAll) {
		t.Errorf("QueueEntries(all) = %+v, want %+v", got, wantAll)
	}
}

func TestQueueDepth(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	depth := func() int {
		t.Helper()
		n, err := st.QueueDepth(t.Context())
		if err != nil {
			t.Fatalf("QueueDepth: %v", err)
		}
		return n
	}
	if n := depth(); n != 0 {
		t.Errorf("empty queue depth = %d", n)
	}
	for i := range 3 {
		mustEnqueue(t, st, deploymentOf(fmt.Sprintf("app-%d", i), randomSHA(t)), core.PriorityNormal)
	}
	waiting := mustEnqueue(t, st, deploymentOf("app-0", randomSHA(t)), core.PriorityNormal)
	if n := depth(); n != 4 {
		t.Errorf("depth = %d, want 4", n)
	}
	d, _, _, err := st.Claim(t.Context(), "w1", testLease)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if n := depth(); n != 3 {
		t.Errorf("depth with one leased = %d, want 3", n)
	}
	mustExec(t, st, startEntry, d.ID)
	mustExec(t, st, expireLease, d.ID)
	mustExec(t, st, cancelEntry, waiting.ID)
	if n := depth(); n != 2 {
		t.Errorf("depth with one in flight and one cancelled = %d, want 2", n)
	}
}
