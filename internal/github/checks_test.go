package github

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
)

const checksSHA = "0123456789abcdef0123456789abcdef01234567"

func checkRuns(n int) []githubtest.CheckRun {
	runs := make([]githubtest.CheckRun, n)
	for i := range runs {
		runs[i] = githubtest.CheckRun{ID: int64(i + 1), Name: "job-" + strconv.Itoa(i), Status: "completed", Conclusion: "success"}
	}
	return runs
}

func statuses(n int) []githubtest.Status {
	out := make([]githubtest.Status, n)
	for i := range out {
		out[i] = githubtest.Status{Context: "ci/" + strconv.Itoa(i), State: "success"}
	}
	return out
}

func TestListCheckRunsPagination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		runs      int
		wantCalls int64
		wantErr   error
	}{
		{name: "none", runs: 0, wantCalls: 1},
		{name: "one page", runs: 3, wantCalls: 1},
		{name: "exactly one page", runs: 100, wantCalls: 1},
		{name: "three pages", runs: 250, wantCalls: 3},
		{name: "at the cap", runs: 1000, wantCalls: 10},
		{name: "past the cap", runs: 1001, wantCalls: 10, wantErr: ErrTooManyChecks},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.srv.Install("acme", "app", 1)
			f.srv.SetCheckRuns("acme", "app", checksSHA, checkRuns(tt.runs)...)
			client := f.repoClient(t, "acme", "app")

			got, err := client.ListCheckRuns(t.Context(), "acme", "app", checksSHA)
			if calls := f.srv.ChecksCalls(); calls != tt.wantCalls {
				t.Errorf("made %d API calls, want %d", calls, tt.wantCalls)
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) || got != nil {
					t.Fatalf("ListCheckRuns = %d runs, %v, want %v", len(got), err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListCheckRuns: %v", err)
			}
			if got == nil || len(got) != tt.runs {
				t.Fatalf("ListCheckRuns returned %d runs, want %d", len(got), tt.runs)
			}
			for i, run := range got {
				if run.ID != int64(i+1) || run.Name != "job-"+strconv.Itoa(i) {
					t.Fatalf("run %d = %+v, out of order or duplicated", i, run)
				}
			}
		})
	}
}

func TestListCheckRunsDecodesFields(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Install("acme", "app", 1)
	f.srv.SetCheckRuns("acme", "app", checksSHA,
		githubtest.CheckRun{ID: 7, Name: "test", Status: "completed", Conclusion: "failure", HTMLURL: "https://example.test/runs/7", AppSlug: "github-actions"},
		githubtest.CheckRun{ID: 8, Name: "lint", Status: "in_progress"},
		githubtest.CheckRun{ID: 9, Name: "test", Status: "completed", Conclusion: "success"},
	)
	got, err := f.repoClient(t, "acme", "app").ListCheckRuns(t.Context(), "acme", "app", checksSHA)
	if err != nil {
		t.Fatalf("ListCheckRuns: %v", err)
	}
	want := []CheckRun{
		{ID: 9, Name: "test", Status: "completed", Conclusion: "success"},
		{ID: 8, Name: "lint", Status: "in_progress"},
	}
	if len(got) != len(want) {
		t.Fatalf("ListCheckRuns = %+v, want latest runs %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("run %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	f.srv.SetCheckRuns("acme", "app", checksSHA, githubtest.CheckRun{ID: 7, Name: "test", Status: "completed", Conclusion: "failure", HTMLURL: "https://example.test/runs/7", AppSlug: "github-actions"})
	got, err = f.repoClient(t, "acme", "app").ListCheckRuns(t.Context(), "acme", "app", checksSHA)
	if err != nil {
		t.Fatalf("ListCheckRuns: %v", err)
	}
	full := CheckRun{ID: 7, Name: "test", Status: "completed", Conclusion: "failure", HTMLURL: "https://example.test/runs/7", App: AppRef{Slug: "github-actions"}}
	if len(got) != 1 || got[0] != full {
		t.Errorf("ListCheckRuns = %+v, want %+v", got, full)
	}
}

func TestCombinedStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		statuses  []githubtest.Status
		wantCalls int64
	}{
		{name: "none", wantCalls: 1},
		{name: "two pages", statuses: statuses(150), wantCalls: 2},
		{name: "fields", statuses: []githubtest.Status{{Context: "ci/build", State: "failure", Description: "broken", TargetURL: "https://ci.example.test/1"}}, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.srv.Install("acme", "app", 1)
			f.srv.SetStatuses("acme", "app", checksSHA, tt.statuses...)
			got, err := f.repoClient(t, "acme", "app").CombinedStatus(t.Context(), "acme", "app", checksSHA)
			if err != nil {
				t.Fatalf("CombinedStatus: %v", err)
			}
			if calls := f.srv.ChecksCalls(); calls != tt.wantCalls {
				t.Errorf("made %d API calls, want %d", calls, tt.wantCalls)
			}
			if len(got) != len(tt.statuses) {
				t.Fatalf("CombinedStatus returned %d statuses, want %d", len(got), len(tt.statuses))
			}
			for i, want := range tt.statuses {
				if got[i] != (CommitStatus{Context: want.Context, State: want.State, Description: want.Description, TargetURL: want.TargetURL}) {
					t.Errorf("status %d = %+v, want %+v", i, got[i], want)
				}
			}
		})
	}
}

func TestListCheckSuites(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Install("acme", "app", 1)
	f.srv.SetCheckSuites("acme", "app", checksSHA,
		githubtest.CheckSuite{ID: 1, Status: "completed", Conclusion: "success", LatestCheckRunsCount: 3, AppSlug: "github-actions"},
		githubtest.CheckSuite{ID: 2, Status: "queued", AppSlug: "other-ci"},
	)
	got, err := f.repoClient(t, "acme", "app").ListCheckSuites(t.Context(), "acme", "app", checksSHA)
	if err != nil {
		t.Fatalf("ListCheckSuites: %v", err)
	}
	want := []CheckSuite{
		{ID: 1, Status: "completed", Conclusion: "success", LatestCheckRunsCount: 3, App: AppRef{Slug: "github-actions"}},
		{ID: 2, Status: "queued", App: AppRef{Slug: "other-ci"}},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ListCheckSuites = %+v, want %+v", got, want)
	}
}

func TestChecksErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		owner      string
		repo       string
		sha        string
		wantIs     error
		wantStatus int
		wantCalls  int64
	}{
		{name: "short SHA", owner: "acme", repo: "app", sha: "abc1234", wantIs: ErrInvalidCommitSHA},
		{name: "uppercase SHA", owner: "acme", repo: "app", sha: strings.ToUpper(checksSHA), wantIs: ErrInvalidCommitSHA},
		{name: "path traversal in SHA", owner: "acme", repo: "app", sha: "../../installation", wantIs: ErrInvalidCommitSHA},
		{name: "invalid owner", owner: "acme/evil", repo: "app", sha: checksSHA, wantIs: ErrInvalidRepository},
		{name: "invalid repository", owner: "acme", repo: "..", sha: checksSHA, wantIs: ErrInvalidRepository},
		{name: "unknown commit", owner: "acme", repo: "app", sha: strings.Repeat("f", 40), wantStatus: http.StatusUnprocessableEntity, wantCalls: 1},
		{name: "repository outside the installation", owner: "other", repo: "app", sha: checksSHA, wantStatus: http.StatusNotFound, wantCalls: 1},
	}
	calls := []struct {
		name string
		call func(c *Client, owner, repo, sha string) error
	}{
		{name: "check runs", call: func(c *Client, owner, repo, sha string) error {
			_, err := c.ListCheckRuns(t.Context(), owner, repo, sha)
			return err
		}},
		{name: "statuses", call: func(c *Client, owner, repo, sha string) error {
			_, err := c.CombinedStatus(t.Context(), owner, repo, sha)
			return err
		}},
		{name: "suites", call: func(c *Client, owner, repo, sha string) error {
			_, err := c.ListCheckSuites(t.Context(), owner, repo, sha)
			return err
		}},
	}
	for _, tt := range tests {
		for _, c := range calls {
			t.Run(tt.name+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				f := newFixture(t)
				f.srv.Install("acme", "app", 1)
				f.srv.Install("other", "app", 2)
				f.srv.SetCheckRuns("acme", "app", checksSHA)
				f.srv.SetCheckRuns("other", "app", checksSHA)
				err := c.call(f.repoClient(t, "acme", "app"), tt.owner, tt.repo, tt.sha)
				if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
					t.Errorf("error = %v, want %v", err, tt.wantIs)
				}
				if tt.wantStatus != 0 {
					var apiErr *APIError
					if !errors.As(err, &apiErr) || apiErr.StatusCode != tt.wantStatus {
						t.Errorf("error = %v, want API status %d", err, tt.wantStatus)
					}
				}
				if got := f.srv.ChecksCalls(); got != tt.wantCalls {
					t.Errorf("made %d API calls, want %d", got, tt.wantCalls)
				}
			})
		}
	}
}

func TestChecksRequireInstallationToken(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Install("acme", "app", 1)
	f.srv.SetCheckRuns("acme", "app", checksSHA, checkRuns(1)...)
	unauthenticated := &Client{baseURL: f.app.baseURL, http: &http.Client{}}
	_, err := unauthenticated.ListCheckRuns(t.Context(), "acme", "app", checksSHA)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated ListCheckRuns error = %v, want 401", err)
	}
	if f.srv.ChecksCalls() != 0 {
		t.Errorf("unauthenticated request reached the checks handler")
	}
}
