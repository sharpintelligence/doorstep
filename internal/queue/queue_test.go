package queue_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/queue"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func randomSHA(t *testing.T) string {
	t.Helper()
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random SHA: %v", err)
	}
	return hex.EncodeToString(b)
}

func deployment(t *testing.T, app string) core.Deployment {
	t.Helper()
	sha := randomSHA(t)
	return core.Deployment{
		Target:      core.Target{App: app, Environment: "prod"},
		Kind:        core.KindDeploy,
		Source:      core.SourceWebhook,
		RequestedBy: "octocat",
		Commit:      core.Commit{Repository: "acme/" + app, SHA: sha},
		Image:       core.ImageRef{Repository: "ghcr.io/acme/" + app, Tag: sha},
	}
}

func TestPriorityFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind   core.Kind
		urgent bool
		want   core.Priority
	}{
		{core.KindDeploy, false, core.PriorityNormal},
		{core.KindDeploy, true, core.PriorityUrgent},
		{core.KindRollback, false, core.PriorityRollback},
		{core.KindRollback, true, core.PriorityRollback},
	}
	for _, tt := range tests {
		if got := queue.PriorityFor(tt.kind, tt.urgent); got != tt.want {
			t.Errorf("PriorityFor(%s, %v) = %s, want %s", tt.kind, tt.urgent, got, tt.want)
		}
	}
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		lease         time.Duration
		wantLease     time.Duration
		wantHeartbeat time.Duration
	}{
		{name: "default", wantLease: queue.DefaultLease, wantHeartbeat: queue.DefaultLease / 3},
		{name: "negative", lease: -time.Second, wantLease: queue.DefaultLease, wantHeartbeat: queue.DefaultLease / 3},
		{name: "custom", lease: 90 * time.Second, wantLease: 90 * time.Second, wantHeartbeat: 30 * time.Second},
	}
	for _, tt := range tests {
		q := queue.New(queue.Options{Lease: tt.lease})
		if q.Lease() != tt.wantLease || q.HeartbeatInterval() != tt.wantHeartbeat {
			t.Errorf("%s: lease %s, heartbeat %s; want %s, %s", tt.name, q.Lease(), q.HeartbeatInterval(), tt.wantLease, tt.wantHeartbeat)
		}
	}
}

func TestNewWorkerID(t *testing.T) {
	t.Parallel()

	a, err := queue.NewWorkerID()
	if err != nil {
		t.Fatalf("NewWorkerID: %v", err)
	}
	b, err := queue.NewWorkerID()
	if err != nil {
		t.Fatalf("NewWorkerID: %v", err)
	}
	pattern := regexp.MustCompile(`^\S+-[0-9a-f]{8}$`)
	if !pattern.MatchString(a) || a == b {
		t.Errorf("worker IDs %q and %q, want distinct host-suffix IDs", a, b)
	}
}

func TestQueueBatchesPerApp(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	q := queue.New(queue.Options{Store: st, MaxBatch: func(app string) int {
		if app == "web" {
			return 3
		}
		return 1
	}})
	ctx := t.Context()
	var web, api []core.DeploymentID
	for range 4 {
		r, err := q.Enqueue(ctx, deployment(t, "web"), queue.EnqueueOptions{})
		if err != nil || !r.Created {
			t.Fatalf("Enqueue(web) = %+v, %v", r, err)
		}
		web = append(web, r.Deployment.ID)
		r, err = q.Enqueue(ctx, deployment(t, "api"), queue.EnqueueOptions{})
		if err != nil || !r.Created {
			t.Fatalf("Enqueue(api) = %+v, %v", r, err)
		}
		api = append(api, r.Deployment.ID)
	}

	want := map[string]struct {
		id         core.DeploymentID
		superseded int
	}{
		"web": {id: web[2], superseded: 2},
		"api": {id: api[0], superseded: 0},
	}
	for range 2 {
		c, ok, err := q.Claim(ctx, "w1")
		if err != nil || !ok {
			t.Fatalf("Claim = %v, %v", ok, err)
		}
		w := want[c.Deployment.Target.App]
		if c.Deployment.ID != w.id || len(c.Superseded) != w.superseded || c.Attempt != 1 {
			t.Errorf("claimed %+v for %s, want deployment %d superseding %d", c, c.Deployment.Target, w.id, w.superseded)
		}
	}
	depth, err := q.Depth(ctx)
	if err != nil || depth != 4 {
		t.Errorf("Depth = %d, %v; want 4", depth, err)
	}
}

func TestQueueEnqueueUrgentAndExpedite(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	q := queue.New(queue.Options{Store: st})
	ctx := t.Context()
	d := deployment(t, "web")
	first, err := q.Enqueue(ctx, d, queue.EnqueueOptions{})
	if err != nil || !first.Created {
		t.Fatalf("Enqueue = %+v, %v", first, err)
	}
	again, err := q.Enqueue(ctx, d, queue.EnqueueOptions{Urgent: true})
	if err != nil || again.Created || again.Deployment.ID != first.Deployment.ID {
		t.Fatalf("urgent Enqueue of the same commit = %+v, %v", again, err)
	}
	other, err := q.Enqueue(ctx, deployment(t, "web"), queue.EnqueueOptions{})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q.Expedite(ctx, other.Deployment.ID, "alice"); err != nil {
		t.Fatalf("Expedite: %v", err)
	}
	if err := q.Expedite(ctx, 999999, "alice"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Expedite(unknown) = %v, want ErrNotFound", err)
	}

	entries, err := q.Entries(ctx, "web")
	if err != nil || len(entries) != 2 {
		t.Fatalf("Entries = %+v, %v", entries, err)
	}
	for _, e := range entries {
		if e.Priority != core.PriorityUrgent {
			t.Errorf("entry %d priority = %s, want urgent", e.Deployment.ID, e.Priority)
		}
	}
	if entries[0].Deployment.ID != first.Deployment.ID {
		t.Errorf("first entry = %d, want %d", entries[0].Deployment.ID, first.Deployment.ID)
	}
}

func TestQueueLeaseLifecycle(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	q := queue.New(queue.Options{Store: st, Lease: time.Minute})
	ctx := t.Context()
	r, err := q.Enqueue(ctx, deployment(t, "web"), queue.EnqueueOptions{})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	id := r.Deployment.ID
	c, ok, err := q.Claim(ctx, "w1")
	if err != nil || !ok || c.Deployment.ID != id {
		t.Fatalf("Claim = %+v, %v, %v", c, ok, err)
	}
	if expires, err := q.Heartbeat(ctx, id, "w1"); err != nil || time.Until(expires) < 30*time.Second {
		t.Errorf("Heartbeat = %v, %v", expires, err)
	}
	if err := q.Release(ctx, id, "w1", -time.Second); err != nil {
		t.Errorf("Release with a negative delay = %v, want it treated as none", err)
	}
	if _, err := q.Heartbeat(ctx, id, "w1"); !errors.Is(err, core.ErrLeaseLost) {
		t.Errorf("Heartbeat after release = %v, want ErrLeaseLost", err)
	}
	c, ok, err = q.Claim(ctx, "w2")
	if err != nil || !ok || c.Attempt != 2 {
		t.Fatalf("Claim after release = %+v, %v, %v", c, ok, err)
	}
	if err := q.Complete(ctx, id, "w1"); !errors.Is(err, core.ErrLeaseLost) {
		t.Errorf("Complete by the old owner = %v, want ErrLeaseLost", err)
	}
	if err := q.Complete(ctx, id, "w2"); err != nil {
		t.Errorf("Complete: %v", err)
	}
	if entries, err := q.Entries(ctx, "web"); err != nil || len(entries) != 0 {
		t.Errorf("Entries after complete = %+v, %v", entries, err)
	}
}

func TestRunnerEndToEnd(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	q := queue.New(queue.Options{Store: st, Lease: 2 * time.Second, MaxBatch: func(string) int { return 10 }})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	handled := make(chan queue.Claimed, 10)
	runner := queue.NewRunner(queue.RunnerOptions{
		Source:            q,
		Listen:            q.Listen,
		WorkerID:          "runner-1",
		PollInterval:      time.Hour,
		HeartbeatInterval: q.HeartbeatInterval(),
	})
	done := make(chan error, 1)
	go func() {
		done <- runner.Run(ctx, func(ctx context.Context, c queue.Claimed) (queue.Result, error) {
			if err := q.Complete(ctx, c.Deployment.ID, "runner-1"); err != nil {
				return queue.Result{}, err
			}
			handled <- c
			return queue.Result{}, nil
		})
	}()

	time.Sleep(200 * time.Millisecond)
	r, err := q.Enqueue(ctx, deployment(t, "web"), queue.EnqueueOptions{})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	select {
	case c := <-handled:
		if c.Deployment.ID != r.Deployment.ID {
			t.Errorf("handled %d, want %d", c.Deployment.ID, r.Deployment.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not handle the enqueued deployment; notification wake-up missing")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestRunnerRecoversCrashedWorker(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	q := queue.New(queue.Options{Store: st, Lease: time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	r, err := q.Enqueue(ctx, deployment(t, "web"), queue.EnqueueOptions{})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, ok, err := q.Claim(ctx, "crashed"); err != nil || !ok {
		t.Fatalf("Claim = %v, %v", ok, err)
	}

	var mu sync.Mutex
	var got queue.Claimed
	handled := make(chan struct{})
	runner := queue.NewRunner(queue.RunnerOptions{
		Source:            q,
		WorkerID:          "survivor",
		PollInterval:      100 * time.Millisecond,
		HeartbeatInterval: q.HeartbeatInterval(),
	})
	done := make(chan error, 1)
	go func() {
		done <- runner.Run(ctx, func(ctx context.Context, c queue.Claimed) (queue.Result, error) {
			mu.Lock()
			got = c
			mu.Unlock()
			if err := q.Complete(ctx, c.Deployment.ID, "survivor"); err != nil {
				return queue.Result{}, err
			}
			close(handled)
			return queue.Result{}, nil
		})
	}()
	select {
	case <-handled:
	case <-time.After(15 * time.Second):
		t.Fatal("survivor never took over the expired lease")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.Deployment.ID != r.Deployment.ID || got.Attempt != 2 {
		t.Errorf("survivor handled %d at attempt %d, want %d at attempt 2", got.Deployment.ID, got.Attempt, r.Deployment.ID)
	}
}

var _ queue.Source = (*queue.Queue)(nil)
