package github

import (
	"errors"
	"net/http"
	"testing"
)

func TestBranchHead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		owner      string
		branch     string
		want       string
		wantIs     error
		wantStatus int
		wantCalls  int64
	}{
		{name: "main", owner: "acme", branch: "main", want: checksSHA, wantCalls: 1},
		{name: "nested branch", owner: "acme", branch: "release/2026.11", want: "ffffffffffffffffffffffffffffffffffffffff", wantCalls: 1},
		{name: "branch needing escapes", owner: "acme", branch: "fix#1 50%", want: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", wantCalls: 1},
		{name: "case-sensitive branch", owner: "acme", branch: "Main", wantIs: ErrBranchNotFound, wantCalls: 1},
		{name: "missing branch", owner: "acme", branch: "gone", wantIs: ErrBranchNotFound, wantStatus: http.StatusNotFound, wantCalls: 1},
		{name: "repository outside the installation", owner: "other", branch: "main", wantIs: ErrBranchNotFound, wantStatus: http.StatusNotFound, wantCalls: 1},
		{name: "empty branch", owner: "acme", branch: "", wantIs: ErrInvalidBranch},
		{name: "empty segment", owner: "acme", branch: "release//1", wantIs: ErrInvalidBranch},
		{name: "leading slash", owner: "acme", branch: "/main", wantIs: ErrInvalidBranch},
		{name: "path traversal", owner: "acme", branch: "../../installation", wantIs: ErrInvalidBranch},
		{name: "invalid owner", owner: "acme/evil", branch: "main", wantIs: ErrInvalidRepository},
		{name: "invalid SHA from GitHub", owner: "acme", branch: "broken", wantIs: ErrInvalidCommitSHA, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.srv.Install("acme", "app", 1)
			f.srv.Install("other", "app", 2)
			f.srv.SetBranch("acme", "app", "main", checksSHA)
			f.srv.SetBranch("acme", "app", "release/2026.11", "ffffffffffffffffffffffffffffffffffffffff")
			f.srv.SetBranch("acme", "app", "fix#1 50%", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
			f.srv.SetBranch("acme", "app", "broken", "HEAD")
			f.srv.SetBranch("other", "app", "main", checksSHA)

			got, err := f.repoClient(t, "acme", "app").BranchHead(t.Context(), tt.owner, "app", tt.branch)
			if calls := f.srv.BranchCalls(); calls != tt.wantCalls {
				t.Errorf("made %d API calls, want %d", calls, tt.wantCalls)
			}
			if tt.wantIs == nil && tt.wantStatus == 0 {
				if err != nil || got != tt.want {
					t.Fatalf("BranchHead = %q, %v, want %q", got, err, tt.want)
				}
				return
			}
			if got != "" {
				t.Errorf("BranchHead = %q on error", got)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error = %v, want %v", err, tt.wantIs)
			}
			if tt.wantStatus != 0 {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != tt.wantStatus {
					t.Errorf("error = %v, want API status %d", err, tt.wantStatus)
				}
			}
		})
	}
}
