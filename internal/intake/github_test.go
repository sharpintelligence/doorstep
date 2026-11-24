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

func newGitHub(t *testing.T) (*githubtest.Server, intake.GitHub) {
	t.Helper()
	srv := githubtest.NewServer(t)
	app, err := github.New(github.Options{AppID: srv.AppID, PrivateKey: srv.PrivateKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	return srv, intake.GitHub{Clients: app}
}

func TestGitHubChecks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		names      []string
		wantCalls  int64
		wantRuns   []int64
		wantSuites int
		wantState  core.ChecksState
	}{
		{name: "every check with suites", wantCalls: 3, wantRuns: []int64{2, 3}, wantSuites: 2, wantState: core.ChecksPending},
		{name: "one required check", names: []string{"test"}, wantCalls: 2, wantRuns: []int64{2}, wantState: core.ChecksSuccess},
		{name: "required checks deduplicated", names: []string{"test", "lint", "test"}, wantCalls: 3, wantRuns: []int64{2, 3}, wantState: core.ChecksPending},
		{name: "required status only", names: []string{"ci/legacy"}, wantCalls: 2, wantState: core.ChecksSuccess},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, gh := newGitHub(t)
			srv.Install("Example-User", "blog", 7)
			srv.SetCheckRuns("example-user", "blog", shaA,
				githubtest.CheckRun{ID: 1, Name: "test", Status: "completed", Conclusion: "failure", AppSlug: "github-actions"},
				githubtest.CheckRun{ID: 2, Name: "test", Status: "completed", Conclusion: "success", AppSlug: "github-actions"},
				githubtest.CheckRun{ID: 3, Name: "lint", Status: "in_progress", AppSlug: "github-actions"},
			)
			srv.SetStatuses("example-user", "blog", shaA, githubtest.Status{Context: "ci/legacy", State: "success"})
			srv.SetCheckSuites("example-user", "blog", shaA,
				githubtest.CheckSuite{ID: 10, Status: "completed", Conclusion: "success", LatestCheckRunsCount: 1, AppSlug: "github-actions"},
				githubtest.CheckSuite{ID: 11, Status: "in_progress", LatestCheckRunsCount: 1, AppSlug: "github-actions"},
			)

			snapshot, err := gh.Checks(t.Context(), "Example-User/blog", shaA, tt.names)
			if err != nil {
				t.Fatalf("Checks: %v", err)
			}
			if got := srv.ChecksCalls(); got != tt.wantCalls {
				t.Errorf("made %d API calls, want %d", got, tt.wantCalls)
			}
			var runs []int64
			for _, r := range snapshot.CheckRuns {
				runs = append(runs, r.ID)
			}
			if len(runs) != len(tt.wantRuns) || len(snapshot.Statuses) != 1 || len(snapshot.Suites) != tt.wantSuites {
				t.Errorf("snapshot = %+v, want runs %v and %d suites", snapshot, tt.wantRuns, tt.wantSuites)
			}
			for i := range min(len(runs), len(tt.wantRuns)) {
				if runs[i] != tt.wantRuns[i] {
					t.Errorf("runs = %v, want %v", runs, tt.wantRuns)
				}
			}
			if state, _ := intake.Evaluate(tt.names, snapshot); state != tt.wantState {
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
		for _, names := range [][]string{nil, {"test"}} {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				srv, gh := newGitHub(t)
				srv.Install("example-user", "blog", 7)
				srv.SetCheckRuns("example-user", "blog", shaA)
				_, err := gh.Checks(t.Context(), tt.repository, tt.sha, names)
				assertGitHubError(t, err, tt.wantIs, tt.wantStatus)
			})
		}
	}
}

func TestGitHubBranchHead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		repository string
		branch     string
		want       string
		wantErr    bool
		wantIs     error
	}{
		{name: "head", repository: "Example-User/blog", branch: "main", want: shaA},
		{name: "nested branch", repository: "example-user/blog", branch: "release/1.x", want: shaB},
		{name: "missing branch", repository: "example-user/blog", branch: "gone"},
		{name: "not installed", repository: "example-user/private", branch: "main", wantErr: true, wantIs: github.ErrNotInstalled},
		{name: "not owner/name", repository: "blog", branch: "main", wantErr: true},
		{name: "invalid branch", repository: "example-user/blog", branch: "../main", wantErr: true, wantIs: github.ErrInvalidBranch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, gh := newGitHub(t)
			srv.Install("example-user", "blog", 7)
			srv.SetBranch("example-user", "blog", "main", shaA)
			srv.SetBranch("example-user", "blog", "release/1.x", shaB)
			head, err := gh.BranchHead(t.Context(), tt.repository, tt.branch)
			if !tt.wantErr {
				if err != nil || head != tt.want {
					t.Errorf("BranchHead = %q, %v, want %q", head, err, tt.want)
				}
				return
			}
			assertGitHubError(t, err, tt.wantIs, 0)
		})
	}
}

func assertGitHubError(t *testing.T, err error, wantIs error, wantStatus int) {
	t.Helper()
	if err == nil {
		t.Fatal("call succeeded, want an error")
	}
	if wantIs != nil && !errors.Is(err, wantIs) {
		t.Errorf("error = %v, want %v", err, wantIs)
	}
	if wantStatus != 0 {
		var apiErr *github.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != wantStatus {
			t.Errorf("error = %v, want API status %d", err, wantStatus)
		}
	}
}
