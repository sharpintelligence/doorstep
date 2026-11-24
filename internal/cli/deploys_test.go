package cli

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/deploys"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
	"github.com/sharpintelligence/doorstep/internal/intake"
	"github.com/sharpintelligence/doorstep/internal/queue"
	"github.com/sharpintelligence/doorstep/internal/rollback"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
	"github.com/sharpintelligence/doorstep/internal/web"
)

func TestDeployApps(t *testing.T) {
	t.Parallel()

	got := deployApps(oneAppCatalog(t))
	want := []deploys.App{{Target: core.AppTarget("blog"), Repository: "example-user/blog", Branch: "main", ImageRepository: "ghcr.io/example-user/blog"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deployApps = %+v, want %+v", got, want)
	}
}

func TestWebApps(t *testing.T) {
	t.Parallel()

	file, err := config.Parse(strings.NewReader(argoConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := webApps(file.Catalog())
	if len(got) != 2 {
		t.Fatalf("webApps = %+v", got)
	}
	want := web.App{Name: "shop", Target: core.AppTarget("shop"), Repository: got[1].Repository, Branch: got[1].Branch, GitOpsRepository: "example-user/homelab", ArgoCDApplication: "apps/shop-prod"}
	if got[1] != want || got[1].Repository == "" || got[1].Branch == "" {
		t.Fatalf("webApps[1] = %+v, want %+v", got[1], want)
	}
}

func TestBranchCommits(t *testing.T) {
	t.Parallel()

	srv := githubtest.NewServer(t)
	fake := srv.AddRepo("example-user", "blog", 5)
	first := fake.Seed("main", "a.txt", "1\n")
	fake.CreateBranch("feature", "main")
	second := fake.Commit("main", "second", map[string]string{"a.txt": "2\n"})
	unmerged := fake.Commit("feature", "unmerged", map[string]string{"b.txt": "1\n"})
	app, err := github.New(github.Options{AppID: srv.AppID, PrivateKey: srv.PrivateKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	commits := branchCommits{repos: app}

	tests := []struct {
		name       string
		repository string
		branch     string
		sha        string
		want       bool
		wantErr    error
		wantErrMsg string
	}{
		{name: "branch head", repository: "example-user/blog", branch: "main", sha: second, want: true},
		{name: "ancestor", repository: "example-user/blog", branch: "main", sha: first, want: true},
		{name: "unmerged branch commit", repository: "example-user/blog", branch: "main", sha: unmerged},
		{name: "unknown commit", repository: "example-user/blog", branch: "main", sha: strings.Repeat("e", 40), wantErr: deploys.ErrCommitNotFound},
		{name: "missing branch", repository: "example-user/blog", branch: "gone", sha: first, wantErr: github.ErrRefNotFound},
		{name: "malformed repository", repository: "blog", branch: "main", sha: first, wantErrMsg: "must be owner/name"},
		{name: "not installed", repository: "example-user/other", branch: "main", sha: first, wantErrMsg: "not installed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := commits.OnBranch(t.Context(), tt.repository, tt.branch, tt.sha)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("OnBranch error = %v, want %v", err, tt.wantErr)
				}
			case tt.wantErrMsg != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Fatalf("OnBranch error = %v, want %q", err, tt.wantErrMsg)
				}
			case err != nil || got != tt.want:
				t.Fatalf("OnBranch = %v, %v, want %v", got, err, tt.want)
			}
		})
	}
}

type fakeFlagger struct {
	flag intake.MigrationFlag
	err  error
}

func (f fakeFlagger) MigrationFlag(context.Context, core.Target, core.Commit) (intake.MigrationFlag, error) {
	return f.flag, f.err
}

func TestIntakeMigrations(t *testing.T) {
	t.Parallel()

	flagged, reason, err := intakeMigrations{intake: fakeFlagger{flag: intake.MigrationFlag{Flagged: true, Reason: "label db"}}}.MigrationFlag(t.Context(), core.AppTarget("blog"), core.Commit{})
	if err != nil || !flagged || reason != "label db" {
		t.Fatalf("MigrationFlag = %v, %q, %v", flagged, reason, err)
	}
	boom := errors.New("boom")
	if _, _, err := (intakeMigrations{intake: fakeFlagger{err: boom}}).MigrationFlag(t.Context(), core.AppTarget("blog"), core.Commit{}); !errors.Is(err, boom) {
		t.Fatalf("MigrationFlag error = %v", err)
	}
}

type stubDeploys struct {
	result    deploys.Result
	cancelled core.Deployment
	err       error
}

func (s stubDeploys) Deploy(context.Context, deploys.Request) (deploys.Result, error) {
	return s.result, s.err
}

func (s stubDeploys) Cancel(context.Context, deploys.CancelRequest) (core.Deployment, error) {
	return s.cancelled, s.err
}

func TestAnnouncingDeploys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		deploys       stubDeploys
		cancelCtx     bool
		wantQueued    []core.DeploymentID
		wantCancelled []core.DeploymentID
	}{
		{
			name:          "created and cancelled",
			deploys:       stubDeploys{result: deploys.Result{Deployment: core.Deployment{ID: 4}, Created: true}, cancelled: core.Deployment{ID: 6}},
			wantQueued:    []core.DeploymentID{4},
			wantCancelled: []core.DeploymentID{6},
		},
		{
			name:          "after the caller gave up",
			deploys:       stubDeploys{result: deploys.Result{Deployment: core.Deployment{ID: 4}, Created: true}, cancelled: core.Deployment{ID: 6}},
			cancelCtx:     true,
			wantQueued:    []core.DeploymentID{4},
			wantCancelled: []core.DeploymentID{6},
		},
		{
			name:          "existing deployment is not announced again",
			deploys:       stubDeploys{result: deploys.Result{Deployment: core.Deployment{ID: 4}}, cancelled: core.Deployment{ID: 6}},
			wantCancelled: []core.DeploymentID{6},
		},
		{name: "refused", deploys: stubDeploys{err: deploys.ErrCommitNotOnBranch}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := &recordingAnnouncer{}
			d := announcingDeploys{deploys: tt.deploys, announcer: a}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancelCtx {
				cancel()
			}
			if _, err := d.Deploy(ctx, deploys.Request{}); !errors.Is(err, tt.deploys.err) {
				t.Errorf("Deploy error = %v", err)
			}
			if _, err := d.Cancel(ctx, deploys.CancelRequest{}); !errors.Is(err, tt.deploys.err) {
				t.Errorf("Cancel error = %v", err)
			}
			if !reflect.DeepEqual(a.queued, tt.wantQueued) || !reflect.DeepEqual(a.cancelled, tt.wantCancelled) {
				t.Errorf("queued = %v, cancelled = %v, want %v and %v", a.queued, a.cancelled, tt.wantQueued, tt.wantCancelled)
			}
			if a.expired {
				t.Error("announcement ran on a cancelled context")
			}
		})
	}
}

func TestNewWebServesTheStore(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	locksService, _, err := newLocks(st, config.Default().Locks, nil, nil)
	if err != nil {
		t.Fatalf("newLocks: %v", err)
	}
	rollbacks, err := rollback.New(rollback.Options{Repository: rollback.NewRepository(st)})
	if err != nil {
		t.Fatalf("rollback.New: %v", err)
	}
	authService, err := auth.New(auth.Options{Repository: auth.NewRepository(st)})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	succeededDeployment(t, st, strings.Repeat("a", 40), cliGoodDigest)
	routes, err := newWeb(webInput{
		config:        config.Default(),
		catalog:       oneAppCatalog(t),
		history:       st,
		queue:         queue.New(queue.Options{Store: st}),
		locks:         locksService,
		lockEvents:    st,
		rollbacks:     rollbacks,
		blocks:        rollbacks,
		audit:         st,
		tokens:        authService.Tokens(),
		authenticator: authService.Authenticator(),
		logger:        slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("newWeb: %v", err)
	}
	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.Handle(pattern, h)
	}
	principal := auth.Principal{Actor: core.Actor{Kind: core.ActorUser, ID: "oidc:alice", Name: "alice"}, Role: core.RoleViewer, Method: auth.MethodSession}
	for path, want := range map[string]string{"/": `href="/apps/blog"`, "/apps/blog": "Deployments of blog", "/locks": "No app is locked", "/audit": `<option value="blog">`} {
		req := httptest.NewRequestWithContext(auth.WithPrincipal(t.Context(), principal), http.MethodGet, path, http.NoBody)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("GET %s = %d without %q:\n%s", path, rec.Code, want, rec.Body.String())
		}
	}
	if !strings.Contains(routesBody(t, mux, principal, "/apps/blog"), "https://github.com/example-user/blog/commit/aaaaaaaa") {
		t.Error("app page does not link commits to GitHub")
	}
	if _, ok := routes["POST /apps/{app}/deployments"]; ok {
		t.Error("deploy route mounted without a deploy service")
	}
}

func routesBody(t *testing.T, h http.Handler, p auth.Principal, path string) string {
	t.Helper()
	req := httptest.NewRequestWithContext(auth.WithPrincipal(t.Context(), p), http.MethodGet, path, http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Body.String()
}
