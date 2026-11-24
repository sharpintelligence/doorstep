package store_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

const (
	testLease = time.Minute
	none      = -1
)

type claimEnv struct {
	t   *testing.T
	st  *store.Store
	ids []core.DeploymentID
}

func (e *claimEnv) enqueue(app string, priority int) {
	e.t.Helper()
	e.ids = append(e.ids, mustCreate(e.t, e.st, newDeployment(app), priority).ID)
}

func (e *claimEnv) exec(sql string, entry int) {
	e.t.Helper()
	mustExec(e.t, e.st, sql, e.ids[entry])
}

func (e *claimEnv) expectClaim(worker string, want, wantAttempt int) {
	e.t.Helper()
	before := time.Now()
	d, c, ok, err := e.st.Claim(e.t.Context(), worker, testLease)
	if err != nil {
		e.t.Fatalf("Claim: %v", err)
	}
	if want == none {
		if ok {
			e.t.Fatalf("Claim returned deployment %d (%s), want nothing", d.ID, d.Target)
		}
		return
	}
	if !ok {
		e.t.Fatalf("Claim returned nothing, want entry %d (deployment %d)", want, e.ids[want])
	}
	if d.ID != e.ids[want] {
		e.t.Fatalf("Claim returned deployment %d, want entry %d (deployment %d)", d.ID, want, e.ids[want])
	}
	if c.Attempt != wantAttempt {
		e.t.Errorf("Attempt = %d, want %d", c.Attempt, wantAttempt)
	}
	if c.LeaseExpiresAt.Before(before.Add(testLease-5*time.Second)) || c.LeaseExpiresAt.After(time.Now().Add(testLease+5*time.Second)) {
		e.t.Errorf("LeaseExpiresAt = %v, want about %v from now", c.LeaseExpiresAt, testLease)
	}
	owner := ""
	if err := store.QueryRowSQL(e.t.Context(), e.st, "SELECT lease_owner FROM queue_entries WHERE deployment_id = $1", d.ID).Scan(&owner); err != nil {
		e.t.Fatalf("read lease owner: %v", err)
	}
	if owner != worker {
		e.t.Errorf("lease_owner = %q, want %q", owner, worker)
	}
}

const (
	completeEntry = "DELETE FROM queue_entries WHERE deployment_id = $1"
	expireLease   = "UPDATE queue_entries SET lease_expires_at = now() - interval '1 second' WHERE deployment_id = $1"
	delayEntry    = "UPDATE queue_entries SET available_at = now() + interval '1 hour' WHERE deployment_id = $1"
	releaseEntry  = "UPDATE queue_entries SET available_at = now() - interval '1 second' WHERE deployment_id = $1"
	cancelEntry   = "UPDATE deployments SET state = 'cancelled' WHERE id = $1"
	startEntry    = "UPDATE deployments SET state = 'resolving', started_at = now() WHERE id = $1"

	succeedDeployment = "UPDATE deployments SET state = 'succeeded', finished_at = now() WHERE id = $1"
)

func TestClaim(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(e *claimEnv)
	}{
		{
			name: "empty queue",
			run: func(e *claimEnv) {
				e.expectClaim("w1", none, 0)
			},
		},
		{
			name: "fifo within an app",
			run: func(e *claimEnv) {
				e.enqueue("web", 0)
				e.enqueue("web", 0)
				e.enqueue("web", 0)
				e.expectClaim("w1", 0, 1)
				e.exec(completeEntry, 0)
				e.expectClaim("w1", 1, 1)
				e.exec(completeEntry, 1)
				e.expectClaim("w1", 2, 1)
				e.exec(completeEntry, 2)
				e.expectClaim("w1", none, 0)
			},
		},
		{
			name: "priority ahead of id",
			run: func(e *claimEnv) {
				e.enqueue("web", 0)
				e.enqueue("web", 10)
				e.enqueue("web", 5)
				e.expectClaim("w1", 1, 1)
				e.exec(completeEntry, 1)
				e.expectClaim("w1", 2, 1)
				e.exec(completeEntry, 2)
				e.expectClaim("w1", 0, 1)
			},
		},
		{
			name: "global order prefers priority then id across apps",
			run: func(e *claimEnv) {
				e.enqueue("web", 0)
				e.enqueue("api", 0)
				e.enqueue("db", 3)
				e.expectClaim("w1", 2, 1)
				e.expectClaim("w2", 0, 1)
				e.expectClaim("w3", 1, 1)
			},
		},
		{
			name: "second entry blocked while first is leased",
			run: func(e *claimEnv) {
				e.enqueue("web", 0)
				e.enqueue("web", 0)
				e.expectClaim("w1", 0, 1)
				e.expectClaim("w2", none, 0)
			},
		},
		{
			name: "entry behind an unavailable head waits",
			run: func(e *claimEnv) {
				e.enqueue("web", 0)
				e.enqueue("web", 0)
				e.exec(delayEntry, 0)
				e.expectClaim("w1", none, 0)
				e.exec(releaseEntry, 0)
				e.expectClaim("w1", 0, 1)
			},
		},
		{
			name: "other apps claimable in parallel",
			run: func(e *claimEnv) {
				e.enqueue("web", 0)
				e.enqueue("web", 0)
				e.enqueue("api", 0)
				e.expectClaim("w1", 0, 1)
				e.expectClaim("w2", 2, 1)
				e.expectClaim("w3", none, 0)
			},
		},
		{
			name: "same environment name in different apps is independent",
			run: func(e *claimEnv) {
				e.enqueue("web", 0)
				e.enqueue("worker", 0)
				e.expectClaim("w1", 0, 1)
				e.expectClaim("w1", 1, 1)
			},
		},
		{
			name: "future available_at not claimable",
			run: func(e *claimEnv) {
				e.enqueue("web", 0)
				e.exec(delayEntry, 0)
				e.expectClaim("w1", none, 0)
				e.exec(releaseEntry, 0)
				e.expectClaim("w1", 0, 1)
			},
		},
		{
			name: "expired lease is reclaimed with attempts incremented",
			run: func(e *claimEnv) {
				e.enqueue("web", 0)
				e.enqueue("web", 0)
				e.expectClaim("w1", 0, 1)
				e.expectClaim("w2", none, 0)
				e.exec(expireLease, 0)
				e.expectClaim("w2", 0, 2)
				e.exec(expireLease, 0)
				e.expectClaim("w3", 0, 3)
			},
		},
		{
			name: "high priority entry waits for a live lease in its app",
			run: func(e *claimEnv) {
				e.enqueue("web", 0)
				e.expectClaim("w1", 0, 1)
				e.enqueue("web", 100)
				e.expectClaim("w2", none, 0)
				e.exec(expireLease, 0)
				e.expectClaim("w2", 1, 1)
				e.expectClaim("w3", none, 0)
			},
		},
		{
			name: "terminal entry is skipped and does not hold the line",
			run: func(e *claimEnv) {
				e.enqueue("web", 10)
				e.enqueue("web", 0)
				e.exec(cancelEntry, 0)
				e.expectClaim("w1", 1, 1)
				e.expectClaim("w2", none, 0)
			},
		},
		{
			name: "terminal entry with a live lease still blocks its app",
			run: func(e *claimEnv) {
				e.enqueue("web", 0)
				e.expectClaim("w1", 0, 1)
				e.exec(cancelEntry, 0)
				e.enqueue("web", 0)
				e.enqueue("api", 0)
				e.expectClaim("w2", 2, 1)
				e.expectClaim("w2", none, 0)
				e.exec(expireLease, 0)
				e.expectClaim("w2", 1, 1)
			},
		},
		{
			name: "expired terminal entry is never reclaimed",
			run: func(e *claimEnv) {
				e.enqueue("web", 0)
				e.expectClaim("w1", 0, 1)
				e.exec(cancelEntry, 0)
				e.exec(expireLease, 0)
				e.expectClaim("w2", none, 0)
			},
		},
		{
			name: "in-flight deployment outranks a higher priority entry after its lease expires",
			run: func(e *claimEnv) {
				e.enqueue("web", 0)
				e.expectClaim("w1", 0, 1)
				e.exec(startEntry, 0)
				e.exec(expireLease, 0)
				e.enqueue("web", 100)
				e.enqueue("web", 200)
				e.expectClaim("w2", 0, 2)
				e.expectClaim("w3", none, 0)
			},
		},
		{
			name: "in-flight deployments are resumed before queued work of other apps",
			run: func(e *claimEnv) {
				e.enqueue("web", 0)
				e.enqueue("api", 200)
				e.expectClaim("w1", 1, 1)
				e.expectClaim("w1", 0, 1)
				e.exec(startEntry, 0)
				e.exec(expireLease, 0)
				e.exec(completeEntry, 1)
				e.enqueue("db", 200)
				e.expectClaim("w2", 0, 2)
				e.expectClaim("w2", 2, 1)
			},
		},
		{
			name: "high priority entry claimable once the leased entry completes",
			run: func(e *claimEnv) {
				e.enqueue("web", 0)
				e.enqueue("web", 0)
				e.expectClaim("w1", 0, 1)
				e.enqueue("web", 100)
				e.expectClaim("w2", none, 0)
				e.exec(completeEntry, 0)
				e.expectClaim("w2", 2, 1)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(&claimEnv{t: t, st: storetest.Open(t)})
		})
	}
}

func TestClaimTakesOnlyClaimableStates(t *testing.T) {
	t.Parallel()

	for _, state := range core.States() {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			d := mustCreate(t, st, newDeployment("web"), 0)
			mustExec(t, st, "UPDATE deployments SET state = $2 WHERE id = $1", d.ID, state)
			got, _, ok, err := st.Claim(t.Context(), "w1", testLease)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if ok != state.Claimable() {
				t.Fatalf("Claim of a %s deployment = %v, want %v", state, ok, state.Claimable())
			}
			if ok && got.ID != d.ID {
				t.Errorf("Claim returned deployment %d, want %d", got.ID, d.ID)
			}
		})
	}
}

func TestClaimExcludesContendedApps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		exclude []string
		want    string
	}{
		{name: "nothing excluded", exclude: nil, want: "web"},
		{name: "head app excluded", exclude: []string{"web"}, want: "api"},
		{name: "every app excluded", exclude: []string{"web", "api"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			mustCreate(t, st, newDeployment("web"), 1)
			mustCreate(t, st, newDeployment("api"), 0)
			excluded := []store.AppID{}
			for _, app := range tt.exclude {
				id, err := st.EnsureApp(t.Context(), core.Target{App: app, Environment: "prod"})
				if err != nil {
					t.Fatalf("EnsureApp: %v", err)
				}
				excluded = append(excluded, id)
			}
			d, ok, err := store.ClaimExcluding(t.Context(), st, "w1", testLease, excluded)
			if err != nil {
				t.Fatalf("ClaimExcluding: %v", err)
			}
			if ok != (tt.want != "") || d.Target.App != tt.want {
				t.Errorf("ClaimExcluding = %q, %v; want %q", d.Target.App, ok, tt.want)
			}
		})
	}
}

func TestClaimRejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	mustCreate(t, st, newDeployment("web"), 0)
	tests := []struct {
		name   string
		worker string
		lease  time.Duration
	}{
		{name: "empty worker", worker: "", lease: time.Minute},
		{name: "blank worker", worker: "  ", lease: time.Minute},
		{name: "zero lease", worker: "w1", lease: 0},
		{name: "negative lease", worker: "w1", lease: -time.Second},
	}
	for _, tt := range tests {
		_, _, ok, err := st.Claim(t.Context(), tt.worker, tt.lease)
		if err == nil || ok {
			t.Errorf("%s: Claim(%q, %s) = ok %v, err %v; want error", tt.name, tt.worker, tt.lease, ok, err)
		}
	}
	if n := queryInt(t, st, "SELECT count(*) FROM queue_entries WHERE lease_owner IS NOT NULL"); n != 0 {
		t.Errorf("rejected claims leased %d entries", n)
	}
}

func TestClaimInsideTransactionRollsBack(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	created := mustCreate(t, st, newDeployment("web"), 0)
	err := st.InTx(t.Context(), func(tx *store.Store) error {
		d, _, ok, err := tx.Claim(t.Context(), "w1", testLease)
		if err != nil || !ok || d.ID != created.ID {
			t.Fatalf("Claim in transaction = %d, %v, %v", d.ID, ok, err)
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("InTx = %v", err)
	}
	d, c, ok, err := st.Claim(t.Context(), "w2", testLease)
	if err != nil || !ok || d.ID != created.ID || c.Attempt != 1 {
		t.Errorf("Claim after rollback = %d, %+v, %v, %v", d.ID, c, ok, err)
	}
}

var errRollback = errors.New("rollback")

func TestClaimConcurrent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		apps       int
		perApp     int
		workers    int
		priorities func(i int) int
	}{
		{name: "fifo", apps: 4, perApp: 3, workers: 16, priorities: func(int) int { return 0 }},
		{name: "mixed priorities", apps: 3, perApp: 4, workers: 12, priorities: func(i int) int { return (i * 7) % 3 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			head := map[core.Target]core.DeploymentID{}
			headPriority := map[core.Target]int{}
			for i := range tt.apps * tt.perApp {
				priority := tt.priorities(i)
				d := mustCreate(t, st, newDeployment(fmt.Sprintf("app-%d", i%tt.apps)), priority)
				if best, ok := headPriority[d.Target]; !ok || priority > best {
					head[d.Target] = d.ID
					headPriority[d.Target] = priority
				}
			}

			type result struct {
				d   core.Deployment
				ok  bool
				err error
			}
			results := make(chan result, tt.workers)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for w := range tt.workers {
				wg.Go(func() {
					<-start
					d, _, ok, err := st.Claim(t.Context(), fmt.Sprintf("worker-%d", w), testLease)
					results <- result{d: d, ok: ok, err: err}
				})
			}
			close(start)
			wg.Wait()
			close(results)

			claimed := map[core.DeploymentID]bool{}
			perTarget := map[core.Target]int{}
			for r := range results {
				if r.err != nil {
					t.Errorf("Claim: %v", r.err)
					continue
				}
				if !r.ok {
					continue
				}
				if claimed[r.d.ID] {
					t.Errorf("deployment %d claimed twice", r.d.ID)
				}
				claimed[r.d.ID] = true
				perTarget[r.d.Target]++
				if head[r.d.Target] != r.d.ID {
					t.Errorf("claimed deployment %d for %s, head of line is %d", r.d.ID, r.d.Target, head[r.d.Target])
				}
			}
			if len(claimed) != tt.apps {
				t.Errorf("%d claims succeeded, want %d", len(claimed), tt.apps)
			}
			for target, n := range perTarget {
				if n != 1 {
					t.Errorf("%s claimed %d times", target, n)
				}
			}
			live := queryInt(t, st, `SELECT count(*) FROM (
				SELECT app_id FROM queue_entries WHERE lease_expires_at > now() GROUP BY app_id HAVING count(*) > 1
			) contended`)
			if live != 0 {
				t.Errorf("%d apps hold more than one live lease", live)
			}
		})
	}
}

func TestClaimConcurrentWithLateHighPriorityEntries(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	const apps = 4
	for i := range apps {
		mustCreate(t, st, newDeployment(fmt.Sprintf("app-%d", i)), 0)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range apps {
		wg.Go(func() {
			<-start
			if _, err := st.CreateDeployment(t.Context(), newDeployment(fmt.Sprintf("app-%d", i)), 100); err != nil {
				t.Errorf("CreateDeployment: %v", err)
			}
		})
	}
	for w := range 4 * apps {
		wg.Go(func() {
			<-start
			if _, _, _, err := st.Claim(t.Context(), fmt.Sprintf("worker-%d", w), testLease); err != nil {
				t.Errorf("Claim: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()

	live := queryInt(t, st, `SELECT count(*) FROM (
		SELECT app_id FROM queue_entries WHERE lease_expires_at > now() GROUP BY app_id HAVING count(*) > 1
	) contended`)
	if live != 0 {
		t.Errorf("%d apps hold more than one live lease", live)
	}
}
