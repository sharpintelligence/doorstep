package notify

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
)

type fakeGitHubDeployment struct {
	ID          int64           `json:"id"`
	SHA         string          `json:"sha"`
	Ref         string          `json:"ref"`
	Task        string          `json:"task"`
	Environment string          `json:"environment"`
	Description string          `json:"description"`
	Payload     json.RawMessage `json:"payload"`
	repo        string
}

type fakeGitHubStatus struct {
	deploymentID int64
	State        string `json:"state"`
	Description  string `json:"description"`
	LogURL       string `json:"log_url"`
	AutoInactive bool   `json:"auto_inactive"`
}

type fakeDeploymentsAPI struct {
	mu          sync.Mutex
	nextID      int64
	deployments []fakeGitHubDeployment
	statuses    []fakeGitHubStatus
	lists       int
	creates     int
	failStatus  int
	failMessage string
	failHeader  http.Header
}

func (f *fakeDeploymentsAPI) register(srv *githubtest.Server) {
	srv.Handle("GET /repos/{owner}/{repo}/deployments", http.HandlerFunc(f.list))
	srv.Handle("POST /repos/{owner}/{repo}/deployments", http.HandlerFunc(f.create))
	srv.Handle("POST /repos/{owner}/{repo}/deployments/{id}/statuses", http.HandlerFunc(f.status))
}

func (f *fakeDeploymentsAPI) setFailure(status int) {
	f.setFailureResponse(status, "Resource not accessible by integration", nil)
}

func (f *fakeDeploymentsAPI) setFailureResponse(status int, message string, header http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStatus, f.failMessage, f.failHeader = status, message, header
}

func (f *fakeDeploymentsAPI) fail(w http.ResponseWriter) bool {
	if f.failStatus == 0 {
		return false
	}
	for key, values := range f.failHeader {
		w.Header()[key] = values
	}
	githubtest.WriteError(w, f.failStatus, f.failMessage)
	return true
}

func (f *fakeDeploymentsAPI) list(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.fail(w) {
		return
	}
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	q := r.URL.Query()
	out := []fakeGitHubDeployment{}
	for i := len(f.deployments) - 1; i >= 0; i-- {
		d := f.deployments[i]
		if d.repo == repo && d.SHA == q.Get("sha") && d.Environment == q.Get("environment") && d.Task == q.Get("task") {
			out = append(out, d)
		}
	}
	githubtest.WriteJSON(w, http.StatusOK, out)
}

func (f *fakeDeploymentsAPI) create(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if f.fail(w) {
		return
	}
	var d fakeGitHubDeployment
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		githubtest.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	f.nextID++
	d.ID, d.SHA, d.repo = f.nextID, d.Ref, r.PathValue("owner")+"/"+r.PathValue("repo")
	f.deployments = append(f.deployments, d)
	githubtest.WriteJSON(w, http.StatusCreated, d)
}

func (f *fakeDeploymentsAPI) status(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail(w) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var s fakeGitHubStatus
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		githubtest.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.deploymentID = id
	f.statuses = append(f.statuses, s)
	githubtest.WriteJSON(w, http.StatusCreated, map[string]any{"id": len(f.statuses)})
}

func (f *fakeDeploymentsAPI) snapshot() ([]fakeGitHubDeployment, []fakeGitHubStatus, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeGitHubDeployment(nil), f.deployments...), append([]fakeGitHubStatus(nil), f.statuses...), f.lists, f.creates
}

type githubFixture struct {
	srv   *githubtest.Server
	api   *fakeDeploymentsAPI
	app   *github.App
	clock *testClock
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newGitHubFixture(t *testing.T) *githubFixture {
	t.Helper()
	srv := githubtest.NewServer(t)
	srv.Install("example-user", "blog", 1)
	api := &fakeDeploymentsAPI{}
	api.register(srv)
	app, err := github.New(github.Options{AppID: srv.AppID, PrivateKey: srv.PrivateKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	return &githubFixture{srv: srv, api: api, app: app, clock: &testClock{now: testTime}}
}

func (f *githubFixture) sink(t *testing.T) *GitHub {
	t.Helper()
	sink, err := NewGitHub(GitHubOptions{Repos: f.app, Apps: map[string]bool{"blog": true, "off": false}, Links: testLinks(), Now: f.clock.Now})
	if err != nil {
		t.Fatalf("NewGitHub: %v", err)
	}
	return sink
}

func deliver(t *testing.T, sink *GitHub, n core.Notification) error {
	t.Helper()
	if !sink.Accepts(n) {
		t.Fatalf("sink does not accept %s", n.Kind)
	}
	return sink.Prepare(n)(t.Context())
}

func TestGitHubSinkLifecycle(t *testing.T) {
	t.Parallel()

	f := newGitHubFixture(t)
	sink := f.sink(t)
	for _, name := range []string{"deploy-queued", "deploy-started", "deploy-succeeded"} {
		if err := deliver(t, sink, sampleNamed(t, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	deployments, statuses, lists, creates := f.api.snapshot()
	if creates != 1 || lists != 1 || len(deployments) != 1 {
		t.Fatalf("creates = %d, lists = %d, deployments = %d; want one lookup and one deployment", creates, lists, len(deployments))
	}
	d := deployments[0]
	if d.Ref != testSHA || d.Environment != "blog" || d.Task != "deploy" || d.Description != "doorstep deploy #42" {
		t.Errorf("deployment = %+v", d)
	}
	if string(d.Payload) != `{"doorstep_deployment_id":42,"kind":"deploy"}` {
		t.Errorf("payload = %s", d.Payload)
	}
	wantStates := []string{"queued", "in_progress", "success"}
	wantDescriptions := []string{"Queued", "Deploying", "Deployed"}
	if len(statuses) != len(wantStates) {
		t.Fatalf("statuses = %+v", statuses)
	}
	for i, s := range statuses {
		if s.deploymentID != d.ID || s.State != wantStates[i] || s.Description != wantDescriptions[i] ||
			s.LogURL != "https://doorstep.example.com/deployments/42" || !s.AutoInactive {
			t.Errorf("status %d = %+v, want %s %q", i, s, wantStates[i], wantDescriptions[i])
		}
	}
	if sink.cacheLen() != 0 {
		t.Errorf("cache holds %d entries after a terminal status", sink.cacheLen())
	}
}

func TestGitHubSinkFindsExistingDeploymentAfterRestart(t *testing.T) {
	t.Parallel()

	f := newGitHubFixture(t)
	if err := deliver(t, f.sink(t), sampleNamed(t, "deploy-started")); err != nil {
		t.Fatalf("started: %v", err)
	}
	restarted := f.sink(t)
	if err := deliver(t, restarted, sampleNamed(t, "deploy-failed")); err != nil {
		t.Fatalf("failed: %v", err)
	}
	deployments, statuses, lists, creates := f.api.snapshot()
	if creates != 1 || lists != 2 || len(deployments) != 1 {
		t.Fatalf("creates = %d, lists = %d; want the restarted sink to find the existing deployment", creates, lists)
	}
	last := statuses[len(statuses)-1]
	if last.State != "failure" || last.Description != "Failed" || last.deploymentID != deployments[0].ID {
		t.Errorf("failure status = %+v", last)
	}
}

func TestGitHubSinkRollback(t *testing.T) {
	t.Parallel()

	f := newGitHubFixture(t)
	sink := f.sink(t)
	for _, name := range []string{"deploy-started", "deploy-failed", "rollback-started", "deploy-rolled-back"} {
		if err := deliver(t, sink, sampleNamed(t, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	deployments, statuses, _, _ := f.api.snapshot()
	if len(deployments) != 2 {
		t.Fatalf("deployments = %+v, want the deploy and its rollback", deployments)
	}
	rollback := deployments[1]
	if rollback.Description != "Rollback of #42" || rollback.Ref != "fedcba9876543210fedcba9876543210fedcba98" || rollback.Environment != "blog" {
		t.Errorf("rollback deployment = %+v", rollback)
	}
	if string(rollback.Payload) != `{"doorstep_deployment_id":43,"kind":"rollback","rollback_of_id":42}` {
		t.Errorf("rollback payload = %s", rollback.Payload)
	}
	var got []string
	for _, s := range statuses {
		got = append(got, strconv.FormatInt(s.deploymentID, 10)+":"+s.State+":"+s.Description)
	}
	want := []string{
		"1:in_progress:Deploying",
		"1:failure:Failed",
		"2:in_progress:Rolling back",
		"2:success:Rolled back #42",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("statuses =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestGitHubSinkInactiveStates(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ sample, description string }{
		{"deploy-cancelled", "Cancelled"},
		{"deploy-superseded", "Superseded by #44"},
	} {
		t.Run(tt.sample, func(t *testing.T) {
			t.Parallel()
			f := newGitHubFixture(t)
			if err := deliver(t, f.sink(t), sampleNamed(t, tt.sample)); err != nil {
				t.Fatalf("deliver: %v", err)
			}
			_, statuses, _, _ := f.api.snapshot()
			if len(statuses) != 1 || statuses[0].State != "inactive" || statuses[0].Description != tt.description {
				t.Errorf("statuses = %+v", statuses)
			}
		})
	}
}

func TestGitHubSinkAccepts(t *testing.T) {
	t.Parallel()

	f := newGitHubFixture(t)
	sink := f.sink(t)
	mutate := func(name string, change func(*core.Notification)) core.Notification {
		n := sampleNamed(t, name)
		change(&n)
		return n
	}
	tests := []struct {
		name string
		n    core.Notification
		want bool
	}{
		{name: "deploy started", n: sampleNamed(t, "deploy-started"), want: true},
		{name: "queued", n: sampleNamed(t, "deploy-queued"), want: true},
		{name: "superseded", n: sampleNamed(t, "deploy-superseded"), want: true},
		{name: "lock changed", n: sampleNamed(t, "lock-acquired")},
		{name: "lock reminder", n: sampleNamed(t, "lock-reminder")},
		{name: "app disabled", n: forApp(sampleNamed(t, "deploy-started"), "off")},
		{name: "app unknown", n: forApp(sampleNamed(t, "deploy-started"), "other")},
		{name: "missing sha", n: mutate("deploy-started", func(n *core.Notification) { n.Deployment.Commit.SHA = "" })},
		{name: "short sha", n: mutate("deploy-started", func(n *core.Notification) { n.Deployment.Commit.SHA = "abc1234" })},
		{name: "missing repository", n: mutate("deploy-started", func(n *core.Notification) { n.Deployment.Commit.Repository = "" })},
		{name: "repository without owner", n: mutate("deploy-started", func(n *core.Notification) { n.Deployment.Commit.Repository = "blog" })},
		{name: "repository with host", n: mutate("deploy-started", func(n *core.Notification) { n.Deployment.Commit.Repository = "github.com/example-user/blog" })},
		{name: "no deployment id", n: mutate("deploy-started", func(n *core.Notification) { n.Deployment.ID = 0 })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sink.Accepts(tt.n); got != tt.want {
				t.Errorf("Accepts() = %v, want %v", got, tt.want)
			}
		})
	}
	if sink.Name() != "github:deployments" || sink.Type() != TypeGitHub {
		t.Errorf("Name, Type = %q, %q", sink.Name(), sink.Type())
	}
}

func TestGitHubSinkPermissionProblemsWarnOncePerHour(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(*githubFixture)
		want  error
	}{
		{name: "forbidden", setup: func(f *githubFixture) { f.api.setFailure(http.StatusForbidden) }},
		{name: "not found", setup: func(f *githubFixture) { f.api.setFailure(http.StatusNotFound) }},
		{name: "not installed", setup: func(f *githubFixture) { f.srv.Uninstall(1) }, want: github.ErrNotInstalled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newGitHubFixture(t)
			tt.setup(f)
			logger, logs := newTestLogger()
			sink, err := NewGitHub(GitHubOptions{Repos: f.app, Apps: map[string]bool{"blog": true}, Logger: logger, Now: f.clock.Now})
			if err != nil {
				t.Fatalf("NewGitHub: %v", err)
			}
			n := sampleNamed(t, "deploy-started")
			for range 3 {
				err := deliver(t, sink, n)
				if err == nil || retryable(err) || !isReported(err) {
					t.Fatalf("deliver() = %v, want a reported permanent error", err)
				}
				if tt.want != nil && !errors.Is(err, tt.want) {
					t.Errorf("deliver() = %v, want %v", err, tt.want)
				}
			}
			if got := logs.count(t, "cannot report deployments to github", "WARN"); got != 1 {
				t.Errorf("warnings = %d, want 1: %s", got, logs)
			}
			if !strings.Contains(logs.String(), "Deployments: Read and write") || !strings.Contains(logs.String(), "example-user/blog") {
				t.Errorf("warning has no hint or repository: %s", logs)
			}
			f.clock.Advance(time.Hour)
			_ = deliver(t, sink, n)
			if got := logs.count(t, "cannot report deployments to github", "WARN"); got != 2 {
				t.Errorf("warnings after an hour = %d, want 2", got)
			}
		})
	}
}

func TestGitHubSinkTransientErrorsAreRetryable(t *testing.T) {
	t.Parallel()

	f := newGitHubFixture(t)
	f.api.setFailure(http.StatusBadGateway)
	err := deliver(t, f.sink(t), sampleNamed(t, "deploy-started"))
	if err == nil || !retryable(err) || isReported(err) {
		t.Errorf("deliver() = %v, want a retryable error", err)
	}
	f.api.setFailure(http.StatusUnprocessableEntity)
	err = deliver(t, f.sink(t), sampleNamed(t, "deploy-started"))
	if err == nil || retryable(err) {
		t.Errorf("deliver() = %v, want a permanent error", err)
	}
}

func TestGitHubSinkRateLimitsAreRetried(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		status         int
		message        string
		header         http.Header
		wantRetryAfter time.Duration
		wantDelay      bool
	}{
		{
			name:      "primary rate limit",
			status:    http.StatusForbidden,
			message:   "API rate limit exceeded for installation ID 1.",
			header:    http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)}},
			wantDelay: true,
		},
		{
			name:           "secondary rate limit",
			status:         http.StatusForbidden,
			message:        "You have exceeded a secondary rate limit.",
			header:         http.Header{"Retry-After": {"60"}},
			wantRetryAfter: time.Minute,
			wantDelay:      true,
		},
		{
			name:    "secondary rate limit without headers",
			status:  http.StatusForbidden,
			message: "You have exceeded a secondary rate limit.",
		},
		{
			name:           "too many requests",
			status:         http.StatusTooManyRequests,
			header:         http.Header{"Retry-After": {"9"}},
			wantRetryAfter: 9 * time.Second,
			wantDelay:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newGitHubFixture(t)
			f.api.setFailureResponse(tt.status, tt.message, tt.header)
			logger, logs := newTestLogger()
			sink, err := NewGitHub(GitHubOptions{Repos: f.app, Apps: map[string]bool{"blog": true}, Logger: logger, Now: f.clock.Now})
			if err != nil {
				t.Fatalf("NewGitHub: %v", err)
			}
			err = deliver(t, sink, sampleNamed(t, "deploy-started"))
			if err == nil || !retryable(err) || isReported(err) {
				t.Fatalf("deliver() = %v, want a retryable, unreported error", err)
			}
			delay, ok := retryAfter(err, time.Now())
			if ok != tt.wantDelay {
				t.Fatalf("retryAfter() ok = %t, want %t", ok, tt.wantDelay)
			}
			if tt.wantRetryAfter > 0 && delay != tt.wantRetryAfter {
				t.Errorf("retryAfter() = %s, want %s", delay, tt.wantRetryAfter)
			}
			if tt.wantDelay && delay <= 0 {
				t.Errorf("retryAfter() = %s, want a positive delay", delay)
			}
			if got := logs.count(t, "cannot report deployments to github", "WARN"); got != 0 {
				t.Errorf("permission warnings = %d, want 0: %s", got, logs)
			}
		})
	}
}

func TestGitHubSinkCacheIsBounded(t *testing.T) {
	t.Parallel()

	f := newGitHubFixture(t)
	sink, err := NewGitHub(GitHubOptions{Repos: f.app, Apps: map[string]bool{"blog": true}, CacheSize: 2})
	if err != nil {
		t.Fatalf("NewGitHub: %v", err)
	}
	for id := range 5 {
		n := sampleNamed(t, "deploy-started")
		n.Deployment.ID = core.DeploymentID(id + 1)
		if err := deliver(t, sink, n); err != nil {
			t.Fatalf("deliver: %v", err)
		}
	}
	if sink.cacheLen() != 2 {
		t.Errorf("cache holds %d entries, want 2", sink.cacheLen())
	}
	if _, err := NewGitHub(GitHubOptions{}); err == nil {
		t.Error("NewGitHub without a repository client source succeeded")
	}
}

func TestGitHubStatusDescriptionOmitsFailureReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		sample string
		want   string
	}{
		{sample: "deploy-failed", want: "Failed"},
		{sample: "rollback-failed", want: "Rollback failed"},
	}
	for _, tt := range tests {
		t.Run(tt.sample, func(t *testing.T) {
			t.Parallel()
			n := sampleNamed(t, tt.sample)
			if n.Deployment.FailureReason == "" {
				t.Fatal("sample has no failure reason")
			}
			if got := statusDescription(n); got != tt.want {
				t.Errorf("statusDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGitHubState(t *testing.T) {
	t.Parallel()

	want := map[core.NotificationKind]github.DeploymentState{
		core.NotificationDeployQueued:     github.DeploymentQueued,
		core.NotificationDeployStarted:    github.DeploymentInProgress,
		core.NotificationDeploySucceeded:  github.DeploymentSuccess,
		core.NotificationDeployRolledBack: github.DeploymentSuccess,
		core.NotificationDeployFailed:     github.DeploymentFailure,
		core.NotificationDeployCancelled:  github.DeploymentInactive,
		core.NotificationDeploySuperseded: github.DeploymentInactive,
	}
	for _, kind := range core.NotificationKinds() {
		got, ok := githubState(kind)
		w, wantOK := want[kind]
		if got != w || ok != wantOK {
			t.Errorf("githubState(%s) = %q, %v; want %q, %v", kind, got, ok, w, wantOK)
		}
		if terminal := isTerminal(kind); terminal != (wantOK && kind != core.NotificationDeployQueued && kind != core.NotificationDeployStarted) {
			t.Errorf("isTerminal(%s) = %v", kind, terminal)
		}
	}
}
