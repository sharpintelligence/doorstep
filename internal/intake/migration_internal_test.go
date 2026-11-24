package intake

import (
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
)

func TestHasMigrationTrailer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message string
		want    bool
	}{
		{name: "trailer true", message: "Add column\n\nDoorstep-Migration: true", want: true},
		{name: "trailer yes", message: "Add column\n\nDoorstep-Migration: yes", want: true},
		{name: "case insensitive", message: "x\n\ndoorstep-migration: TRUE", want: true},
		{name: "squash bullet", message: "Feature (#12)\n\n* Add column\n* Doorstep-Migration: true\n", want: true},
		{name: "dash bullet and padding", message: "x\n  - Doorstep-Migration :  yes  ", want: true},
		{name: "crlf line endings", message: "x\r\n\r\nDoorstep-Migration: true\r\n", want: true},
		{name: "trailing period", message: "x\n\nDoorstep-Migration: true.", want: true},
		{name: "anywhere in the message", message: "Doorstep-Migration: true\n\nsubject was the trailer", want: true},
		{name: "false", message: "x\n\nDoorstep-Migration: false"},
		{name: "no value", message: "x\n\nDoorstep-Migration:"},
		{name: "mentioned in prose", message: "set Doorstep-Migration: true when needed"},
		{name: "other trailer", message: "x\n\nCo-Authored-By: someone"},
		{name: "empty", message: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := HasMigrationTrailer(tt.message); got != tt.want {
				t.Errorf("HasMigrationTrailer(%q) = %v, want %v", tt.message, got, tt.want)
			}
		})
	}
}

func TestDetectMigration(t *testing.T) {
	t.Parallel()

	shaA := "aaaaaaa" + strings.Repeat("1", 33)
	shaB := "bbbbbbb" + strings.Repeat("2", 33)
	shaC := "ccccccc" + strings.Repeat("3", 33)
	labels := MigrationPolicy{Labels: []string{"db-migration"}}
	trailer := MigrationPolicy{Trailer: true}
	both := MigrationPolicy{Labels: []string{"db-migration", "schema"}, Trailer: true}
	merged := func(n int, base string, labels ...string) github.CommitPullRequest {
		return github.CommitPullRequest{Number: n, Merged: true, BaseRef: base, Labels: labels}
	}

	tests := []struct {
		name    string
		policy  MigrationPolicy
		commits []github.CommitInfo
		pulls   map[string][]github.CommitPullRequest
		want    MigrationFlag
	}{
		{
			name:    "label on merged pull request",
			policy:  labels,
			commits: []github.CommitInfo{{SHA: shaA}},
			pulls:   map[string][]github.CommitPullRequest{shaA: {merged(12, "main", "backend", "db-migration")}},
			want:    MigrationFlag{Flagged: true, Reason: "label db-migration on #12 (aaaaaaa)"},
		},
		{
			name:    "label matches case-insensitively",
			policy:  labels,
			commits: []github.CommitInfo{{SHA: shaA}},
			pulls:   map[string][]github.CommitPullRequest{shaA: {merged(3, "main", "DB-Migration")}},
			want:    MigrationFlag{Flagged: true, Reason: "label DB-Migration on #3 (aaaaaaa)"},
		},
		{
			name:    "open pull request ignored",
			policy:  labels,
			commits: []github.CommitInfo{{SHA: shaA}},
			pulls:   map[string][]github.CommitPullRequest{shaA: {{Number: 4, BaseRef: "main", Labels: []string{"db-migration"}}}},
		},
		{
			name:    "pull request into another branch ignored",
			policy:  labels,
			commits: []github.CommitInfo{{SHA: shaA}},
			pulls:   map[string][]github.CommitPullRequest{shaA: {merged(5, "release", "db-migration")}},
		},
		{
			name:    "unconfigured label ignored",
			policy:  labels,
			commits: []github.CommitInfo{{SHA: shaA}},
			pulls:   map[string][]github.CommitPullRequest{shaA: {merged(6, "main", "migration")}},
		},
		{
			name:    "trailer",
			policy:  trailer,
			commits: []github.CommitInfo{{SHA: shaA, Message: "x\n\nDoorstep-Migration: yes"}},
			want:    MigrationFlag{Flagged: true, Reason: "trailer Doorstep-Migration in aaaaaaa"},
		},
		{
			name:    "trailer disabled",
			policy:  labels,
			commits: []github.CommitInfo{{SHA: shaA, Message: "x\n\nDoorstep-Migration: yes"}},
		},
		{
			name:   "flagged commit in the middle of the range",
			policy: both,
			commits: []github.CommitInfo{
				{SHA: shaA, Message: "first"},
				{SHA: shaB, Message: "second"},
				{SHA: shaC, Message: "head"},
			},
			pulls: map[string][]github.CommitPullRequest{shaB: {merged(7, "main", "schema")}},
			want:  MigrationFlag{Flagged: true, Reason: "label schema on #7 (bbbbbbb)"},
		},
		{
			name:   "several reasons are listed and counted",
			policy: both,
			commits: []github.CommitInfo{
				{SHA: shaA, Message: "Doorstep-Migration: true"},
				{SHA: shaB, Message: "Doorstep-Migration: true"},
				{SHA: shaC, Message: "Doorstep-Migration: true"},
			},
			pulls: map[string][]github.CommitPullRequest{shaA: {merged(1, "main", "db-migration")}, shaC: {merged(2, "main", "schema")}},
			want: MigrationFlag{Flagged: true, Reason: "label db-migration on #1 (aaaaaaa); trailer Doorstep-Migration in aaaaaaa; " +
				"trailer Doorstep-Migration in bbbbbbb; +2 more"},
		},
		{
			name:    "nothing flagged",
			policy:  both,
			commits: []github.CommitInfo{{SHA: shaA, Message: "plain"}},
		},
		{
			name:    "long labels are truncated",
			policy:  MigrationPolicy{Labels: []string{strings.Repeat("l", 50)}},
			commits: []github.CommitInfo{{SHA: shaA}, {SHA: shaB}, {SHA: shaC}},
			pulls: map[string][]github.CommitPullRequest{
				shaA: {merged(1, "main", strings.Repeat("l", 50))},
				shaB: {merged(2, "main", strings.Repeat("l", 50))},
				shaC: {merged(3, "main", strings.Repeat("l", 50))},
			},
			want: MigrationFlag{Flagged: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := detectMigration(tt.policy, "main", tt.commits, tt.pulls)
			if got.Flagged != tt.want.Flagged {
				t.Fatalf("detectMigration = %+v, want %+v", got, tt.want)
			}
			if tt.want.Reason != "" && got.Reason != tt.want.Reason {
				t.Errorf("reason = %q, want %q", got.Reason, tt.want.Reason)
			}
			if got.Flagged && (got.Reason == "" || len([]rune(got.Reason)) > core.MaxMigrationReasonLength) {
				t.Errorf("reason %q is empty or longer than %d", got.Reason, core.MaxMigrationReasonLength)
			}
			if !got.Flagged && got.Reason != "" {
				t.Errorf("unflagged result has reason %q", got.Reason)
			}
		})
	}
}

func TestTooManyCommits(t *testing.T) {
	t.Parallel()

	got := tooManyCommits(52)
	if !got.Flagged || got.Reason != "52 commits since the last release, too many to inspect" {
		t.Errorf("tooManyCommits(52) = %+v", got)
	}
}

func TestMigrationPolicyValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		policy  MigrationPolicy
		wantErr string
	}{
		{name: "labels", policy: MigrationPolicy{Labels: []string{"db"}}},
		{name: "trailer", policy: MigrationPolicy{Trailer: true}},
		{name: "nothing", policy: MigrationPolicy{}, wantErr: "at least one label or the trailer"},
		{name: "blank label", policy: MigrationPolicy{Labels: []string{" "}}, wantErr: "must not be empty"},
		{name: "too many labels", policy: MigrationPolicy{Labels: make([]string, 11)}, wantErr: "more than 10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.policy.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("validate() = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("validate() = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
