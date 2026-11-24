package intake_test

import (
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
	"github.com/sharpintelligence/doorstep/internal/intake"
)

func TestGitHubMigrationSource(t *testing.T) {
	t.Parallel()

	srv, gh := newGitHub(t)
	repo := srv.AddRepo("example-user", "blog", 7)
	base := repo.Seed("main", "a.txt", "1")
	migration := repo.Commit("main", "add column (#1)", map[string]string{"a.txt": "2"})
	head := repo.Commit("main", "tidy\n\nDoorstep-Migration: yes", map[string]string{"a.txt": "3"})
	repo.AddPull(githubtest.PullRequest{Title: "add column", Head: "column", Merged: true, MergeCommitSHA: migration, Labels: []string{"db-migration"}})

	commits, err := gh.Range(t.Context(), "Example-User/blog", base, head, intake.MaxMigrationRange)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if commits.Status != github.ComparisonAhead || commits.Total != 2 || len(commits.Commits) != 2 || commits.Commits[0].SHA != migration || commits.Commits[1].SHA != head {
		t.Errorf("Range = %+v", commits)
	}
	c, err := gh.Commit(t.Context(), "example-user/blog", head)
	if err != nil || !intake.HasMigrationTrailer(c.Message) {
		t.Errorf("Commit = %+v, %v", c, err)
	}
	pulls, err := gh.PullRequests(t.Context(), "example-user/blog", migration)
	if err != nil || len(pulls) != 1 || !pulls[0].Merged || pulls[0].BaseRef != "main" || pulls[0].Labels[0] != "db-migration" {
		t.Errorf("PullRequests = %+v, %v", pulls, err)
	}

	tests := []struct {
		name string
		call func() error
		is   error
	}{
		{name: "range not installed", call: func() error {
			_, err := gh.Range(t.Context(), "example-user/private", base, head, 50)
			return err
		}, is: github.ErrNotInstalled},
		{name: "range unknown base", call: func() error {
			_, err := gh.Range(t.Context(), "example-user/blog", strings.Repeat("e", 40), head, 50)
			return err
		}, is: github.ErrCommitNotFound},
		{name: "commit not owner/name", call: func() error {
			_, err := gh.Commit(t.Context(), "blog", head)
			return err
		}},
		{name: "pulls not installed", call: func() error {
			_, err := gh.PullRequests(t.Context(), "example-user/private", head)
			return err
		}, is: github.ErrNotInstalled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertGitHubError(t, tt.call(), tt.is, 0)
		})
	}
}

func TestGitHubMigrationDetectionEndToEnd(t *testing.T) {
	t.Parallel()

	srv, gh := newGitHub(t)
	repo := srv.AddRepo("example-user", "blog", 7)
	base := repo.Seed("main", "a.txt", "1")
	migration := repo.Commit("main", "add column (#1)", map[string]string{"a.txt": "2"})
	head := repo.Commit("main", "follow-up (#2)", map[string]string{"a.txt": "3"})
	repo.AddPull(githubtest.PullRequest{Title: "add column", Head: "column", Merged: true, MergeCommitSHA: migration, Labels: []string{"DB-Migration"}})
	repo.AddPull(githubtest.PullRequest{Title: "follow-up", Head: "follow-up", Merged: true, MergeCommitSHA: head})

	app := migratingApp("prod", intake.MigrationPolicy{Labels: []string{"db-migration"}})
	app.Repository = "example-user/blog"
	commits := newFakeCommits()
	checks := newFakeChecks()
	checks.set("example-user/blog", head, green("test"))
	branches := newFakeBranches()
	branches.set("example-user/blog", "main", head)
	baselines := &fakeBaselines{commits: map[core.Target]core.Commit{app.Target: {Repository: "example-user/blog", SHA: base}}}
	svc, err := intake.New(intake.Options{Apps: []intake.App{app}, Commits: commits, Checks: checks, Branches: branches, Migrations: gh, Baselines: baselines})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	results, err := svc.HandlePush(t.Context(), intake.Push{Repository: "example-user/blog", Branch: "main", SHA: head, Source: core.SourceWebhook, PushedBy: "octocat"})
	if err != nil || len(results) != 1 || !results[0].Enqueued {
		t.Fatalf("HandlePush = %+v, %v", results, err)
	}
	d := commits.queue.all()[0]
	if !d.Migration || d.MigrationReason != "label DB-Migration on #1 ("+migration[:7]+")" {
		t.Errorf("deployment migration = %v %q", d.Migration, d.MigrationReason)
	}
}
