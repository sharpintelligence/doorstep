package store_test

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

type batchEnv struct {
	t     *testing.T
	st    *store.Store
	ids   map[string]core.DeploymentID
	names map[core.DeploymentID]string
	shas  map[string]string
	apps  map[string]string
	limit int
}

func newBatchEnv(t *testing.T, limit int) *batchEnv {
	t.Helper()
	return &batchEnv{
		t:     t,
		st:    storetest.Open(t),
		ids:   map[string]core.DeploymentID{},
		names: map[core.DeploymentID]string{},
		shas:  map[string]string{},
		apps:  map[string]string{},
		limit: limit,
	}
}

func (e *batchEnv) record(name string, d core.Deployment) {
	e.ids[name] = d.ID
	e.names[d.ID] = name
}

func (e *batchEnv) deploy(name, app string, priority core.Priority) {
	e.t.Helper()
	e.enqueue(name, app, randomSHA(e.t), priority)
}

func (e *batchEnv) redeploy(name, of string, priority core.Priority) {
	e.t.Helper()
	e.enqueue(name, e.apps[of], e.shas[of], priority)
}

func (e *batchEnv) enqueue(name, app, sha string, priority core.Priority) {
	e.t.Helper()
	d, created, err := e.st.EnqueueDeployment(e.t.Context(), deploymentOf(app, sha), priority)
	if err != nil || !created {
		e.t.Fatalf("EnqueueDeployment(%s) = %v, %v", name, created, err)
	}
	e.shas[name] = sha
	e.apps[name] = app
	e.record(name, d)
}

func (e *batchEnv) finish(name string) {
	e.t.Helper()
	e.exec(succeedDeployment, name)
	e.exec(completeEntry, name)
}

func (e *batchEnv) rollback(name, app, of string) {
	e.t.Helper()
	d := deploymentOf(app, randomSHA(e.t))
	d.Kind = core.KindRollback
	d.Source = core.SourceSystem
	target := e.ids[of]
	d.RollbackOfID = &target
	e.record(name, mustCreate(e.t, e.st, d, int(core.PriorityRollback)))
}

func (e *batchEnv) exec(sql, name string) {
	e.t.Helper()
	mustExec(e.t, e.st, sql, e.ids[name])
}

func (e *batchEnv) claim(want string, wantSuperseded ...string) {
	e.t.Helper()
	got, ok, err := e.st.ClaimBatch(e.t.Context(), "w1", testLease, func(core.Target) int { return e.limit })
	if err != nil {
		e.t.Fatalf("ClaimBatch: %v", err)
	}
	if want == "" {
		if ok {
			e.t.Fatalf("ClaimBatch returned %s, want nothing", e.names[got.Deployment.ID])
		}
		return
	}
	if !ok {
		e.t.Fatalf("ClaimBatch returned nothing, want %s", want)
	}
	if got.Deployment.ID != e.ids[want] {
		e.t.Fatalf("ClaimBatch returned %s, want %s", e.names[got.Deployment.ID], want)
	}
	if got.Claim.Attempt < 1 || got.Claim.LeaseExpiresAt.Before(time.Now().Add(testLease/2)) {
		e.t.Errorf("claim = %+v, want a live lease", got.Claim)
	}
	var superseded []string
	for _, id := range got.Superseded {
		superseded = append(superseded, e.names[id])
	}
	if !slices.Equal(superseded, wantSuperseded) {
		e.t.Errorf("superseded %v, want %v", superseded, wantSuperseded)
	}
	for _, name := range wantSuperseded {
		e.assertSuperseded(name, want)
	}
	var owner string
	if err := store.QueryRowSQL(e.t.Context(), e.st, "SELECT lease_owner FROM queue_entries WHERE deployment_id = $1", e.ids[want]).Scan(&owner); err != nil || owner != "w1" {
		e.t.Errorf("lease owner of %s = %q, %v; want w1", want, owner, err)
	}
	if n := queryInt(e.t, e.st, "SELECT count(*) FROM queue_entries WHERE lease_owner IS NOT NULL"); n != 1 {
		e.t.Errorf("%d leased entries after a claim, want 1", n)
	}
}

func (e *batchEnv) assertSuperseded(name, by string) {
	e.t.Helper()
	d, err := e.st.GetDeployment(e.t.Context(), e.ids[name])
	if err != nil {
		e.t.Fatalf("GetDeployment(%s): %v", name, err)
	}
	if d.State != core.StateSuperseded || d.SupersededByID == nil || *d.SupersededByID != e.ids[by] || d.FinishedAt == nil {
		e.t.Errorf("%s = state %s, superseded by %v, finished %v; want superseded by %s", name, d.State, d.SupersededByID, d.FinishedAt, by)
	}
	if n := queryInt(e.t, e.st, "SELECT count(*) FROM queue_entries WHERE deployment_id = $1", d.ID); n != 0 {
		e.t.Errorf("%s still has a queue entry", name)
	}
	events := queryInt(e.t, e.st, `SELECT count(*) FROM deployment_events
		WHERE deployment_id = $1 AND type = 'transition' AND from_state = 'queued' AND to_state = 'superseded'
		AND actor = 'doorstep' AND message = $2`, d.ID, fmt.Sprintf("superseded by deployment %d", e.ids[by]))
	if events != 1 {
		e.t.Errorf("%s has %d supersede events, want 1", name, events)
	}
}

func (e *batchEnv) assertState(name string, want core.State) {
	e.t.Helper()
	d, err := e.st.GetDeployment(e.t.Context(), e.ids[name])
	if err != nil {
		e.t.Fatalf("GetDeployment(%s): %v", name, err)
	}
	if d.State != want {
		e.t.Errorf("%s state = %s, want %s", name, d.State, want)
	}
}

func TestClaimBatch(t *testing.T) {
	t.Parallel()

	const normal, urgent = core.PriorityNormal, core.PriorityUrgent
	tests := []struct {
		name  string
		limit int
		run   func(e *batchEnv)
	}{
		{
			name:  "limit of one never batches",
			limit: 1,
			run: func(e *batchEnv) {
				e.deploy("c1", "web", normal)
				e.deploy("c2", "web", normal)
				e.deploy("c3", "web", normal)
				e.claim("c1")
				e.exec(completeEntry, "c1")
				e.claim("c2")
				e.exec(completeEntry, "c2")
				e.claim("c3")
				e.exec(completeEntry, "c3")
				e.claim("")
			},
		},
		{
			name:  "zero limit behaves as one",
			limit: 0,
			run: func(e *batchEnv) {
				e.deploy("c1", "web", normal)
				e.deploy("c2", "web", normal)
				e.claim("c1")
			},
		},
		{
			name:  "three of five then the rest",
			limit: 3,
			run: func(e *batchEnv) {
				for i := 1; i <= 5; i++ {
					e.deploy(fmt.Sprintf("c%d", i), "web", normal)
				}
				e.claim("c3", "c1", "c2")
				e.assertState("c4", core.StateQueued)
				e.exec(completeEntry, "c3")
				e.claim("c5", "c4")
				e.exec(completeEntry, "c5")
				e.claim("")
			},
		},
		{
			name:  "limit larger than the queue",
			limit: 10,
			run: func(e *batchEnv) {
				e.deploy("c1", "web", normal)
				e.deploy("c2", "web", normal)
				e.deploy("c3", "web", normal)
				e.claim("c3", "c1", "c2")
			},
		},
		{
			name:  "batches stay within their app",
			limit: 10,
			run: func(e *batchEnv) {
				e.deploy("w1", "web", normal)
				e.deploy("a1", "api", normal)
				e.deploy("w2", "web", normal)
				e.claim("w2", "w1")
				e.assertState("a1", core.StateQueued)
			},
		},
		{
			name:  "delayed entries inside the window are batched",
			limit: 10,
			run: func(e *batchEnv) {
				e.deploy("c1", "web", normal)
				e.deploy("c2", "web", normal)
				e.deploy("c3", "web", normal)
				e.exec(delayEntry, "c3")
				e.claim("c3", "c1", "c2")
			},
		},
		{
			name:  "urgent supersedes every older normal deploy regardless of the limit",
			limit: 1,
			run: func(e *batchEnv) {
				e.deploy("c1", "web", normal)
				e.deploy("c2", "web", normal)
				e.deploy("c3", "web", normal)
				e.deploy("hotfix", "web", urgent)
				e.deploy("c5", "web", normal)
				e.claim("hotfix", "c1", "c2", "c3")
				e.assertState("c5", core.StateQueued)
				e.exec(completeEntry, "hotfix")
				e.claim("c5")
			},
		},
		{
			name:  "urgent redeploy of an older commit leaves newer deploys queued",
			limit: 1,
			run: func(e *batchEnv) {
				e.deploy("c8", "web", normal)
				e.claim("c8")
				e.finish("c8")
				e.deploy("c9", "web", normal)
				e.deploy("c10", "web", normal)
				e.redeploy("c8 again", "c8", urgent)
				e.claim("c8 again")
				e.assertState("c9", core.StateQueued)
				e.assertState("c10", core.StateQueued)
				e.finish("c8 again")
				e.claim("c9")
				e.finish("c9")
				e.claim("c10")
			},
		},
		{
			name:  "urgent redeploy of an older commit still supersedes commits seen before it",
			limit: 10,
			run: func(e *batchEnv) {
				e.deploy("c7", "web", normal)
				e.deploy("c8", "web", normal)
				e.claim("c8", "c7")
				e.finish("c8")
				e.redeploy("c7 again", "c7", normal)
				e.deploy("c9", "web", normal)
				e.redeploy("c8 again", "c8", urgent)
				e.claim("c8 again", "c7 again")
				e.assertState("c9", core.StateQueued)
			},
		},
		{
			name:  "batch deploys the newest commit, not the latest request",
			limit: 10,
			run: func(e *batchEnv) {
				e.deploy("c8", "web", normal)
				e.claim("c8")
				e.finish("c8")
				e.deploy("c9", "web", normal)
				e.deploy("c10", "web", normal)
				e.redeploy("c8 again", "c8", normal)
				e.claim("c10", "c9", "c8 again")
			},
		},
		{
			name:  "urgent deploys batch among themselves",
			limit: 10,
			run: func(e *batchEnv) {
				e.deploy("c1", "web", normal)
				e.deploy("u1", "web", urgent)
				e.deploy("c3", "web", normal)
				e.deploy("u2", "web", urgent)
				e.deploy("c5", "web", normal)
				e.claim("u2", "c1", "u1", "c3")
				e.assertState("c5", core.StateQueued)
			},
		},
		{
			name:  "rollbacks never batch and are never superseded",
			limit: 10,
			run: func(e *batchEnv) {
				e.deploy("c1", "web", normal)
				e.deploy("c2", "web", normal)
				e.rollback("r1", "web", "c1")
				e.rollback("r2", "web", "c1")
				e.claim("r1")
				e.assertState("c1", core.StateQueued)
				e.assertState("c2", core.StateQueued)
				e.exec(completeEntry, "r1")
				e.claim("r2")
				e.exec(completeEntry, "r2")
				e.deploy("u3", "web", urgent)
				e.claim("u3", "c1", "c2")
			},
		},
		{
			name:  "urgent leaves queued rollbacks alone",
			limit: 10,
			run: func(e *batchEnv) {
				e.deploy("c1", "web", normal)
				e.rollback("r1", "web", "c1")
				e.exec(delayEntry, "r1")
				e.deploy("u2", "web", urgent)
				e.claim("")
				e.exec(releaseEntry, "r1")
				e.claim("r1")
				e.assertState("c1", core.StateQueued)
			},
		},
		{
			name:  "resumed in-flight deployment never batches",
			limit: 10,
			run: func(e *batchEnv) {
				e.deploy("c1", "web", normal)
				e.claim("c1")
				e.exec(startEntry, "c1")
				e.exec(expireLease, "c1")
				e.deploy("c2", "web", normal)
				e.deploy("c3", "web", urgent)
				e.claim("c1")
				e.assertState("c2", core.StateQueued)
				e.assertState("c3", core.StateQueued)
			},
		},
		{
			name:  "expired claim of a queued head is batched with newer deploys",
			limit: 10,
			run: func(e *batchEnv) {
				e.deploy("c1", "web", normal)
				e.claim("c1")
				e.exec(expireLease, "c1")
				e.deploy("c2", "web", normal)
				e.claim("c2", "c1")
			},
		},
		{
			name:  "cancelled deployments are not superseded",
			limit: 10,
			run: func(e *batchEnv) {
				e.deploy("c1", "web", normal)
				e.deploy("c2", "web", normal)
				e.deploy("c3", "web", normal)
				e.exec(cancelEntry, "c2")
				e.claim("c3", "c1")
				e.assertState("c2", core.StateCancelled)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(newBatchEnv(t, tt.limit))
		})
	}
}

func TestClaimBatchUsesTheTargetLimit(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	var web, api []core.DeploymentID
	for range 3 {
		web = append(web, mustCreate(t, st, deploymentOf("web", randomSHA(t)), 0).ID)
		api = append(api, mustCreate(t, st, deploymentOf("api", randomSHA(t)), 0).ID)
	}
	limits := map[string]int{"web": 2, "api": 3}
	var seen []core.Target
	limit := func(target core.Target) int {
		seen = append(seen, target)
		return limits[target.App]
	}
	want := map[string]core.DeploymentID{"web": web[1], "api": api[2]}
	for range 2 {
		got, ok, err := st.ClaimBatch(t.Context(), "w1", testLease, limit)
		if err != nil || !ok {
			t.Fatalf("ClaimBatch = %v, %v", ok, err)
		}
		if got.Deployment.ID != want[got.Deployment.Target.App] {
			t.Errorf("claimed %d for %s, want %d", got.Deployment.ID, got.Deployment.Target, want[got.Deployment.Target.App])
		}
	}
	if len(seen) != 2 || seen[0].Environment != "prod" {
		t.Errorf("limit called with %v", seen)
	}
}

func TestClaimBatchRejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	mustCreate(t, st, newDeployment("web"), 0)
	tests := []struct {
		name   string
		worker string
		lease  time.Duration
	}{
		{name: "empty worker", worker: "", lease: time.Minute},
		{name: "zero lease", worker: "w1", lease: 0},
	}
	for _, tt := range tests {
		if _, ok, err := st.ClaimBatch(t.Context(), tt.worker, tt.lease, nil); err == nil || ok {
			t.Errorf("%s: ClaimBatch = %v, %v; want error", tt.name, ok, err)
		}
	}
	got, ok, err := st.ClaimBatch(t.Context(), "w1", testLease, nil)
	if err != nil || !ok || len(got.Superseded) != 0 {
		t.Errorf("ClaimBatch with nil limit = %+v, %v, %v", got, ok, err)
	}
}

func TestClaimBatchConcurrent(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	const apps, perApp, workers = 4, 5, 16
	newest := map[string]core.DeploymentID{}
	for i := range apps * perApp {
		app := fmt.Sprintf("app-%d", i%apps)
		d, _, err := st.EnqueueDeployment(t.Context(), deploymentOf(app, randomSHA(t)), core.PriorityNormal)
		if err != nil {
			t.Fatalf("EnqueueDeployment: %v", err)
		}
		newest[app] = d.ID
	}

	var mu sync.Mutex
	claimed := map[string][]core.DeploymentID{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := range workers {
		wg.Go(func() {
			<-start
			got, ok, err := st.ClaimBatch(t.Context(), fmt.Sprintf("worker-%d", w), testLease, func(core.Target) int { return 10 })
			if err != nil {
				t.Errorf("ClaimBatch: %v", err)
				return
			}
			if ok {
				mu.Lock()
				claimed[got.Deployment.Target.App] = append(claimed[got.Deployment.Target.App], got.Deployment.ID)
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()

	if len(claimed) != apps {
		t.Errorf("claimed work for %d apps, want %d", len(claimed), apps)
	}
	for app, ids := range claimed {
		if len(ids) != 1 || ids[0] != newest[app] {
			t.Errorf("%s claimed %v, want only %d", app, ids, newest[app])
		}
	}
	if n := queryInt(t, st, "SELECT count(*) FROM deployments WHERE state = 'superseded'"); n != apps*(perApp-1) {
		t.Errorf("%d superseded deployments, want %d", n, apps*(perApp-1))
	}
	if n := queryInt(t, st, "SELECT count(*) FROM queue_entries"); n != apps {
		t.Errorf("%d queue entries left, want %d", n, apps)
	}
}
