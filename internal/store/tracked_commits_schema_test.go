package store_test

import (
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

const insertRawTrackedCommit = `
INSERT INTO tracked_commits (app_id, repository, branch, commit_sha, source, checks_state, checks)
VALUES ($1, $2, $3, $4, $5, $6, $7)`

type rawTrackedCommit struct {
	repository string
	branch     string
	sha        string
	source     string
	state      string
	checks     string
}

func validRawTrackedCommit() rawTrackedCommit {
	return rawTrackedCommit{repository: "acme/blog", branch: "main", sha: testSHA, source: "webhook", state: "pending", checks: "[]"}
}

func insertRawTracked(t *testing.T, st *store.Store, app string, c rawTrackedCommit) error {
	t.Helper()
	appID, err := st.EnsureApp(t.Context(), core.Target{App: app, Environment: "prod"})
	if err != nil {
		t.Fatalf("EnsureApp: %v", err)
	}
	return store.ExecSQL(t.Context(), st, insertRawTrackedCommit, appID, c.repository, c.branch, c.sha, c.source, c.state, c.checks)
}

func TestSchemaTrackedCommitsAcceptEveryCoreValue(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	for _, s := range core.ChecksStates() {
		c := validRawTrackedCommit()
		c.state = string(s)
		if err := insertRawTracked(t, st, "state-"+string(s), c); err != nil {
			t.Errorf("checks state %s rejected: %v", s, err)
		}
	}
	for _, s := range core.Sources() {
		c := validRawTrackedCommit()
		c.source = string(s)
		if err := insertRawTracked(t, st, "source-"+string(s), c); err != nil {
			t.Errorf("source %s rejected: %v", s, err)
		}
	}
}

func TestSchemaTrackedCommitsRejectInvalidRows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mutate     func(*rawTrackedCommit)
		constraint string
	}{
		{name: "blank repository", mutate: func(c *rawTrackedCommit) { c.repository = " " }, constraint: "tracked_commits_repository_check"},
		{name: "blank branch", mutate: func(c *rawTrackedCommit) { c.branch = "" }, constraint: "tracked_commits_branch_check"},
		{name: "short SHA", mutate: func(c *rawTrackedCommit) { c.sha = testSHA[:7] }, constraint: "tracked_commits_commit_sha_check"},
		{name: "uppercase SHA", mutate: func(c *rawTrackedCommit) { c.sha = strings.ToUpper(testSHA) }, constraint: "tracked_commits_commit_sha_check"},
		{name: "unknown source", mutate: func(c *rawTrackedCommit) { c.source = "cron" }, constraint: "tracked_commits_source_check"},
		{name: "unknown checks state", mutate: func(c *rawTrackedCommit) { c.state = "neutral" }, constraint: "tracked_commits_checks_state_check"},
		{name: "checks object", mutate: func(c *rawTrackedCommit) { c.checks = "{}" }, constraint: "tracked_commits_checks_check"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			c := validRawTrackedCommit()
			tt.mutate(&c)
			assertConstraint(t, insertRawTracked(t, st, "blog", c), tt.constraint)
		})
	}
}

func TestSchemaTrackedCommitsUniqueness(t *testing.T) {
	t.Parallel()

	t.Run("one row per app and SHA", func(t *testing.T) {
		t.Parallel()
		st := storetest.Open(t)
		if err := insertRawTracked(t, st, "blog", validRawTrackedCommit()); err != nil {
			t.Fatalf("insert: %v", err)
		}
		assertConstraint(t, insertRawTracked(t, st, "blog", validRawTrackedCommit()), "tracked_commits_app_id_commit_sha_key")
		if err := insertRawTracked(t, st, "blog-staging", validRawTrackedCommit()); err != nil {
			t.Errorf("same SHA for another app rejected: %v", err)
		}
	})

	t.Run("one tracked commit per deployment", func(t *testing.T) {
		t.Parallel()
		st := storetest.Open(t)
		d := mustCreate(t, st, newDeployment("blog"), 0)
		first := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
		second := mustTrack(t, st, newTrackedCommit("other", shaOf('b')))
		mustExec(t, st, "UPDATE tracked_commits SET deployment_id = $1 WHERE id = $2", d.ID, first.ID)
		err := store.ExecSQL(t.Context(), st, "UPDATE tracked_commits SET deployment_id = $1 WHERE id = $2", d.ID, second.ID)
		assertConstraint(t, err, "tracked_commits_deployment_id_key")
	})
}
