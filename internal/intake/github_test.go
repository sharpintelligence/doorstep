package intake_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
	"github.com/sharpintelligence/doorstep/internal/intake"
)

func newGitHubChecks(t *testing.T) (*githubtest.Server, intake.GitHubChecks) {
	t.Helper()
	srv := githubtest.NewServer(t)
	app, err := github.New(github.Options{AppID: srv.AppID, PrivateKey: srv.PrivateKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	return srv, intake.GitHubChecks{Clients: app}
}

func TestGitHubChecks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		withSuites bool
		wantCalls  int64
		wantSuites int
		wantState  core.ChecksState
	}{
		{name: "runs and statuses", withSuites: false, wantCalls: 2, wantState: core.ChecksSuccess},
		{name: "with suites", withSuites: true, wantCalls: 3, wantSuites: 2, wantState: core.ChecksPending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, checks := newGitHubChecks(t)
			srv.Install("Example-User", "blog", 7)
			srv.SetCheckRuns("example-user", "blog", shaA,
				githubtest.CheckRun{ID: 1, Name: "test", Status: "completed", Conclusion: "failure", AppSlug: "github-actions"},
				githubtest.CheckRun{ID: 2, Name: "test", Status: "completed", Conclusion: "success", AppSlug: "github-actions"},
			)
			srv.SetStatuses("example-user", "blog", shaA, githubtest.Status{Context: "ci/legacy", State: "success"})
			srv.SetCheckSuites("example-user", "blog", shaA,
				githubtest.CheckSuite{ID: 10, Status: "completed", Conclusion: "success", LatestCheckRunsCount: 1, AppSlug: "github-actions"},
				githubtest.CheckSuite{ID: 11, Status: "in_progress", LatestCheckRunsCount: 1, AppSlug: "github-actions"},
			)

			snapshot, err := checks.Checks(t.Context(), "Example-User/blog", shaA, tt.withSuites)
			if err != nil {
				t.Fatalf("Checks: %v", err)
			}
			if got := srv.ChecksCalls(); got != tt.wantCalls {
				t.Errorf("made %d API calls, want %d", got, tt.wantCalls)
			}
			if len(snapshot.CheckRuns) != 1 || snapshot.CheckRuns[0].ID != 2 || len(snapshot.Statuses) != 1 || len(snapshot.Suites) != tt.wantSuites {
				t.Errorf("snapshot = %+v", snapshot)
			}
			if state, _ := intake.Evaluate(nil, snapshot); state != tt.wantState {
				t.Errorf("Evaluate = %s, want %s", state, tt.wantState)
			}
		})
	}
}

func TestGitHubChecksErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		repository string
		sha        string
		wantIs     error
		wantStatus int
	}{
		{name: "not installed", repository: "example-user/private", sha: shaA, wantIs: github.ErrNotInstalled},
		{name: "invalid owner", repository: "-bad/blog", sha: shaA, wantIs: github.ErrInvalidRepository},
		{name: "not owner/name", repository: "blog", sha: shaA},
		{name: "unknown commit", repository: "example-user/blog", sha: shaB, wantStatus: http.StatusUnprocessableEntity},
		{name: "invalid SHA", repository: "example-user/blog", sha: "HEAD", wantIs: github.ErrInvalidCommitSHA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, checks := newGitHubChecks(t)
			srv.Install("example-user", "blog", 7)
			srv.SetCheckRuns("example-user", "blog", shaA)
			_, err := checks.Checks(t.Context(), tt.repository, tt.sha, true)
			if err == nil {
				t.Fatal("Checks succeeded")
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error = %v, want %v", err, tt.wantIs)
			}
			if tt.wantStatus != 0 {
				var apiErr *github.APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != tt.wantStatus {
					t.Errorf("error = %v, want API status %d", err, tt.wantStatus)
				}
			}
		})
	}
}
