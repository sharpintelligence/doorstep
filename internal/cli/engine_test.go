package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/engine"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
	"github.com/sharpintelligence/doorstep/internal/gitops"
	"github.com/sharpintelligence/doorstep/internal/intake"
	"github.com/sharpintelligence/doorstep/internal/metrics"
	"github.com/sharpintelligence/doorstep/internal/registry"
	"github.com/sharpintelligence/doorstep/internal/rollback"
)

const (
	adapterSHA    = "0123456789abcdef0123456789abcdef01234567"
	adapterDigest = core.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111")
)

type fakeCommitResolver struct {
	digest registry.Digest
	err    error
	calls  []string
}

func (f *fakeCommitResolver) ResolveCommit(_ context.Context, repository, sha string) (registry.Digest, error) {
	f.calls = append(f.calls, repository+":"+sha)
	return f.digest, f.err
}

func TestDigestResolver(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		digest     registry.Digest
		err        error
		want       core.Digest
		wantIs     []error
		wantNot    []error
		wantWarned bool
	}{
		{name: "resolved", digest: registry.Digest(adapterDigest), want: adapterDigest},
		{name: "malformed digest", digest: "md5:abc", wantIs: []error{engine.ErrPermanent}},
		{name: "invalid reference", err: fmt.Errorf("resolve: %w", registry.ErrInvalidReference), wantIs: []error{engine.ErrPermanent, registry.ErrInvalidReference}},
		{name: "not found", err: registry.ErrNotFound, wantIs: []error{engine.ErrImageNotReady, registry.ErrNotFound}, wantNot: []error{engine.ErrPermanent}},
		{name: "denied", err: registry.ErrDenied, wantIs: []error{engine.ErrImageNotReady, registry.ErrDenied}, wantWarned: true},
		{name: "transient", err: errors.New("connection reset"), wantNot: []error{engine.ErrPermanent, engine.ErrImageNotReady}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs strings.Builder
			fake := &fakeCommitResolver{digest: tt.digest, err: tt.err}
			r := digestResolver{resolver: fake, logger: slog.New(slog.NewTextHandler(&logs, nil))}
			got, err := r.Resolve(t.Context(), core.ImageRef{Repository: "ghcr.io/example-user/blog", Tag: adapterSHA})
			if got != tt.want {
				t.Errorf("Resolve() = %q, want %q", got, tt.want)
			}
			if (err != nil) != (tt.want == "") {
				t.Errorf("Resolve() error = %v", err)
			}
			for _, target := range tt.wantIs {
				if !errors.Is(err, target) {
					t.Errorf("error %v is not %v", err, target)
				}
			}
			for _, target := range tt.wantNot {
				if errors.Is(err, target) {
					t.Errorf("error %v is %v", err, target)
				}
			}
			if !reflect.DeepEqual(fake.calls, []string{"ghcr.io/example-user/blog:" + adapterSHA}) {
				t.Errorf("calls = %v", fake.calls)
			}
			if warned := strings.Contains(logs.String(), "level=WARN"); warned != tt.wantWarned {
				t.Errorf("warned = %t, want %t: %s", warned, tt.wantWarned, logs.String())
			}
		})
	}
}

type fakeGitOps struct {
	mu       sync.Mutex
	result   gitops.Result
	err      error
	requests []gitops.Request
	abandons []gitops.Request
	pinned   gitops.Pinned
	current  []gitops.Target
}

func (f *fakeGitOps) Current(_ context.Context, t gitops.Target) (gitops.Pinned, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current = append(f.current, t)
	return f.pinned, f.err
}

func (f *fakeGitOps) Pin(_ context.Context, req gitops.Request) (gitops.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	return f.result, f.err
}

func (f *fakeGitOps) Abandon(_ context.Context, req gitops.Request) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abandons = append(f.abandons, req)
	return f.err
}

func TestPinWriter(t *testing.T) {
	t.Parallel()

	target := gitops.Target{App: "blog", Owner: "example-user", Repo: "homelab", Branch: "main", Dir: "apps/blog", ImageRepository: "ghcr.io/example-user/blog", Kustomize: &gitops.KustomizeImage{Name: "ghcr.io/example-user/blog"}, Mode: gitops.ModeCommit}
	rollbackOf := core.DeploymentID(3)
	d := core.Deployment{ID: 7, Target: core.AppTarget("blog"), Kind: core.KindRollback, RollbackOfID: &rollbackOf, Commit: core.Commit{Repository: "example-user/blog", SHA: adapterSHA}}
	req := engine.PinRequest{Deployment: d, Digest: adapterDigest, Blocks: []core.Digest{"sha256:" + core.Digest(strings.Repeat("2", 64))}, Unblocks: []core.Digest{"sha256:" + core.Digest(strings.Repeat("3", 64))}, Repin: true}
	wantRequest := gitops.Request{
		Target:     target,
		Digest:     adapterDigest,
		Deployment: 7,
		Kind:       core.KindRollback,
		AppCommit:  d.Commit,
		RollbackOf: &rollbackOf,
		Blocks:     req.Blocks,
		Unblocks:   req.Unblocks,
		Repin:      true,
	}
	commit := core.Commit{Repository: "example-user/homelab", SHA: strings.Repeat("c", 40)}
	fake := &fakeGitOps{
		result: gitops.Result{Commit: commit, Previous: "sha256:" + core.Digest(strings.Repeat("4", 64)), Changed: true},
	}
	p := pinWriter{writer: fake, targets: map[core.Target]gitops.Target{core.AppTarget("blog"): target}}

	res, err := p.Pin(t.Context(), req)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if res != (engine.PinResult{Commit: commit, Previous: fake.result.Previous, Changed: true}) {
		t.Errorf("Pin() = %+v", res)
	}
	if !reflect.DeepEqual(fake.requests, []gitops.Request{wantRequest}) {
		t.Errorf("gitops requests = %+v, want %+v", fake.requests, wantRequest)
	}
	if err := p.Abandon(t.Context(), req); err != nil {
		t.Errorf("Abandon: %v", err)
	}
	if !reflect.DeepEqual(fake.abandons, []gitops.Request{wantRequest}) {
		t.Errorf("abandons = %+v", fake.abandons)
	}

	unknown := req
	unknown.Deployment.Target = core.AppTarget("other")
	if _, err := p.Pin(t.Context(), unknown); !errors.Is(err, engine.ErrPermanent) {
		t.Errorf("Pin(unknown app) error = %v, want ErrPermanent", err)
	}
	if err := p.Abandon(t.Context(), unknown); !errors.Is(err, engine.ErrPermanent) {
		t.Errorf("Abandon(unknown app) error = %v, want ErrPermanent", err)
	}

	fake.err = gitops.ErrBranchProtected
	if _, err := p.Pin(t.Context(), req); !errors.Is(err, engine.ErrPermanent) || !errors.Is(err, gitops.ErrBranchProtected) {
		t.Errorf("Pin() error = %v, want ErrPermanent wrapping ErrBranchProtected", err)
	}
}

func TestPinWriterCurrent(t *testing.T) {
	t.Parallel()

	target := gitops.Target{App: "blog", Owner: "example-user", Repo: "homelab", Branch: "main", Dir: "apps/blog", ImageRepository: "ghcr.io/example-user/blog", Kustomize: &gitops.KustomizeImage{Name: "ghcr.io/example-user/blog"}, Mode: gitops.ModeCommit}
	tests := []struct {
		name          string
		app           string
		pinned        gitops.Pinned
		err           error
		want          core.Digest
		wantPermanent bool
		wantCalls     int
	}{
		{name: "pinned digest", app: "blog", pinned: gitops.Pinned{Digest: adapterDigest, Value: string(adapterDigest)}, want: adapterDigest, wantCalls: 1},
		{name: "not a digest", app: "blog", pinned: gitops.Pinned{Value: "v1.2.3"}, wantCalls: 1},
		{name: "pin file gone", app: "blog", err: gitops.ErrPinNotFound, wantPermanent: true, wantCalls: 1},
		{name: "transient", app: "blog", err: &github.APIError{StatusCode: 502}, wantCalls: 1},
		{name: "unknown app", app: "other", wantPermanent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeGitOps{pinned: tt.pinned, err: tt.err}
			p := pinWriter{writer: fake, targets: map[core.Target]gitops.Target{core.AppTarget("blog"): target}}
			got, err := p.Current(t.Context(), core.Deployment{ID: 7, Target: core.AppTarget(tt.app)})
			if (err != nil) != (tt.err != nil || tt.wantPermanent) {
				t.Fatalf("Current() error = %v", err)
			}
			if errors.Is(err, engine.ErrPermanent) != tt.wantPermanent {
				t.Errorf("Current() error = %v, want permanent %v", err, tt.wantPermanent)
			}
			if got != tt.want {
				t.Errorf("Current() = %q, want %q", got, tt.want)
			}
			if len(fake.current) != tt.wantCalls || (tt.wantCalls > 0 && !reflect.DeepEqual(fake.current[0], target)) {
				t.Errorf("gitops Current calls = %+v", fake.current)
			}
		})
	}
}

func TestClassifyPinError(t *testing.T) {
	t.Parallel()

	permanent := []error{
		gitops.ErrInvalidRequest,
		gitops.ErrPinNotFound,
		gitops.ErrAmbiguousPin,
		gitops.ErrUnsupportedYAML,
		gitops.ErrBranchProtected,
		gitops.ErrPinOverwritten,
		gitops.ErrPullRequestClosed,
		github.ErrFileTooLarge,
		github.ErrNotAFile,
	}
	for _, sentinel := range permanent {
		t.Run(sentinel.Error(), func(t *testing.T) {
			t.Parallel()
			err := classifyPinError(fmt.Errorf("pin: %w", sentinel))
			if !errors.Is(err, engine.ErrPermanent) || !errors.Is(err, sentinel) {
				t.Errorf("classifyPinError() = %v, want permanent wrapping %v", err, sentinel)
			}
		})
	}
	t.Run("tampered pull request", func(t *testing.T) {
		t.Parallel()
		err := classifyPinError(fmt.Errorf("pin: %w", gitops.ErrPullRequestTampered))
		if !errors.Is(err, engine.ErrNeedsAttention) || errors.Is(err, engine.ErrPermanent) || !errors.Is(err, gitops.ErrPullRequestTampered) {
			t.Errorf("classifyPinError() = %v, want ErrNeedsAttention wrapping ErrPullRequestTampered", err)
		}
	})
	transient := []error{gitops.ErrConflict, gitops.ErrMergeTimeout, github.ErrRefNotFound, &github.APIError{StatusCode: 502}, context.DeadlineExceeded}
	for _, cause := range transient {
		t.Run("transient "+cause.Error(), func(t *testing.T) {
			t.Parallel()
			err := classifyPinError(cause)
			if errors.Is(err, engine.ErrPermanent) || errors.Is(err, engine.ErrNeedsAttention) || !errors.Is(err, cause) {
				t.Errorf("classifyPinError(%v) = %v, want it unchanged", cause, err)
			}
		})
	}
	if classifyPinError(nil) != nil {
		t.Errorf("classifyPinError(nil) is not nil")
	}
}

type recordingAnnouncer struct {
	mu        sync.Mutex
	queued    []core.DeploymentID
	cancelled []core.DeploymentID
	expired   bool
}

func (a *recordingAnnouncer) Queued(ctx context.Context, id core.DeploymentID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queued = append(a.queued, id)
	a.expired = a.expired || ctx.Err() != nil
}

func (a *recordingAnnouncer) Cancelled(ctx context.Context, ids ...core.DeploymentID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancelled = append(a.cancelled, ids...)
	a.expired = a.expired || ctx.Err() != nil
}

type settleCommits struct {
	intake.Commits
	settled  core.TrackedCommit
	enqueued bool
	err      error
}

func (c settleCommits) Settle(context.Context, core.TrackedCommitID, core.ChecksEvaluation, intake.EnqueueFunc) (core.TrackedCommit, bool, error) {
	return c.settled, c.enqueued, c.err
}

func TestAnnouncingCommits(t *testing.T) {
	t.Parallel()

	id := core.DeploymentID(9)
	tests := []struct {
		name     string
		commits  settleCommits
		want     []core.DeploymentID
		wantErr  bool
		cancelFn bool
	}{
		{name: "enqueued", commits: settleCommits{settled: core.TrackedCommit{DeploymentID: &id}, enqueued: true}, want: []core.DeploymentID{9}},
		{name: "enqueued after the caller gave up", commits: settleCommits{settled: core.TrackedCommit{DeploymentID: &id}, enqueued: true}, want: []core.DeploymentID{9}, cancelFn: true},
		{name: "already enqueued", commits: settleCommits{settled: core.TrackedCommit{DeploymentID: &id}}},
		{name: "pending", commits: settleCommits{settled: core.TrackedCommit{}}},
		{name: "error", commits: settleCommits{settled: core.TrackedCommit{DeploymentID: &id}, enqueued: true, err: errors.New("boom")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := &recordingAnnouncer{}
			c := announcingCommits{Commits: tt.commits, announcer: a}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancelFn {
				cancel()
			}
			_, _, err := c.Settle(ctx, 1, core.ChecksEvaluation{}, nil)
			if (err != nil) != tt.wantErr {
				t.Errorf("Settle() error = %v", err)
			}
			if !reflect.DeepEqual(a.queued, tt.want) || a.cancelled != nil {
				t.Errorf("queued = %v, cancelled = %v, want queued %v", a.queued, a.cancelled, tt.want)
			}
			if a.expired {
				t.Errorf("announcement ran on a cancelled context")
			}
		})
	}
}

type fakeRollbacks struct {
	result rollback.Result
	err    error
}

func (f fakeRollbacks) Rollback(context.Context, rollback.Request) (rollback.Result, error) {
	return f.result, f.err
}

func TestAnnouncingRollbacks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		rollbacks     fakeRollbacks
		wantQueued    []core.DeploymentID
		wantCancelled []core.DeploymentID
	}{
		{
			name:          "queued and cancelled",
			rollbacks:     fakeRollbacks{result: rollback.Result{Rollback: core.Deployment{ID: 5}, Cancelled: []core.DeploymentID{3, 4}}},
			wantQueued:    []core.DeploymentID{5},
			wantCancelled: []core.DeploymentID{3, 4},
		},
		{
			name:       "nothing cancelled",
			rollbacks:  fakeRollbacks{result: rollback.Result{Rollback: core.Deployment{ID: 5}}},
			wantQueued: []core.DeploymentID{5},
		},
		{name: "refused", rollbacks: fakeRollbacks{err: core.ErrNotFound}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := &recordingAnnouncer{}
			r := announcingRollbacks{rollbacks: tt.rollbacks, announcer: a}
			got, err := r.Rollback(t.Context(), rollback.Request{})
			if !errors.Is(err, tt.rollbacks.err) || !reflect.DeepEqual(got, tt.rollbacks.result) {
				t.Errorf("Rollback() = %+v, %v", got, err)
			}
			if !reflect.DeepEqual(a.queued, tt.wantQueued) || !reflect.DeepEqual(a.cancelled, tt.wantCancelled) {
				t.Errorf("queued = %v, cancelled = %v, want %v and %v", a.queued, a.cancelled, tt.wantQueued, tt.wantCancelled)
			}
		})
	}
}

type fakeStatusReader struct {
	status core.SyncStatus
	err    error
	asked  []core.Target
}

func (f *fakeStatusReader) Status(_ context.Context, t core.Target) (core.SyncStatus, error) {
	f.asked = append(f.asked, t)
	return f.status, f.err
}

func TestArgoApplications(t *testing.T) {
	t.Parallel()

	catalog := loadCatalog(t, `
apps:
  - name: blog
    source:
      repo: example-user/blog
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: apps/blog
    argocd:
      application: blog-prod
      namespace: gitops
`)
	tests := []struct {
		name      string
		namespace string
		app       string
		status    core.SyncStatus
		err       error
		want      gates.Application
		wantErr   error
		wantAsked bool
	}{
		{
			name:      "synced and healthy",
			namespace: "gitops",
			app:       "blog-prod",
			status: core.SyncStatus{
				State:          core.SyncSynced,
				Revision:       adapterSHA,
				OperationPhase: "Succeeded",
				Health:         core.Health{Status: core.HealthHealthy, Message: "all good"},
			},
			want:      gates.Application{SyncStatus: gates.SyncSynced, Revision: adapterSHA, Health: gates.HealthHealthy, HealthMessage: "all good", OperationPhase: gates.OperationSucceeded},
			wantAsked: true,
		},
		{
			name:      "out of sync and degraded",
			namespace: "gitops",
			app:       "blog-prod",
			status:    core.SyncStatus{State: core.SyncOutOfSync, OperationPhase: "Running", Health: core.Health{Status: core.HealthDegraded}},
			want:      gates.Application{SyncStatus: gates.SyncOutOfSync, Health: gates.HealthDegraded, OperationPhase: gates.OperationRunning},
			wantAsked: true,
		},
		{
			name:      "unknown states",
			namespace: "gitops",
			app:       "blog-prod",
			status:    core.SyncStatus{State: "", Health: core.Health{Status: ""}},
			want:      gates.Application{SyncStatus: gates.SyncUnknown, Health: gates.HealthUnknown},
			wantAsked: true,
		},
		{
			name:      "not configured",
			namespace: "argocd",
			app:       "blog",
			wantErr:   gates.ErrApplicationNotFound,
		},
		{
			name:      "deleted in the cluster",
			namespace: "gitops",
			app:       "blog-prod",
			err:       fmt.Errorf("get: %w", core.ErrSyncTargetNotFound),
			wantErr:   gates.ErrApplicationNotFound,
			wantAsked: true,
		},
		{
			name:      "forbidden",
			namespace: "gitops",
			app:       "blog-prod",
			err:       core.ErrSyncForbidden,
			wantErr:   core.ErrSyncForbidden,
			wantAsked: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeStatusReader{status: tt.status, err: tt.err}
			got, err := newArgoApplications(reader, catalog).Application(t.Context(), tt.namespace, tt.app)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Errorf("Application() error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("Application() = %+v, want %+v", got, tt.want)
			}
			if asked := len(reader.asked) > 0; asked != tt.wantAsked {
				t.Errorf("asked Argo CD = %t, want %t", asked, tt.wantAsked)
			}
			if tt.wantAsked && reader.asked[0] != core.AppTarget("blog") {
				t.Errorf("asked for %v, want blog", reader.asked[0])
			}
		})
	}
}

func TestGateStatusMapping(t *testing.T) {
	t.Parallel()

	syncs := map[core.SyncState]string{
		core.SyncSynced:    gates.SyncSynced,
		core.SyncOutOfSync: gates.SyncOutOfSync,
		core.SyncUnknown:   gates.SyncUnknown,
		"Strange":          gates.SyncUnknown,
	}
	for in, want := range syncs {
		if got := gateSyncStatus(in); got != want {
			t.Errorf("gateSyncStatus(%q) = %q, want %q", in, got, want)
		}
	}
	healths := map[core.HealthStatus]string{
		core.HealthHealthy:     gates.HealthHealthy,
		core.HealthProgressing: gates.HealthProgressing,
		core.HealthDegraded:    gates.HealthDegraded,
		core.HealthSuspended:   gates.HealthSuspended,
		core.HealthMissing:     gates.HealthMissing,
		core.HealthUnknown:     gates.HealthUnknown,
		"Strange":              gates.HealthUnknown,
	}
	for in, want := range healths {
		if got := gateHealth(in); got != want {
			t.Errorf("gateHealth(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewGateEvaluatorBuildsPlans(t *testing.T) {
	t.Parallel()

	catalog := loadCatalog(t, `
apps:
  - name: blog
    source:
      repo: example-user/blog
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: apps/blog
    gates:
      before:
        - type: lock
      after:
        - type: soak
          duration: 1m
`)
	evaluator, err := newGateEvaluator(gateDeps{catalog: catalog, logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("newGateEvaluator: %v", err)
	}
	if evaluator == nil {
		t.Fatal("newGateEvaluator returned nil")
	}
}

func TestEngineOutcomesAreMetricsOutcomes(t *testing.T) {
	t.Parallel()

	pairs := map[engine.Outcome]metrics.Outcome{
		engine.OutcomeSucceeded:      metrics.OutcomeSucceeded,
		engine.OutcomeFailed:         metrics.OutcomeFailed,
		engine.OutcomeRolledBack:     metrics.OutcomeRolledBack,
		engine.OutcomeRollbackFailed: metrics.OutcomeRollbackFailed,
		engine.OutcomeCancelled:      metrics.OutcomeCancelled,
		engine.OutcomeSuperseded:     metrics.OutcomeSuperseded,
	}
	for in, want := range pairs {
		if metrics.Outcome(in) != want {
			t.Errorf("engine outcome %q does not match metrics outcome %q", in, want)
		}
	}
}

func TestRunServeEngineRequirements(t *testing.T) {
	t.Parallel()

	gh := githubtest.NewServer(t)
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "no GitHub App",
			wantErr: "engine: 1 apps are configured but the GitHub App is not",
		},
		{
			name:    "no Kubernetes connection",
			env:     githubEnv(gh, nil),
			wantErr: "engine: 1 apps are configured, so doorstep must read their Argo CD Applications",
		},
		{
			name:    "unreadable kubeconfig",
			env:     githubEnv(gh, map[string]string{config.EnvKubeconfig: filepath.Join(t.TempDir(), "missing")}),
			wantErr: "must read their Argo CD Applications",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := maps.Clone(tt.env)
			if env == nil {
				env = map[string]string{}
			}
			env[config.EnvDatabaseURL] = "postgres://unused"
			code, _, stderr := cliRun{t: t, env: env}.run("serve", "--listen-addr", "127.0.0.1:0", "--config", writeConfig(t, authApp))
			if code != 1 || !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("serve = %d %q, want an error containing %q", code, stderr, tt.wantErr)
			}
		})
	}
}
