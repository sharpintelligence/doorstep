package github

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
)

func TestCreateCommitStatus(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("é", 200)
	tests := []struct {
		name    string
		owner   string
		repo    string
		sha     string
		req     CommitStatusRequest
		reply   *githubtest.StatusReply
		want    githubtest.Status
		wantErr func(t *testing.T, err error)
	}{
		{
			name: "full request",
			req: CommitStatusRequest{
				State: CommitStatusPending, Context: "doorstep/blog/after/health",
				Description: "Argo CD is Progressing", TargetURL: "https://doorstep.example.com/deployments/42",
			},
			want: githubtest.Status{
				Context: "doorstep/blog/after/health", State: "pending",
				Description: "Argo CD is Progressing", TargetURL: "https://doorstep.example.com/deployments/42",
			},
		},
		{
			name: "minimal request",
			req:  CommitStatusRequest{State: CommitStatusSuccess, Context: "doorstep/blog/before/checks"},
			want: githubtest.Status{Context: "doorstep/blog/before/checks", State: "success"},
		},
		{
			name: "description truncated to 140 runes",
			req:  CommitStatusRequest{State: CommitStatusFailure, Context: "c", Description: long},
			want: githubtest.Status{Context: "c", State: "failure", Description: strings.Repeat("é", 139) + "…"},
		},
		{
			name:  "forbidden",
			req:   CommitStatusRequest{State: CommitStatusError, Context: "c"},
			reply: &githubtest.StatusReply{Code: http.StatusForbidden, Message: "Resource not accessible by integration"},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				assertAPIStatus(t, err, http.StatusForbidden)
			},
		},
		{
			name: "invalid sha",
			sha:  "abc",
			req:  CommitStatusRequest{State: CommitStatusSuccess, Context: "c"},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrInvalidCommitSHA) {
					t.Errorf("error = %v, want ErrInvalidCommitSHA", err)
				}
			},
		},
		{
			name: "unknown state",
			req:  CommitStatusRequest{State: "skipped", Context: "c"},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				if err == nil || !strings.Contains(err.Error(), `unknown state "skipped"`) {
					t.Errorf("error = %v", err)
				}
			},
		},
		{
			name: "empty context",
			req:  CommitStatusRequest{State: CommitStatusSuccess, Context: " "},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				if err == nil || !strings.Contains(err.Error(), "context must not be empty") {
					t.Errorf("error = %v", err)
				}
			},
		},
		{
			name:  "invalid repository",
			owner: "-bad",
			req:   CommitStatusRequest{State: CommitStatusSuccess, Context: "c"},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrInvalidRepository) {
					t.Errorf("error = %v, want ErrInvalidRepository", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.srv.Install("acme", "app", 7)
			f.srv.OnCreateStatus(func(*http.Request, githubtest.Status) *githubtest.StatusReply { return tt.reply })
			client := f.repoClient(t, "acme", "app")
			owner, repo, sha := "acme", "app", testSHA
			if tt.owner != "" {
				owner = tt.owner
			}
			if tt.repo != "" {
				repo = tt.repo
			}
			if tt.sha != "" {
				sha = tt.sha
			}

			err := client.CreateCommitStatus(t.Context(), owner, repo, sha, tt.req)
			posted := f.srv.PostedStatuses("acme", "app", testSHA)
			if tt.wantErr != nil {
				tt.wantErr(t, err)
				if len(posted) != 0 {
					t.Errorf("posted %+v despite the error", posted)
				}
				if tt.reply == nil && f.srv.StatusCalls() != 0 {
					t.Errorf("invalid request reached GitHub %d times", f.srv.StatusCalls())
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateCommitStatus: %v", err)
			}
			if len(posted) != 1 || posted[0] != tt.want {
				t.Fatalf("posted = %+v, want %+v", posted, tt.want)
			}
			if n := utf8.RuneCountInString(posted[0].Description); n > maxDescriptionRunes {
				t.Errorf("description has %d runes", n)
			}
		})
	}
}

func TestCreateCommitStatusUpdatesCombinedStatus(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Install("acme", "app", 7)
	client := f.repoClient(t, "acme", "app")
	for _, state := range []CommitStatusState{CommitStatusPending, CommitStatusSuccess} {
		if err := client.CreateCommitStatus(t.Context(), "acme", "app", testSHA, CommitStatusRequest{State: state, Context: "doorstep/app/after/health"}); err != nil {
			t.Fatalf("CreateCommitStatus(%s): %v", state, err)
		}
	}
	statuses, err := client.CombinedStatus(t.Context(), "acme", "app", testSHA)
	if err != nil {
		t.Fatalf("CombinedStatus: %v", err)
	}
	if len(statuses) != 1 || statuses[0].State != "success" || statuses[0].Context != "doorstep/app/after/health" {
		t.Errorf("combined statuses = %+v, want the latest per context", statuses)
	}
}
