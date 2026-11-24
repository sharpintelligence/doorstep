package intake_test

import (
	"errors"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
)

func TestBlockedCommitsAreNotEnqueued(t *testing.T) {
	t.Parallel()

	app := blogApp("prod")
	block := core.Block{ID: 9, Target: app.Target, Commit: core.Commit{Repository: app.Repository, SHA: shaA}, Reason: "bad", CreatedBy: "user:alice"}
	tests := []struct {
		name         string
		setup        func(h *harness)
		wantEnqueued bool
		wantBlocked  bool
		wantErr      error
	}{
		{name: "not blocked", setup: func(*harness) {}, wantEnqueued: true},
		{name: "blocked", setup: func(h *harness) { h.commits.blocks.block(block) }, wantBlocked: true},
		{
			name: "blocked in another environment",
			setup: func(h *harness) {
				other := block
				other.Target.Environment = "staging"
				h.commits.blocks.block(other)
			},
			wantEnqueued: true,
		},
		{name: "block lookup fails", setup: func(h *harness) { h.commits.blocks.fail(errBoom) }, wantErr: errBoom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, app)
			h.checks.set("example-user/blog", shaA, green("test"))
			tt.setup(h)
			h.branches.set("example-user/blog", "main", shaA)

			results, err := h.svc.HandlePush(t.Context(), push(shaA))
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil) != (err == nil) {
				t.Fatalf("HandlePush error = %v, want %v", err, tt.wantErr)
			}
			if len(results) != 1 {
				t.Fatalf("HandlePush = %+v, want one result", results)
			}
			r := results[0]
			if r.Enqueued != tt.wantEnqueued || (r.Blocked != nil) != tt.wantBlocked {
				t.Fatalf("result = %+v, want enqueued %v blocked %v", r, tt.wantEnqueued, tt.wantBlocked)
			}
			if tt.wantBlocked && r.Blocked.ID != block.ID {
				t.Errorf("Blocked = %+v, want block %d", r.Blocked, block.ID)
			}
			if got := len(h.commits.queue.all()); got != boolCount(tt.wantEnqueued) {
				t.Errorf("enqueued %d deployments, want %d", got, boolCount(tt.wantEnqueued))
			}
			if h.commits.blocks.callCount() != 1 {
				t.Errorf("checked blocks %d times, want 1", h.commits.blocks.callCount())
			}
			row := h.commits.get(1)
			wantSaved := tt.wantErr == nil
			if (row.EvaluatedAt != nil) != wantSaved || row.Open() == tt.wantEnqueued {
				t.Errorf("tracked commit = %+v, want evaluation saved %v and open %v", row, wantSaved, !tt.wantEnqueued)
			}
		})
	}
}

func TestBlockedCommitEnqueuesOnceUnblocked(t *testing.T) {
	t.Parallel()

	app := blogApp("prod")
	h := newHarness(t, app)
	h.checks.set("example-user/blog", shaA, green("test"))
	h.commits.blocks.block(core.Block{Target: app.Target, Commit: core.Commit{SHA: shaA}, CreatedBy: "user:alice"})

	if r := h.push(t, push(shaA)); len(r) != 1 || r[0].Enqueued || r[0].Blocked == nil {
		t.Fatalf("push of a blocked commit = %+v", r)
	}
	if r := h.poll(t); len(r) != 1 || r[0].Enqueued || r[0].Blocked == nil {
		t.Fatalf("poll while blocked = %+v", r)
	}

	h.commits.blocks.unblock(app.Target, shaA)
	r := h.poll(t)
	if len(r) != 1 || !r[0].Enqueued || r[0].Blocked != nil {
		t.Fatalf("poll after unblock = %+v, want it enqueued", r)
	}
	if got := h.commits.queue.all(); len(got) != 1 || got[0].Commit.SHA != shaA {
		t.Fatalf("queue = %+v, want one deploy of %s", got, shaA)
	}
	if r := h.poll(t); len(r) != 0 {
		t.Fatalf("poll after enqueue = %+v, want nothing open", r)
	}
}

func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}
