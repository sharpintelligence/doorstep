package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/argocd/argocdtest"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
	"github.com/sharpintelligence/doorstep/internal/gitops"
	"github.com/sharpintelligence/doorstep/internal/registry/registrytest"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const (
	engineHomelabURL = "https://github.com/example-user/homelab.git"
	enginePinFile    = "apps/blog/kustomization.yaml"
	engineWaitLimit  = 20 * time.Second
)

const engineConfig = `
notifications:
  - name: hooks
    type: webhook
    url_env: TEST_HOOK_URL
    events: [deploy.queued, deploy.started, deploy.succeeded, deploy.failed, deploy.rolled_back, deploy.cancelled, deploy.superseded]
apps:
  - name: blog
    source:
      repo: example-user/blog
      required_checks: [test]
      deployments: false
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: apps/blog
    notify: [hooks]
`

type engineFixture struct {
	github     *githubtest.Server
	homelab    *githubtest.Repo
	argo       *argocdtest.Server
	registry   *registrytest.Server
	kubeconfig string
}

func newEngineFixture(t *testing.T) *engineFixture {
	t.Helper()
	gh := githubtest.NewServer(t)
	gh.Install("example-user", "blog", 1)
	homelab := gh.AddRepo("example-user", "homelab", 1)
	reg := registrytest.NewServer(t)
	seed := homelab.Seed("main", enginePinFile, "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - deployment.yaml\nimages:\n  - name: "+reg.Repo("example-user/blog")+"\n    newTag: initial\n")
	argo := argocdtest.NewServer(t)
	argo.Set(argocdtest.Synced("argocd", "blog", engineHomelabURL, seed))
	return &engineFixture{github: gh, homelab: homelab, argo: argo, registry: reg, kubeconfig: argo.Kubeconfig(t)}
}

func (f *engineFixture) env(extra map[string]string) map[string]string {
	env := githubEnv(f.github, extra)
	env[config.EnvKubeconfig] = f.kubeconfig
	return env
}

func (f *engineFixture) config(content string) string {
	return strings.ReplaceAll(content, "ghcr.io/", f.registry.Host()+"/")
}

func (f *engineFixture) followGitOps(ctx context.Context, apply func(sha string) argocdtest.Application) {
	last, _ := f.homelab.BranchSHA("main")
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			sha, ok := f.homelab.BranchSHA("main")
			if ok && sha != last {
				last = sha
				f.argo.Set(apply(sha))
			}
		}
	}()
}

func syncedTo(sha string) argocdtest.Application {
	return argocdtest.Synced("argocd", "blog", engineHomelabURL, sha)
}

func syncFailedAt(sha string) argocdtest.Application {
	app := argocdtest.Synced("argocd", "blog", engineHomelabURL, sha)
	app.Sync = "OutOfSync"
	app.Health = "Degraded"
	app.Operation = &argocdtest.Operation{
		Phase: "Failed", Message: "one or more synchronization tasks completed unsuccessfully", Revision: sha,
		Resources: []argocdtest.SyncResource{{Group: "batch", Kind: "Job", Namespace: "blog", Name: "migrate", Status: "Synced", HookPhase: "Failed", Message: "job failed"}},
	}
	return app
}

type recordingSink struct {
	mu     sync.Mutex
	server *httptest.Server
	events []sinkEvent
}

type sinkEvent struct {
	Type       string `json:"type"`
	Deployment struct {
		ID    core.DeploymentID `json:"id"`
		Kind  core.Kind         `json:"kind"`
		State core.State        `json:"state"`
	} `json:"deployment"`
}

func newRecordingSink(t *testing.T) *recordingSink {
	t.Helper()
	s := &recordingSink{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev sinkEvent
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.events = append(s.events, ev)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *recordingSink) typesFor(id core.DeploymentID) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, ev := range s.events {
		if ev.Deployment.ID == id {
			out = append(out, ev.Type)
		}
	}
	return out
}

func (s *recordingSink) await(t *testing.T, id core.DeploymentID, want []string) {
	t.Helper()
	deadline := time.Now().Add(engineWaitLimit)
	for {
		got := s.typesFor(id)
		if slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("notifications for deployment %d = %v, want %v", id, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type engineServe struct {
	t       *testing.T
	fixture *engineFixture
	sink    *recordingSink
	env     map[string]string
	store   *store.Store
	addr    string
	stderr  *lockedBuffer
	cancel  context.CancelFunc
	done    chan int
}

func startEngineServe(t *testing.T, f *engineFixture) *engineServe {
	t.Helper()
	return startEngineServeWith(t, f, engineConfig)
}

func startEngineServeWith(t *testing.T, f *engineFixture, cfg string) *engineServe {
	t.Helper()
	sink := newRecordingSink(t)
	args, env := withDatabase(t, databaseURLEnv, []string{
		"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "", "--shutdown-delay", "0s",
		"--config", writeConfig(t, f.config(cfg)),
	}, f.env(map[string]string{
		config.EnvGitHubWebhookSecret: intakeTestSecret,
		config.EnvPollInterval:        "0",
		"TEST_HOOK_URL":               sink.server.URL,
	}))
	ctx, cancel := context.WithCancel(t.Context())
	s := &engineServe{t: t, fixture: f, sink: sink, env: env, stderr: &lockedBuffer{}, cancel: cancel, done: make(chan int, 1)}
	go func() { s.done <- Run(ctx, args, io.Discard, s.stderr, envFrom(env)) }()
	s.addr = waitForListening(t, s.stderr, s.done)
	waitForLog(t, s.stderr, s.done, "engine started")
	st, err := store.Open(t.Context(), env[config.EnvDatabaseURL])
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	s.store = st
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
		}
	})
	return s
}

func (s *engineServe) stop() {
	s.t.Helper()
	s.cancel()
	select {
	case code := <-s.done:
		s.done <- code
		if code != 0 {
			s.t.Errorf("exit code = %d, stderr %q", code, s.stderr.String())
		}
	case <-time.After(10 * time.Second):
		s.t.Fatal("serve did not exit after cancellation")
	}
}

func (s *engineServe) push(sha string) (core.DeploymentID, core.Digest) {
	s.t.Helper()
	digest := core.Digest(s.fixture.registry.Push("example-user/blog", sha))
	s.fixture.github.SetBranch("example-user", "blog", "main", sha)
	s.fixture.github.SetCheckRuns("example-user", "blog", sha, githubtest.CheckRun{ID: 1, Name: "test", Status: "completed", Conclusion: "success"})
	body := `{"ref":"refs/heads/main","after":"` + sha + `","repository":{"full_name":"example-user/blog"},"sender":{"login":"octocat"}}`
	if code, resp := postPush(s.t, "http://"+s.addr+"/webhooks/github", "delivery-"+sha, intakeTestSecret, body); code != http.StatusOK {
		s.t.Fatalf("push %s = %d %s", sha, code, resp)
	}
	deadline := time.Now().Add(engineWaitLimit)
	for {
		commits, err := s.store.TrackedCommits(s.t.Context(), "example-user/blog", sha)
		if err != nil {
			s.t.Fatalf("TrackedCommits: %v", err)
		}
		if len(commits) == 1 && commits[0].DeploymentID != nil {
			return *commits[0].DeploymentID, digest
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("commit %s was not enqueued: %+v\n%s", sha, commits, s.stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *engineServe) awaitState(id core.DeploymentID, want ...core.State) core.Deployment {
	s.t.Helper()
	deadline := time.Now().Add(engineWaitLimit)
	for {
		d, err := s.store.GetDeployment(s.t.Context(), id)
		if err != nil {
			s.t.Fatalf("GetDeployment: %v", err)
		}
		if slices.Contains(want, d.State) {
			return d
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("deployment %d is %s, want %v\n%s", id, d.State, want, s.stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *engineServe) transitions(id core.DeploymentID) []core.State {
	s.t.Helper()
	h, err := s.store.GetDeploymentHistory(s.t.Context(), id)
	if err != nil {
		s.t.Fatalf("GetDeploymentHistory: %v", err)
	}
	events := slices.Clone(h.Events)
	slices.SortFunc(events, func(a, b core.Event) int { return int(a.ID - b.ID) })
	var states []core.State
	for _, e := range events {
		if e.Type == core.EventTransition {
			states = append(states, e.To)
		}
	}
	return states
}

func (s *engineServe) token(role core.Role) string {
	s.t.Helper()
	var out, errOut bytes.Buffer
	args := []string{"token", "create", "--name", "engine-" + string(role), "--role", string(role)}
	if code := Run(s.t.Context(), args, &out, &errOut, envFrom(s.env)); code != 0 {
		s.t.Fatalf("token create = %d: %s", code, errOut.String())
	}
	return strings.TrimSpace(out.String())
}

func (s *engineServe) metrics() string {
	s.t.Helper()
	_, body := get(s.t, "http://"+s.addr+"/metrics")
	return body
}

var deploySucceeded = []core.State{core.StateQueued, core.StateResolving, core.StatePinning, core.StateSyncing, core.StateVerifying, core.StateSucceeded}

func TestEngineDeploysAndRollsBack(t *testing.T) {
	t.Parallel()

	f := newEngineFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.followGitOps(ctx, syncedTo)
	s := startEngineServe(t, f)

	firstSHA, secondSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	first, firstDigest := s.push(firstSHA)
	d := s.awaitState(first, core.StateSucceeded, core.StateFailed)
	if d.State != core.StateSucceeded || d.Digest != firstDigest || d.PreviousDigest != "" || d.PinCommitSHA == "" || d.ArgoCDRevision != d.PinCommitSHA {
		t.Fatalf("first deployment = %+v, want succeeded with %s pinned and synced", d, firstDigest)
	}
	if got := s.transitions(first); !slices.Equal(got, deploySucceeded) {
		t.Errorf("transitions = %v, want %v", got, deploySucceeded)
	}
	content, _ := f.homelab.File("main", enginePinFile)
	if !strings.Contains(content, "digest: "+string(firstDigest)) {
		t.Errorf("pin file does not hold %s:\n%s", firstDigest, content)
	}
	head := f.homelab.Commits("main")[0]
	if head.SHA != d.PinCommitSHA || !strings.Contains(head.Message, gitops.TrailerDeployment+": ") || !strings.Contains(head.Message, gitops.TrailerDigest+": "+string(firstDigest)) {
		t.Errorf("pin commit %s = %q, want the trailers of deployment %d", head.SHA, head.Message, first)
	}
	s.sink.await(t, first, []string{"deploy.queued", "deploy.started", "deploy.succeeded"})

	second, secondDigest := s.push(secondSHA)
	d = s.awaitState(second, core.StateSucceeded, core.StateFailed)
	if d.State != core.StateSucceeded || d.Digest != secondDigest || d.PreviousDigest != firstDigest {
		t.Fatalf("second deployment = %+v, want succeeded over %s", d, firstDigest)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+s.addr+"/api/v1/apps/blog/rollback", strings.NewReader(`{"reason":"checkout errors"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token(core.RoleDeployer))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST rollback: %v", err)
	}
	var rolled struct {
		Deployment struct {
			ID core.DeploymentID `json:"id"`
		} `json:"deployment"`
	}
	err = json.NewDecoder(resp.Body).Decode(&rolled)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK || err != nil || rolled.Deployment.ID == 0 {
		t.Fatalf("POST rollback = %d, %v, %+v", resp.StatusCode, err, rolled)
	}
	rb := s.awaitState(rolled.Deployment.ID, core.StateSucceeded, core.StateFailed)
	if rb.State != core.StateSucceeded || rb.Kind != core.KindRollback || rb.Digest != firstDigest || rb.PreviousDigest != secondDigest {
		t.Fatalf("rollback = %+v, want succeeded back to %s", rb, firstDigest)
	}
	if got := s.transitions(rb.ID); !slices.Equal(got, deploySucceeded) {
		t.Errorf("rollback transitions = %v, want %v", got, deploySucceeded)
	}
	content, _ = f.homelab.File("main", enginePinFile)
	if !strings.Contains(content, "digest: "+string(firstDigest)) {
		t.Errorf("pin file does not hold %s after the rollback:\n%s", firstDigest, content)
	}
	if msg := f.homelab.Commits("main")[0].Message; !strings.Contains(msg, gitops.TrailerBlock+": "+string(secondDigest)) {
		t.Errorf("rollback pin commit does not mirror the block of %s: %q", secondDigest, msg)
	}
	s.sink.await(t, rb.ID, []string{"deploy.queued", "deploy.started", "deploy.rolled_back"})

	body := s.metrics()
	for _, want := range []string{
		`doorstep_deployments_started_total{app="blog"} 3`,
		`doorstep_deployment_duration_seconds_count{app="blog",outcome="succeeded"} 2`,
		`doorstep_deployment_duration_seconds_count{app="blog",outcome="rolled_back"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	s.stop()
}

func TestEngineFailsOnSyncFailure(t *testing.T) {
	t.Parallel()

	f := newEngineFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.followGitOps(ctx, syncFailedAt)
	s := startEngineServe(t, f)

	id, _ := s.push(strings.Repeat("c", 40))
	d := s.awaitState(id, core.StateSucceeded, core.StateFailed)
	if d.State != core.StateFailed || !strings.Contains(d.FailureReason, "Argo CD sync failed") {
		t.Fatalf("deployment = %+v, want failed by the sync", d)
	}
	want := []core.State{core.StateQueued, core.StateResolving, core.StatePinning, core.StateSyncing, core.StateFailed}
	if got := s.transitions(id); !slices.Equal(got, want) {
		t.Errorf("transitions = %v, want %v", got, want)
	}
	s.sink.await(t, id, []string{"deploy.queued", "deploy.started", "deploy.failed"})
	if body := s.metrics(); !strings.Contains(body, `doorstep_deployment_duration_seconds_count{app="blog",outcome="failed"} 1`) {
		t.Errorf("metrics do not count the failure")
	}
	s.stop()
}

func TestEngineShutdownMidSyncKeepsDeployment(t *testing.T) {
	t.Parallel()

	f := newEngineFixture(t)
	s := startEngineServe(t, f)

	id, _ := s.push(strings.Repeat("d", 40))
	s.awaitState(id, core.StateSyncing)
	f.argo.AwaitWatch(t, 1)
	s.stop()

	d, err := s.store.GetDeployment(t.Context(), id)
	if err != nil || d.State != core.StateSyncing {
		t.Fatalf("deployment after shutdown = %+v, %v, want still syncing", d, err)
	}
	entries, err := s.store.QueueEntries(t.Context(), "blog")
	if err != nil || len(entries) != 1 || entries[0].Deployment.ID != id || entries[0].LeaseExpiresAt != nil {
		t.Fatalf("queue after shutdown = %+v, %v, want the deployment released", entries, err)
	}
	if got := s.sink.typesFor(id); slices.Contains(got, "deploy.failed") {
		t.Errorf("notifications = %v, shutdown must not fail the deployment", got)
	}
}

const retryEngineConfig = `
notifications:
  - name: hooks
    type: webhook
    url_env: TEST_HOOK_URL
    events: [deploy.queued, deploy.started, deploy.retrying, deploy.succeeded, deploy.failed]
apps:
  - name: blog
    source:
      repo: example-user/blog
      required_checks: [test]
      deployments: false
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: apps/blog
    argocd:
      sync_timeout: 2s
    retry:
      max_attempts: 2
      backoff: 1s
      max_backoff: 1s
    notify: [hooks]
`

func degradedAt(sha string) argocdtest.Application {
	app := argocdtest.Synced("argocd", "blog", engineHomelabURL, sha)
	app.Health = "Degraded"
	app.HealthMessage = "Deployment blog: Readiness probe failed: connection refused"
	return app
}

func (s *engineServe) events(id core.DeploymentID) []core.Event {
	s.t.Helper()
	h, err := s.store.GetDeploymentHistory(s.t.Context(), id)
	if err != nil {
		s.t.Fatalf("GetDeploymentHistory: %v", err)
	}
	var out []core.Event
	for _, e := range h.Events {
		if e.Type == core.EventTransition {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b core.Event) int { return int(a.ID - b.ID) })
	return out
}

func (s *engineServe) awaitRetry(id core.DeploymentID) core.Event {
	s.t.Helper()
	deadline := time.Now().Add(engineWaitLimit)
	for {
		for _, e := range s.events(id) {
			if e.From == core.StateSyncing && e.To == core.StateSyncing {
				return e
			}
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("deployment %d was not retried: %v\n%s", id, s.transitions(id), s.stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEngineRetriesTheSameVersion(t *testing.T) {
	t.Parallel()

	f := newEngineFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.followGitOps(ctx, degradedAt)
	s := startEngineServeWith(t, f, retryEngineConfig)

	id, _ := s.push(strings.Repeat("e", 40))
	retried := s.awaitRetry(id)
	var data struct {
		Retry struct {
			Attempt     int    `json:"attempt"`
			NextAttempt int    `json:"next_attempt"`
			MaxAttempts int    `json:"max_attempts"`
			DelayMS     int64  `json:"delay_ms"`
			Reason      string `json:"reason"`
		} `json:"retry"`
		Diagnosis struct {
			Verdict string `json:"verdict"`
		} `json:"diagnosis"`
	}
	if err := json.Unmarshal(retried.Data, &data); err != nil || data.Retry.Attempt != 1 || data.Retry.NextAttempt != 2 || data.Retry.MaxAttempts != 2 || data.Retry.DelayMS != 1000 || data.Retry.Reason != "transient" || data.Diagnosis.Verdict != "transient" {
		t.Fatalf("retry event data = %s (%v)", retried.Data, err)
	}
	pinned, err := s.store.GetDeployment(t.Context(), id)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	pinCommits := len(f.homelab.Commits("main"))
	f.argo.Set(syncedTo(pinned.PinCommitSHA))

	d := s.awaitState(id, core.StateSucceeded, core.StateFailed)
	if d.State != core.StateSucceeded || d.PinCommitSHA != pinned.PinCommitSHA || d.Digest != pinned.Digest {
		t.Fatalf("deployment = %+v, want succeeded on the same pin", d)
	}
	if got := len(f.homelab.Commits("main")); got != pinCommits {
		t.Errorf("GitOps commits = %d after the retry, want %d: a retry must not write to git", got, pinCommits)
	}
	want := []core.State{core.StateQueued, core.StateResolving, core.StatePinning, core.StateSyncing, core.StateSyncing, core.StateVerifying, core.StateSucceeded}
	if got := s.transitions(id); !slices.Equal(got, want) {
		t.Errorf("transitions = %v, want %v", got, want)
	}
	s.sink.await(t, id, []string{"deploy.queued", "deploy.started", "deploy.retrying", "deploy.succeeded"})
	if body := s.metrics(); !strings.Contains(body, `doorstep_deployment_retries_total{app="blog",class="transient"} 1`) {
		t.Errorf("metrics do not count the retry")
	}
	s.stop()
}

func TestEngineFailsWhenTheRetryBudgetIsSpent(t *testing.T) {
	t.Parallel()

	f := newEngineFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.followGitOps(ctx, degradedAt)
	s := startEngineServeWith(t, f, retryEngineConfig)

	id, _ := s.push(strings.Repeat("f", 40))
	d := s.awaitState(id, core.StateSucceeded, core.StateFailed)
	if d.State != core.StateFailed {
		t.Fatalf("deployment = %+v, want failed", d)
	}
	want := []core.State{core.StateQueued, core.StateResolving, core.StatePinning, core.StateSyncing, core.StateSyncing, core.StateFailed}
	if got := s.transitions(id); !slices.Equal(got, want) {
		t.Errorf("transitions = %v, want %v", got, want)
	}
	events := s.events(id)
	last := events[len(events)-1]
	var data struct {
		Attempts         int    `json:"attempts"`
		Decision         string `json:"decision"`
		RollbackEligible bool   `json:"rollback_eligible"`
		Stage            string `json:"stage"`
	}
	if err := json.Unmarshal(last.Data, &data); err != nil || data.Attempts != 2 || data.Decision != "retries_exhausted" || !data.RollbackEligible || data.Stage != "syncing" {
		t.Errorf("failed event data = %s (%v)", last.Data, err)
	}
	if last.Message != "failed while syncing after 2 attempts" {
		t.Errorf("failed event message = %q", last.Message)
	}
	s.sink.await(t, id, []string{"deploy.queued", "deploy.started", "deploy.retrying", "deploy.failed"})
	s.stop()
}
