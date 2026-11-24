package intake_test

import (
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/intake"
)

func TestPostgresRefusesBlockedCommit(t *testing.T) {
	t.Parallel()

	st, db := openStore(t)
	checks := newFakeChecks()
	checks.set("example-user/blog", shaA, green("test"))
	branches := newFakeBranches()
	branches.set("example-user/blog", "main", shaA)
	app := blogApp("prod")
	svc, err := intake.New(intake.Options{
		Apps:     []intake.App{app},
		Commits:  intake.NewPostgresCommits(st, queueOver),
		Checks:   checks,
		Branches: branches,
		Now:      newTickingClock().Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	block, _, err := st.CreateBlock(t.Context(), core.Block{
		Target:    app.Target,
		Commit:    core.Commit{Repository: app.Repository, SHA: shaA},
		Reason:    "rolled back",
		CreatedBy: "user:alice",
	})
	if err != nil {
		t.Fatalf("CreateBlock: %v", err)
	}

	results, err := svc.HandlePush(t.Context(), push(shaA))
	if err != nil {
		t.Fatalf("HandlePush: %v", err)
	}
	if len(results) != 1 || results[0].Enqueued || results[0].Blocked == nil || results[0].Blocked.ID != block.ID {
		t.Fatalf("HandlePush = %+v, want the commit refused by block %d", results, block.ID)
	}
	if got := results[0].Commit; !got.Open() || got.ChecksState != core.ChecksSuccess {
		t.Fatalf("tracked commit = %+v, want it open and green", got)
	}
	if n := countRows(t, db, "SELECT count(*) FROM deployments"); n != 0 {
		t.Fatalf("%d deployments after a blocked settle, want 0", n)
	}

	if _, err := st.ClearBlocks(t.Context(), app.Target, core.BlockRef{CommitSHA: shaA}, "user:bob"); err != nil {
		t.Fatalf("ClearBlocks: %v", err)
	}
	results, err = svc.Reevaluate(t.Context(), "example-user/blog", shaA)
	if err != nil {
		t.Fatalf("Reevaluate: %v", err)
	}
	if len(results) != 1 || !results[0].Enqueued || results[0].Blocked != nil {
		t.Fatalf("Reevaluate after unblock = %+v, want it enqueued", results)
	}
	if n := countRows(t, db, "SELECT count(*) FROM deployments"); n != 1 {
		t.Fatalf("%d deployments after unblock, want 1", n)
	}
}
