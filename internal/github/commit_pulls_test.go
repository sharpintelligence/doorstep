package github

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
)

func TestClientCommitRange(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	fake := f.srv.AddRepo("acme", "blog", 9)
	first := fake.Seed("main", "a.txt", "1")
	second := fake.Commit("main", "second\n\nDoorstep-Migration: true", map[string]string{"a.txt": "2"})
	third := fake.Commit("main", "third", map[string]string{"a.txt": "3"})
	fourth := fake.Commit("main", "fourth", map[string]string{"a.txt": "4"})
	client := f.repoClient(t, "acme", "blog")

	tests := []struct {
		name       string
		base, head string
		limit      int
		want       CommitRange
		wantIs     error
		wantStatus int
	}{
		{
			name: "ahead lists commits oldest first", base: first, head: fourth, limit: 50,
			want: CommitRange{Status: ComparisonAhead, Total: 3, Commits: []CommitInfo{
				{SHA: second, Message: "second\n\nDoorstep-Migration: true"}, {SHA: third, Message: "third"}, {SHA: fourth, Message: "fourth"},
			}},
		},
		{
			name: "limit caps the commits but not the total", base: first, head: fourth, limit: 2,
			want: CommitRange{Status: ComparisonAhead, Total: 3, Commits: []CommitInfo{
				{SHA: second, Message: "second\n\nDoorstep-Migration: true"}, {SHA: third, Message: "third"},
			}},
		},
		{name: "identical", base: third, head: third, limit: 50, want: CommitRange{Status: ComparisonIdentical}},
		{name: "behind", base: fourth, head: second, limit: 50, want: CommitRange{Status: ComparisonBehind}},
		{name: "unknown commit", base: first, head: strings.Repeat("f", 40), limit: 50, wantIs: ErrCommitNotFound, wantStatus: http.StatusNotFound},
		{name: "short base", base: "abc", head: fourth, limit: 50, wantIs: ErrInvalidCommitSHA},
		{name: "limit too small", base: first, head: fourth, limit: 0},
		{name: "limit too large", base: first, head: fourth, limit: maxCompareCommits + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := client.CommitRange(t.Context(), "acme", "blog", tt.base, tt.head, tt.limit)
			wantErr := tt.wantIs != nil || tt.want.Status == ""
			if wantErr {
				if err == nil {
					t.Fatalf("CommitRange = %+v, want an error", got)
				}
				if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
					t.Errorf("error = %v, want %v", err, tt.wantIs)
				}
				if tt.wantStatus != 0 && !HasStatus(err, tt.wantStatus) {
					t.Errorf("error = %v, want status %d", err, tt.wantStatus)
				}
				return
			}
			if err != nil {
				t.Fatalf("CommitRange: %v", err)
			}
			if got.Status != tt.want.Status || got.Total != tt.want.Total || !slices.Equal(got.Commits, tt.want.Commits) {
				t.Errorf("CommitRange = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestClientCommitRangeRejectsBadResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body map[string]any
		want string
	}{
		{name: "unknown status", body: map[string]any{"status": "sideways"}, want: `unexpected status "sideways"`},
		{name: "malformed commit SHA", body: map[string]any{"status": "ahead", "total_commits": 1, "commits": []any{map[string]any{"sha": "nope"}}}, want: "invalid commit SHA"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.srv.Install("acme", "blog", 9)
			f.srv.Handle("GET /repos/acme/blog/compare/{basehead}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("per_page") != "50" {
					githubtest.WriteError(w, http.StatusBadRequest, "per_page")
					return
				}
				githubtest.WriteJSON(w, http.StatusOK, tt.body)
			}))
			sha := strings.Repeat("a", 40)
			_, err := f.repoClient(t, "acme", "blog").CommitRange(t.Context(), "acme", "blog", sha, sha, 50)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("CommitRange error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestClientCommit(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	fake := f.srv.AddRepo("acme", "blog", 9)
	sha := fake.Seed("main", "a.txt", "1")
	client := f.repoClient(t, "acme", "blog")

	got, err := client.Commit(t.Context(), "acme", "blog", sha)
	if err != nil || got != (CommitInfo{SHA: sha, Message: "seed a.txt"}) {
		t.Fatalf("Commit = %+v, %v", got, err)
	}
	if _, err := client.Commit(t.Context(), "acme", "blog", strings.Repeat("e", 40)); !errors.Is(err, ErrCommitNotFound) {
		t.Errorf("Commit of an unknown SHA error = %v, want ErrCommitNotFound", err)
	}
	if _, err := client.Commit(t.Context(), "acme", "blog", "HEAD"); !errors.Is(err, ErrInvalidCommitSHA) {
		t.Errorf("Commit of a ref error = %v, want ErrInvalidCommitSHA", err)
	}
	if _, err := client.Commit(t.Context(), "acme", "../x", sha); !errors.Is(err, ErrInvalidRepository) {
		t.Errorf("Commit of a bad repository error = %v, want ErrInvalidRepository", err)
	}
}

func TestClientCommitRejectsMismatchedSHA(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Install("acme", "blog", 9)
	f.srv.Handle("GET /repos/acme/blog/commits/{sha}", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		githubtest.WriteJSON(w, http.StatusOK, map[string]any{"sha": strings.Repeat("b", 40), "commit": map[string]string{"message": "x"}})
	}))
	if _, err := f.repoClient(t, "acme", "blog").Commit(t.Context(), "acme", "blog", strings.Repeat("a", 40)); err == nil {
		t.Fatal("Commit accepted a response for another commit")
	}
}

func TestClientCommitPullRequests(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	fake := f.srv.AddRepo("acme", "blog", 9)
	fake.Seed("main", "a.txt", "1")
	merged := fake.Commit("main", "add column (#1)", map[string]string{"a.txt": "2"})
	lonely := fake.Commit("main", "direct push", map[string]string{"a.txt": "3"})
	fake.AddPull(githubtest.PullRequest{Title: "add column", Head: "feature", Base: "main", Merged: true, MergeCommitSHA: merged, Labels: []string{"DB-Migration", "backend"}})
	open := fake.AddPull(githubtest.PullRequest{Title: "follow-up", Head: "follow-up", Base: "release", State: "open", MergeCommitSHA: merged})
	fake.LabelPull(open, "db-migration")
	client := f.repoClient(t, "acme", "blog")

	got, err := client.CommitPullRequests(t.Context(), "acme", "blog", merged)
	if err != nil {
		t.Fatalf("CommitPullRequests: %v", err)
	}
	want := []CommitPullRequest{
		{Number: 1, Merged: true, BaseRef: "main", Labels: []string{"DB-Migration", "backend"}},
		{Number: 2, BaseRef: "release", Labels: []string{"db-migration"}},
	}
	if len(got) != len(want) {
		t.Fatalf("CommitPullRequests = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Number != want[i].Number || got[i].Merged != want[i].Merged || got[i].BaseRef != want[i].BaseRef || !slices.Equal(got[i].Labels, want[i].Labels) {
			t.Errorf("pull %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	none, err := client.CommitPullRequests(t.Context(), "acme", "blog", lonely)
	if err != nil || len(none) != 0 {
		t.Errorf("CommitPullRequests of a direct push = %+v, %v", none, err)
	}
	if _, err := client.CommitPullRequests(t.Context(), "acme", "blog", strings.Repeat("e", 40)); !errors.Is(err, ErrCommitNotFound) {
		t.Errorf("CommitPullRequests of an unknown SHA error = %v, want ErrCommitNotFound", err)
	}
	if _, err := client.CommitPullRequests(t.Context(), "acme", "blog", "main"); !errors.Is(err, ErrInvalidCommitSHA) {
		t.Errorf("CommitPullRequests of a ref error = %v, want ErrInvalidCommitSHA", err)
	}
}
