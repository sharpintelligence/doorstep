package notify

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
)

type statusFixture struct {
	srv      *githubtest.Server
	app      *github.App
	observer *recordingObserver
	logs     *logBuffer
}

func newStatusFixture(t *testing.T) *statusFixture {
	t.Helper()
	srv := githubtest.NewServer(t)
	srv.Install("example-user", "blog", 1)
	app, err := github.New(github.Options{AppID: srv.AppID, PrivateKey: srv.PrivateKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	return &statusFixture{srv: srv, app: app, observer: &recordingObserver{}}
}

func (f *statusFixture) mirror(t *testing.T, mutate ...func(*GateStatusOptions)) *GateStatuses {
	t.Helper()
	logger, logs := newTestLogger()
	f.logs = logs
	opts := GateStatusOptions{
		Repos:    f.app,
		Apps:     map[string]GateStatusApp{"blog": {}},
		Links:    testLinks(),
		Logger:   logger,
		Observer: f.observer,
		Retry:    fastPolicy(),
		Now:      func() time.Time { return testTime },
	}
	for _, m := range mutate {
		m(&opts)
	}
	g, err := NewGateStatuses(opts)
	if err != nil {
		t.Fatalf("NewGateStatuses: %v", err)
	}
	t.Cleanup(func() { _ = g.Close(context.Background()) })
	return g
}

func (f *statusFixture) posted() []githubtest.Status {
	return f.srv.PostedStatuses("example-user", "blog", testSHA)
}

func closeMirror(t *testing.T, g *GateStatuses) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := g.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func record(t *testing.T, g *GateStatuses, d core.Deployment, r core.GateResult) {
	t.Helper()
	if err := g.GateRecorded(t.Context(), d, r); err != nil {
		t.Fatalf("GateRecorded: %v", err)
	}
}

func gate(name string, phase core.GatePhase, status core.GateStatus, message string) core.GateResult {
	return core.GateResult{DeploymentID: 42, Name: name, Type: name, Phase: phase, Status: status, Message: message}
}

type gatedHook struct {
	block   string
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func newGatedHook(block string) *gatedHook {
	return &gatedHook{block: block, release: make(chan struct{}), entered: make(chan struct{})}
}

func (h *gatedHook) hook(r *http.Request, st githubtest.Status) *githubtest.StatusReply {
	if st.Context != h.block {
		return nil
	}
	h.once.Do(func() { close(h.entered) })
	select {
	case <-h.release:
		return nil
	case <-r.Context().Done():
		return &githubtest.StatusReply{Code: http.StatusServiceUnavailable, Message: "client went away"}
	}
}

func (h *gatedHook) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocked status never reached GitHub")
	}
}

func TestGateStatusShape(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("ü", 200)
	tests := []struct {
		name       string
		deployment *core.Deployment
		result     core.GateResult
		messages   bool
		noLinks    bool
		want       githubtest.Status
	}{
		{
			name:       "pending",
			deployment: baseDeployment(42, core.StateVerifying),
			result:     gate("health", core.GatePhaseAfter, core.GatePending, "Application blog is Progressing on argocd.internal"),
			want: githubtest.Status{
				Context: "doorstep/blog/after/health", State: "pending", Description: "Gate pending",
				TargetURL: "https://doorstep.example.com/deployments/42",
			},
		},
		{
			name:       "passed",
			deployment: baseDeployment(42, core.StateQueued),
			result:     gate("checks", core.GatePhaseBefore, core.GatePassed, "all 3 checks passed"),
			want: githubtest.Status{
				Context: "doorstep/blog/before/checks", State: "success", Description: "Gate passed",
				TargetURL: "https://doorstep.example.com/deployments/42",
			},
		},
		{
			name:       "skipped counts as success",
			deployment: baseDeployment(42, core.StateQueued),
			result:     gate("lock", core.GatePhaseBefore, core.GateSkipped, "skipped for rollback"),
			want: githubtest.Status{
				Context: "doorstep/blog/before/lock", State: "success", Description: "Gate skipped",
				TargetURL: "https://doorstep.example.com/deployments/42",
			},
		},
		{
			name:       "failed",
			deployment: baseDeployment(42, core.StateVerifying),
			result:     gate("e2e", core.GatePhaseAfter, core.GateFailed, "e2e failed against staging.internal"),
			want: githubtest.Status{
				Context: "doorstep/blog/after/e2e", State: "failure", Description: "Gate failed",
				TargetURL: "https://doorstep.example.com/deployments/42",
			},
		},
		{
			name: "rollback",
			deployment: func() *core.Deployment {
				d := rollbackDeployment(core.StateVerifying)
				d.Commit.SHA = testSHA
				return d
			}(),
			result: gate("health", core.GatePhaseAfter, core.GatePassed, "Healthy"),
			want: githubtest.Status{
				Context: "doorstep/blog/after/health", State: "success", Description: "Rollback #43: Gate passed",
				TargetURL: "https://doorstep.example.com/deployments/43",
			},
		},
		{
			name:       "message opted in",
			deployment: baseDeployment(42, core.StateVerifying),
			result:     gate("health", core.GatePhaseAfter, core.GatePending, "Application blog is Progressing"),
			messages:   true,
			want: githubtest.Status{
				Context: "doorstep/blog/after/health", State: "pending", Description: "Application blog is Progressing",
				TargetURL: "https://doorstep.example.com/deployments/42",
			},
		},
		{
			name: "message opted in on a rollback",
			deployment: func() *core.Deployment {
				d := rollbackDeployment(core.StateVerifying)
				d.Commit.SHA = testSHA
				return d
			}(),
			result:   gate("health", core.GatePhaseAfter, core.GatePassed, "Healthy"),
			messages: true,
			want: githubtest.Status{
				Context: "doorstep/blog/after/health", State: "success", Description: "Rollback #43: Healthy",
				TargetURL: "https://doorstep.example.com/deployments/43",
			},
		},
		{
			name:       "message opted in but empty",
			deployment: baseDeployment(42, core.StateVerifying),
			result:     gate("health", core.GatePhaseAfter, core.GateFailed, "  "),
			messages:   true,
			want: githubtest.Status{
				Context: "doorstep/blog/after/health", State: "failure", Description: "Gate failed",
				TargetURL: "https://doorstep.example.com/deployments/42",
			},
		},
		{
			name:       "long message opted in",
			deployment: baseDeployment(42, core.StateVerifying),
			result:     gate("health", core.GatePhaseAfter, core.GateFailed, long),
			messages:   true,
			want: githubtest.Status{
				Context: "doorstep/blog/after/health", State: "failure", Description: strings.Repeat("ü", 139) + "…",
				TargetURL: "https://doorstep.example.com/deployments/42",
			},
		},
		{
			name:       "no public url",
			deployment: baseDeployment(42, core.StateVerifying),
			result:     gate("health", core.GatePhaseAfter, core.GatePending, "  "),
			noLinks:    true,
			want:       githubtest.Status{Context: "doorstep/blog/after/health", State: "pending", Description: "Gate pending"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newStatusFixture(t)
			g := f.mirror(t, func(o *GateStatusOptions) {
				o.Apps = map[string]GateStatusApp{"blog": {Messages: tt.messages}}
				if tt.noLinks {
					o.Links = Links{}
				}
			})
			record(t, g, *tt.deployment, tt.result)
			closeMirror(t, g)
			posted := f.posted()
			if len(posted) != 1 || posted[0] != tt.want {
				t.Fatalf("posted = %+v, want %+v", posted, tt.want)
			}
			if utf8.RuneCountInString(posted[0].Description) > 140 {
				t.Errorf("description too long: %d runes", utf8.RuneCountInString(posted[0].Description))
			}
			if f.observer.count(TypeGitHub, ResultSent) != 1 {
				t.Errorf("observer = %+v, want one sent", f.observer.seen)
			}
		})
	}
}

func TestGateStatusContext(t *testing.T) {
	t.Parallel()

	if got := GateStatusContext("blog", core.GatePhaseBefore, "approval"); got != "doorstep/blog/before/approval" {
		t.Errorf("GateStatusContext = %q", got)
	}
	if !core.IsDoorstepStatusContext(GateStatusContext("x", core.GatePhaseAfter, "y")) {
		t.Error("mirrored context does not carry the reserved prefix")
	}
}

func TestGateStatusesIgnore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(d *core.Deployment, r *core.GateResult)
	}{
		{name: "app not listed", mutate: func(d *core.Deployment, _ *core.GateResult) { d.Target.App = "other" }},
		{name: "invalid commit", mutate: func(d *core.Deployment, _ *core.GateResult) { d.Commit.SHA = "abc" }},
		{name: "invalid repository", mutate: func(d *core.Deployment, _ *core.GateResult) { d.Commit.Repository = "no-slash" }},
		{name: "unknown status", mutate: func(_ *core.Deployment, r *core.GateResult) { r.Status = "running" }},
		{name: "unknown phase", mutate: func(_ *core.Deployment, r *core.GateResult) { r.Phase = "during" }},
		{name: "unnamed gate", mutate: func(_ *core.Deployment, r *core.GateResult) { r.Name = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newStatusFixture(t)
			g := f.mirror(t)
			d := *baseDeployment(42, core.StateVerifying)
			r := gate("health", core.GatePhaseAfter, core.GatePassed, "ok")
			tt.mutate(&d, &r)
			record(t, g, d, r)
			closeMirror(t, g)
			if f.srv.StatusCalls() != 0 {
				t.Errorf("GitHub called %d times", f.srv.StatusCalls())
			}
			f.observer.mu.Lock()
			defer f.observer.mu.Unlock()
			if len(f.observer.seen) != 0 {
				t.Errorf("observer = %+v, want nothing", f.observer.seen)
			}
		})
	}
}

func TestGateStatusesCoalesceWhileGitHubIsSlow(t *testing.T) {
	t.Parallel()

	f := newStatusFixture(t)
	h := newGatedHook("doorstep/blog/before/checks")
	f.srv.OnCreateStatus(h.hook)
	g := f.mirror(t, func(o *GateStatusOptions) { o.Apps = map[string]GateStatusApp{"blog": {Messages: true}} })
	d := *baseDeployment(42, core.StateVerifying)

	record(t, g, d, gate("checks", core.GatePhaseBefore, core.GatePassed, "green"))
	h.waitEntered(t)

	started := time.Now()
	record(t, g, d, gate("health", core.GatePhaseAfter, core.GatePending, "Progressing"))
	record(t, g, d, gate("soak", core.GatePhaseAfter, core.GatePending, "soaking"))
	record(t, g, d, gate("health", core.GatePhaseAfter, core.GatePending, "still Progressing"))
	record(t, g, d, gate("health", core.GatePhaseAfter, core.GatePassed, "Healthy"))
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("GateRecorded blocked for %s while GitHub was slow", elapsed)
	}
	close(h.release)
	closeMirror(t, g)

	var got []string
	for _, st := range f.posted() {
		got = append(got, st.Context+"="+st.State+":"+st.Description)
	}
	want := []string{
		"doorstep/blog/before/checks=success:green",
		"doorstep/blog/after/health=success:Healthy",
		"doorstep/blog/after/soak=pending:soaking",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("posted:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if f.observer.count(TypeGitHub, ResultSent) != 3 {
		t.Errorf("observer = %+v, want three sent", f.observer.seen)
	}
}

func TestGateStatusesKeepOrderPerContext(t *testing.T) {
	t.Parallel()

	f := newStatusFixture(t)
	g := f.mirror(t)
	d := *baseDeployment(42, core.StateVerifying)
	statuses := []core.GateStatus{core.GatePending, core.GatePassed}
	for i, status := range statuses {
		record(t, g, d, gate("health", core.GatePhaseAfter, status, string(status)))
		waitFor(t, "status delivery", func() bool { return len(f.posted()) == i+1 })
	}
	closeMirror(t, g)
	posted := f.posted()
	if len(posted) != 2 || posted[0].State != "pending" || posted[1].State != "success" {
		t.Errorf("posted = %+v, want pending then success", posted)
	}
}

func TestGateStatusesRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		failures  int
		reply     githubtest.StatusReply
		wantCalls int64
		wantSent  int
	}{
		{name: "server error", failures: 2, reply: githubtest.StatusReply{Code: http.StatusBadGateway, Message: "bad gateway"}, wantCalls: 3, wantSent: 1},
		{name: "rate limited", failures: 1, reply: githubtest.StatusReply{Code: http.StatusTooManyRequests, Message: "slow down", Header: http.Header{"Retry-After": {"1"}}}, wantCalls: 2, wantSent: 1},
		{name: "secondary rate limit", failures: 1, reply: githubtest.StatusReply{Code: http.StatusForbidden, Message: "You have exceeded a secondary rate limit"}, wantCalls: 2, wantSent: 1},
		{name: "gives up after four attempts", failures: 10, reply: githubtest.StatusReply{Code: http.StatusServiceUnavailable, Message: "unavailable"}, wantCalls: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newStatusFixture(t)
			var calls atomic.Int32
			f.srv.OnCreateStatus(func(*http.Request, githubtest.Status) *githubtest.StatusReply {
				if int(calls.Add(1)) <= tt.failures {
					return &tt.reply
				}
				return nil
			})
			var slept []time.Duration
			var mu sync.Mutex
			g := f.mirror(t, func(o *GateStatusOptions) {
				o.Retry.Sleep = func(ctx context.Context, d time.Duration) error {
					mu.Lock()
					slept = append(slept, d)
					mu.Unlock()
					return ctx.Err()
				}
			})
			record(t, g, *baseDeployment(42, core.StateVerifying), gate("health", core.GatePhaseAfter, core.GatePassed, "Healthy"))
			closeMirror(t, g)
			if f.srv.StatusCalls() != tt.wantCalls {
				t.Errorf("calls = %d, want %d", f.srv.StatusCalls(), tt.wantCalls)
			}
			if len(f.posted()) != tt.wantSent {
				t.Errorf("posted = %+v, want %d", f.posted(), tt.wantSent)
			}
			if f.observer.count(TypeGitHub, ResultSent) != tt.wantSent || f.observer.count(TypeGitHub, ResultFailed) != 1-tt.wantSent {
				t.Errorf("observer = %+v", f.observer.seen)
			}
			mu.Lock()
			defer mu.Unlock()
			if int64(len(slept)) != tt.wantCalls-1 {
				t.Errorf("slept %v, want %d sleeps", slept, tt.wantCalls-1)
			}
			if tt.reply.Header != nil && (len(slept) == 0 || slept[0] != time.Second) {
				t.Errorf("slept %v, want the Retry-After of 1s", slept)
			}
		})
	}
}

func TestGateStatusesPermissionDenied(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(f *statusFixture)
	}{
		{name: "forbidden", setup: func(f *statusFixture) {
			f.srv.OnCreateStatus(func(*http.Request, githubtest.Status) *githubtest.StatusReply {
				return &githubtest.StatusReply{Code: http.StatusForbidden, Message: "Resource not accessible by integration"}
			})
		}},
		{name: "unprocessable", setup: func(f *statusFixture) {
			f.srv.OnCreateStatus(func(*http.Request, githubtest.Status) *githubtest.StatusReply {
				return &githubtest.StatusReply{Code: http.StatusUnprocessableEntity, Message: "No commit found for SHA"}
			})
		}},
		{name: "not installed", setup: func(f *statusFixture) { f.srv.Uninstall(1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newStatusFixture(t)
			tt.setup(f)
			g := f.mirror(t)
			d := *baseDeployment(42, core.StateVerifying)
			record(t, g, d, gate("health", core.GatePhaseAfter, core.GatePending, "Progressing"))
			record(t, g, d, gate("checks", core.GatePhaseBefore, core.GatePassed, "green"))
			closeMirror(t, g)
			if f.srv.StatusCalls() > 2 {
				t.Errorf("calls = %d, want no retries", f.srv.StatusCalls())
			}
			if f.observer.count(TypeGitHub, ResultFailed) != 2 {
				t.Errorf("observer = %+v, want two failures", f.observer.seen)
			}
			warnings := f.logs.count(t, "cannot mirror gates to github", "WARN")
			if tt.name == "unprocessable" {
				if warnings != 0 {
					t.Errorf("permission warnings = %d for a 422, want 0", warnings)
				}
				return
			}
			if warnings != 1 {
				t.Errorf("permission warnings = %d, want exactly 1", warnings)
			}
			if !strings.Contains(f.logs.String(), "Commit statuses: Read and write") {
				t.Errorf("warning lacks the permission hint: %s", f.logs.String())
			}
		})
	}
}

func TestGateStatusesDropWhenFull(t *testing.T) {
	t.Parallel()

	f := newStatusFixture(t)
	h := newGatedHook("doorstep/blog/before/a")
	f.srv.OnCreateStatus(h.hook)
	g := f.mirror(t, func(o *GateStatusOptions) { o.MaxPending = 2 })
	d := *baseDeployment(42, core.StateVerifying)

	record(t, g, d, gate("a", core.GatePhaseBefore, core.GatePassed, "a"))
	h.waitEntered(t)
	record(t, g, d, gate("b", core.GatePhaseBefore, core.GatePassed, "b"))
	record(t, g, d, gate("c", core.GatePhaseBefore, core.GatePassed, "c"))
	record(t, g, d, gate("b", core.GatePhaseBefore, core.GateFailed, "b again"))
	record(t, g, d, gate("d", core.GatePhaseBefore, core.GatePassed, "d"))
	close(h.release)
	closeMirror(t, g)

	var got []string
	for _, st := range f.posted() {
		got = append(got, st.Context+"="+st.State)
	}
	want := []string{"doorstep/blog/before/a=success", "doorstep/blog/before/b=failure", "doorstep/blog/before/c=success"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("posted %v, want %v", got, want)
	}
	if f.observer.count(TypeGitHub, ResultDropped) != 1 {
		t.Errorf("observer = %+v, want one drop", f.observer.seen)
	}
	if f.logs.count(t, "gate commit status dropped", "WARN") != 1 {
		t.Errorf("drop warnings = %d, want 1", f.logs.count(t, "gate commit status dropped", "WARN"))
	}
}

func TestGateStatusesCloseDeadline(t *testing.T) {
	t.Parallel()

	f := newStatusFixture(t)
	h := newGatedHook("doorstep/blog/before/a")
	f.srv.OnCreateStatus(h.hook)
	g := f.mirror(t, func(o *GateStatusOptions) { o.Retry.Attempts = 1 })
	d := *baseDeployment(42, core.StateVerifying)
	record(t, g, d, gate("a", core.GatePhaseBefore, core.GatePassed, "a"))
	h.waitEntered(t)
	record(t, g, d, gate("b", core.GatePhaseBefore, core.GatePassed, "b"))

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := g.Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want DeadlineExceeded", err)
	}
	if f.observer.count(TypeGitHub, ResultFailed) != 1 || f.observer.count(TypeGitHub, ResultDropped) != 1 {
		t.Errorf("observer = %+v, want the in-flight status failed and the queued one dropped", f.observer.seen)
	}

	record(t, g, d, gate("c", core.GatePhaseBefore, core.GatePassed, "c"))
	if f.observer.count(TypeGitHub, ResultDropped) != 2 {
		t.Errorf("observer = %+v, want a status recorded after Close dropped", f.observer.seen)
	}
	if err := g.Close(t.Context()); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if len(f.posted()) != 0 {
		t.Errorf("posted = %+v", f.posted())
	}
}

func TestNewGateStatusesNeedsRepos(t *testing.T) {
	t.Parallel()

	if _, err := NewGateStatuses(GateStatusOptions{}); err == nil {
		t.Error("NewGateStatuses without repositories succeeded")
	}
}
