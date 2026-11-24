package github

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
)

func TestRepoCompare(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	fake := f.srv.AddRepo("acme", "gitops", 9)
	first := fake.Seed("main", "a.yaml", "a: 1\n")
	fake.CreateBranch("side", "main")
	second := fake.Commit("main", "second", map[string]string{"a.yaml": "a: 2\n"})
	third := fake.Commit("main", "third", map[string]string{"a.yaml": "a: 3\n"})
	side := fake.Commit("side", "side", map[string]string{"b.yaml": "b: 1\n"})
	unknown := strings.Repeat("f", 40)

	var mu sync.Mutex
	paths := map[string]bool{}
	fake.OnRequest(func(method, path string) {
		if method == http.MethodGet && strings.Contains(path, "/compare/") {
			mu.Lock()
			paths[path] = true
			mu.Unlock()
		}
	})
	repo := f.repo(t)

	tests := []struct {
		name       string
		base, head string
		want       Comparison
		contains   bool
		wantStatus int
		wantIs     error
	}{
		{name: "identical", base: second, head: second, want: ComparisonIdentical, contains: true},
		{name: "head is ahead", base: first, head: third, want: ComparisonAhead, contains: true},
		{name: "head is behind", base: third, head: second, want: ComparisonBehind},
		{name: "diverged", base: third, head: side, want: ComparisonDiverged},
		{name: "unknown commit", base: first, head: unknown, wantStatus: http.StatusNotFound},
		{name: "short base", base: "abc123", head: third, wantIs: ErrInvalidCommitSHA},
		{name: "uppercase head", base: first, head: strings.ToUpper(third), wantIs: ErrInvalidCommitSHA},
		{name: "path in head", base: first, head: "../../installation", wantIs: ErrInvalidCommitSHA},
	}
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if !paths["/repos/acme/gitops/compare/"+second+"..."+second] {
			t.Errorf("compare paths = %v", paths)
		}
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := repo.Compare(t.Context(), tt.base, tt.head)
			switch {
			case tt.wantIs != nil:
				if !errors.Is(err, tt.wantIs) {
					t.Fatalf("Compare error = %v, want %v", err, tt.wantIs)
				}
			case tt.wantStatus != 0:
				if !HasStatus(err, tt.wantStatus) {
					t.Fatalf("Compare error = %v, want status %d", err, tt.wantStatus)
				}
			default:
				if err != nil || got != tt.want {
					t.Fatalf("Compare = %q, %v, want %q", got, err, tt.want)
				}
				if got.HeadContainsBase() != tt.contains {
					t.Fatalf("HeadContainsBase() = %v, want %v", got.HeadContainsBase(), tt.contains)
				}
			}
		})
	}
}

func TestRepoCompareUnexpectedStatus(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Install("acme", "gitops", 9)
	f.srv.Handle("GET /repos/acme/gitops/compare/{basehead}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") != "1" {
			githubtest.WriteError(w, http.StatusBadRequest, "per_page")
			return
		}
		githubtest.WriteJSON(w, http.StatusOK, map[string]string{"status": "sideways"})
	}))
	sha := strings.Repeat("a", 40)
	if _, err := f.repo(t).Compare(t.Context(), sha, sha); err == nil || !strings.Contains(err.Error(), `unexpected status "sideways"`) {
		t.Fatalf("Compare error = %v", err)
	}
}
