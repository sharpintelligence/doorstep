package store_test

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

const (
	thirdDigest  = core.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	pinSHA       = "1111111111111111111111111111111111111111"
	secondPinSHA = "2222222222222222222222222222222222222222"
)

var blogProd = core.Target{App: "blog", Environment: "prod"}

func newBlock(digest core.Digest, sha string) core.Block {
	b := core.Block{Target: blogProd, Digest: digest, Reason: "broke checkout", CreatedBy: "user:alice"}
	if sha != "" {
		b.Commit = core.Commit{Repository: "acme/blog", SHA: sha}
	}
	return b
}

func mustBlock(t *testing.T, st *store.Store, b core.Block) core.Block {
	t.Helper()
	got, _, err := st.CreateBlock(t.Context(), b)
	if err != nil {
		t.Fatalf("CreateBlock(%s): %v", b.Subject(), err)
	}
	return got
}

func blockIDs(blocks []core.Block) []core.BlockID {
	out := make([]core.BlockID, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.ID)
	}
	return out
}

func TestCreateBlock(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("blog"), 0)

	b := newBlock(testDigest, testSHA)
	b.DeploymentID = &d.ID
	created, isNew, err := st.CreateBlock(t.Context(), b)
	if err != nil || !isNew {
		t.Fatalf("CreateBlock = %v, %v, want a new block", isNew, err)
	}
	if created.ID == 0 || created.Target != blogProd || created.Digest != testDigest || created.Commit != b.Commit ||
		created.Reason != b.Reason || created.CreatedBy != b.CreatedBy || created.DeploymentID == nil || *created.DeploymentID != d.ID ||
		created.RollbackID != nil || created.CreatedAt.IsZero() || !created.Active() || created.BlockCommitSHA != "" {
		t.Fatalf("CreateBlock = %+v", created)
	}

	rb := newDeployment("blog")
	rb.Kind = core.KindRollback
	rb.RollbackOfID = &d.ID
	rollback := mustCreate(t, st, rb, 200)
	again := newBlock(testDigest, "")
	again.Reason = "second reason"
	again.RollbackID = &rollback.ID
	existing, isNew, err := st.CreateBlock(t.Context(), again)
	if err != nil || isNew {
		t.Fatalf("CreateBlock(again) = %v, %v, want the existing block", isNew, err)
	}
	if existing.ID != created.ID || existing.Reason != "broke checkout" || existing.Commit.SHA != testSHA ||
		existing.RollbackID == nil || *existing.RollbackID != rollback.ID {
		t.Fatalf("CreateBlock(again) = %+v, want block %d with the rollback filled in", existing, created.ID)
	}
}

func TestCreateBlockMergesCommitAndDigest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		first      core.Block
		second     core.Block
		wantNew    bool
		wantDigest core.Digest
		wantSHA    string
		wantRepo   string
	}{
		{
			name:       "digest added to a commit-only block",
			first:      newBlock("", testSHA),
			second:     newBlock(testDigest, testSHA),
			wantDigest: testDigest,
			wantSHA:    testSHA,
			wantRepo:   "acme/blog",
		},
		{
			name:       "commit added to a digest-only block",
			first:      newBlock(testDigest, ""),
			second:     newBlock(testDigest, testSHA),
			wantDigest: testDigest,
			wantSHA:    testSHA,
			wantRepo:   "acme/blog",
		},
		{
			name:       "commit-only request finds a digest block of that commit",
			first:      newBlock(testDigest, testSHA),
			second:     newBlock("", testSHA),
			wantDigest: testDigest,
			wantSHA:    testSHA,
			wantRepo:   "acme/blog",
		},
		{
			name:       "another digest of the same commit is a new block",
			first:      newBlock(testDigest, testSHA),
			second:     newBlock(otherDigest, testSHA),
			wantNew:    true,
			wantDigest: otherDigest,
			wantSHA:    testSHA,
			wantRepo:   "acme/blog",
		},
		{
			name:     "digest blocked with another commit gives the commit its own block",
			first:    newBlock(testDigest, testSHA),
			second:   newBlock(testDigest, otherSHA),
			wantNew:  true,
			wantSHA:  otherSHA,
			wantRepo: "acme/blog",
		},
		{
			name:     "another commit is a new block",
			first:    newBlock("", testSHA),
			second:   newBlock("", otherSHA),
			wantNew:  true,
			wantSHA:  otherSHA,
			wantRepo: "acme/blog",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			first := mustBlock(t, st, tt.first)
			got, isNew, err := st.CreateBlock(t.Context(), tt.second)
			if err != nil {
				t.Fatalf("CreateBlock: %v", err)
			}
			if isNew != tt.wantNew || (got.ID == first.ID) == tt.wantNew {
				t.Errorf("CreateBlock = block %d new %v, first was %d, want new %v", got.ID, isNew, first.ID, tt.wantNew)
			}
			if got.Digest != tt.wantDigest || got.Commit.SHA != tt.wantSHA || got.Commit.Repository != tt.wantRepo {
				t.Errorf("CreateBlock = %+v, want digest %q commit %s@%s", got, tt.wantDigest, tt.wantRepo, tt.wantSHA)
			}
		})
	}
}

func TestCreateBlockKeepsEveryCommitOfABlockedDigest(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	first := mustBlock(t, st, newBlock(testDigest, testSHA))
	second := mustBlock(t, st, newBlock(testDigest, otherSHA))
	again, isNew, err := st.CreateBlock(t.Context(), newBlock(testDigest, otherSHA))
	if err != nil || isNew || again.ID != second.ID {
		t.Fatalf("CreateBlock(again) = block %d new %v, %v, want block %d", again.ID, isNew, err, second.ID)
	}

	blocks, err := st.ListBlocks(t.Context(), blogProd, false)
	if err != nil || len(blocks) != 2 || blocks[1].ID != first.ID || blocks[1].Digest != testDigest || blocks[1].Commit.SHA != testSHA ||
		blocks[0].Digest != "" || blocks[0].Commit.SHA != otherSHA {
		t.Errorf("blocks = %+v, %v, want the first block unchanged and a commit-only block of %s", blocks, err, otherSHA)
	}
	for _, sha := range []string{testSHA, otherSHA} {
		if _, found, err := st.Blocked(t.Context(), blogProd, core.Commit{Repository: "acme/blog", SHA: sha}); err != nil || !found {
			t.Errorf("Blocked(%s) = %v, %v, want blocked", sha, found, err)
		}
	}
	if got, found, err := st.ActiveBlock(t.Context(), blogProd, core.BlockRef{Digest: testDigest}); err != nil || !found || got.ID != first.ID {
		t.Errorf("ActiveBlock(digest) = %+v, %v, %v, want block %d", got, found, err, first.ID)
	}
}

func TestCreateBlockRejectsInvalidBlocks(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	tests := []struct {
		name  string
		block core.Block
	}{
		{name: "no subject", block: newBlock("", "")},
		{name: "bad digest", block: newBlock("sha256:abc", "")},
		{name: "no creator", block: func() core.Block { b := newBlock(testDigest, ""); b.CreatedBy = ""; return b }()},
		{name: "reason with newline", block: func() core.Block { b := newBlock(testDigest, ""); b.Reason = "a\nb"; return b }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := st.CreateBlock(t.Context(), tt.block); !errors.Is(err, core.ErrInvalidBlock) {
				t.Fatalf("CreateBlock error = %v, want ErrInvalidBlock", err)
			}
		})
	}
	if n := queryInt(t, st, "SELECT count(*) FROM blocks"); n != 0 {
		t.Fatalf("%d blocks stored, want 0", n)
	}
}

func TestActiveBlockAndBlocked(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	withCommit := mustBlock(t, st, newBlock(testDigest, testSHA))
	commitOnly := mustBlock(t, st, newBlock("", otherSHA))
	mustBlock(t, st, newBlock(thirdDigest, ""))
	if _, err := st.ClearBlocks(t.Context(), blogProd, core.BlockRef{Digest: thirdDigest}, "user:bob"); err != nil {
		t.Fatalf("ClearBlocks: %v", err)
	}
	staging := newBlock(otherDigest, "")
	staging.Target.Environment = "staging"
	mustBlock(t, st, staging)

	tests := []struct {
		name   string
		target core.Target
		ref    core.BlockRef
		want   core.BlockID
	}{
		{name: "by digest", target: blogProd, ref: core.BlockRef{Digest: testDigest}, want: withCommit.ID},
		{name: "by commit of a digest block", target: blogProd, ref: core.BlockRef{CommitSHA: testSHA}, want: withCommit.ID},
		{name: "by commit-only block", target: blogProd, ref: core.BlockRef{CommitSHA: otherSHA}, want: commitOnly.ID},
		{name: "unblocked digest, blocked commit", target: blogProd, ref: core.BlockRef{Digest: otherDigest, CommitSHA: otherSHA}, want: commitOnly.ID},
		{name: "digest match preferred", target: blogProd, ref: core.BlockRef{Digest: testDigest, CommitSHA: otherSHA}, want: withCommit.ID},
		{name: "cleared block", target: blogProd, ref: core.BlockRef{Digest: thirdDigest}},
		{name: "other environment", target: blogProd, ref: core.BlockRef{Digest: otherDigest}},
		{name: "unknown app", target: core.Target{App: "nope", Environment: "prod"}, ref: core.BlockRef{Digest: testDigest}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, found, err := st.ActiveBlock(t.Context(), tt.target, tt.ref)
			if err != nil {
				t.Fatalf("ActiveBlock: %v", err)
			}
			if found != (tt.want != 0) || got.ID != tt.want {
				t.Fatalf("ActiveBlock = %d, %v, want %d", got.ID, found, tt.want)
			}
			if tt.ref.Digest != "" {
				return
			}
			blocked, found, err := st.Blocked(t.Context(), tt.target, core.Commit{Repository: "acme/blog", SHA: tt.ref.CommitSHA})
			if err != nil || found != (tt.want != 0) || blocked.ID != tt.want {
				t.Fatalf("Blocked = %d, %v, %v, want %d", blocked.ID, found, err, tt.want)
			}
		})
	}

	if _, _, err := st.ActiveBlock(t.Context(), blogProd, core.BlockRef{}); !errors.Is(err, core.ErrInvalidBlock) {
		t.Errorf("ActiveBlock(empty ref) error = %v, want ErrInvalidBlock", err)
	}
}

func TestClearBlocks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		ref       core.BlockRef
		wantCount int
		wantErr   error
	}{
		{name: "by digest", ref: core.BlockRef{Digest: testDigest}, wantCount: 1},
		{name: "by commit clears every block of the commit", ref: core.BlockRef{CommitSHA: testSHA}, wantCount: 2},
		{name: "not blocked", ref: core.BlockRef{Digest: thirdDigest}, wantErr: core.ErrNotFound},
		{name: "invalid ref", ref: core.BlockRef{CommitSHA: "main"}, wantErr: core.ErrInvalidBlock},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			mustBlock(t, st, newBlock(testDigest, testSHA))
			mustBlock(t, st, newBlock(otherDigest, testSHA))

			cleared, err := st.ClearBlocks(t.Context(), blogProd, tt.ref, "user:bob")
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ClearBlocks error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil || len(cleared) != tt.wantCount {
				t.Fatalf("ClearBlocks = %d blocks, %v, want %d", len(cleared), err, tt.wantCount)
			}
			for _, b := range cleared {
				if b.Active() || b.ClearedBy != "user:bob" {
					t.Errorf("cleared block = %+v", b)
				}
			}
			if _, err := st.ClearBlocks(t.Context(), blogProd, tt.ref, "user:bob"); !errors.Is(err, core.ErrNotFound) {
				t.Errorf("second ClearBlocks error = %v, want ErrNotFound", err)
			}
		})
	}

	st := storetest.Open(t)
	if _, err := st.ClearBlocks(t.Context(), blogProd, core.BlockRef{Digest: testDigest}, " "); err == nil {
		t.Error("ClearBlocks without an actor succeeded")
	}
}

func TestListBlocks(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	first := mustBlock(t, st, newBlock(testDigest, ""))
	second := mustBlock(t, st, newBlock(otherDigest, ""))
	third := mustBlock(t, st, newBlock("", testSHA))
	if _, err := st.ClearBlocks(t.Context(), blogProd, core.BlockRef{Digest: otherDigest}, "user:bob"); err != nil {
		t.Fatalf("ClearBlocks: %v", err)
	}
	other := newBlock(testDigest, "")
	other.Target.App = "web"
	mustBlock(t, st, other)

	tests := []struct {
		name           string
		includeCleared bool
		want           []core.BlockID
	}{
		{name: "active", want: []core.BlockID{third.ID, first.ID}},
		{name: "including cleared", includeCleared: true, want: []core.BlockID{third.ID, second.ID, first.ID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := st.ListBlocks(t.Context(), blogProd, tt.includeCleared)
			if err != nil {
				t.Fatalf("ListBlocks: %v", err)
			}
			if !slices.Equal(blockIDs(got), tt.want) {
				t.Fatalf("ListBlocks = %v, want %v", blockIDs(got), tt.want)
			}
		})
	}
	if got, err := st.ListBlocks(t.Context(), core.Target{App: "nope", Environment: "prod"}, true); err != nil || len(got) != 0 {
		t.Errorf("ListBlocks(unknown) = %v, %v, want none", got, err)
	}
	if _, err := st.ListBlocks(t.Context(), core.Target{}, false); err == nil {
		t.Error("ListBlocks(invalid target) succeeded")
	}
}

func TestPendingBlockTrailersAndMarkMirrored(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	pending := func(t *testing.T) core.BlockTrailers {
		t.Helper()
		got, err := st.PendingBlockTrailers(t.Context(), blogProd)
		if err != nil {
			t.Fatalf("PendingBlockTrailers: %v", err)
		}
		return got
	}
	mark := func(t *testing.T, trailers core.BlockTrailers, sha string) {
		t.Helper()
		if err := st.MarkBlocksMirrored(t.Context(), blogProd, trailers, core.Commit{Repository: "acme/gitops", SHA: sha}); err != nil {
			t.Fatalf("MarkBlocksMirrored: %v", err)
		}
	}
	clear := func(t *testing.T, d core.Digest) {
		t.Helper()
		if _, err := st.ClearBlocks(t.Context(), blogProd, core.BlockRef{Digest: d}, "user:bob"); err != nil {
			t.Fatalf("ClearBlocks(%s): %v", d, err)
		}
	}
	assertPending := func(t *testing.T, wantBlocks, wantUnblocks []core.Digest) {
		t.Helper()
		got := pending(t)
		if !slices.Equal(got.Blocks, wantBlocks) || !slices.Equal(got.Unblocks, wantUnblocks) {
			t.Fatalf("pending = %+v, want blocks %v unblocks %v", got, wantBlocks, wantUnblocks)
		}
	}

	if got := pending(t); !got.IsZero() {
		t.Fatalf("pending with no blocks = %+v", got)
	}
	mustBlock(t, st, newBlock(testDigest, testSHA))
	mustBlock(t, st, newBlock("", otherSHA))
	mustBlock(t, st, newBlock(otherDigest, ""))
	assertPending(t, []core.Digest{testDigest, otherDigest}, nil)

	mark(t, core.BlockTrailers{Blocks: []core.Digest{testDigest}}, pinSHA)
	assertPending(t, []core.Digest{otherDigest}, nil)
	if n := queryInt(t, st, "SELECT count(*) FROM blocks WHERE block_commit_sha = $1", pinSHA); n != 1 {
		t.Fatalf("%d blocks mirrored in %s, want 1", n, pinSHA)
	}

	clear(t, otherDigest)
	assertPending(t, nil, nil)

	clear(t, testDigest)
	assertPending(t, nil, []core.Digest{testDigest})

	mustBlock(t, st, newBlock(testDigest, ""))
	assertPending(t, []core.Digest{testDigest}, nil)

	mark(t, core.BlockTrailers{Blocks: []core.Digest{testDigest}}, secondPinSHA)
	assertPending(t, nil, nil)
	if n := queryInt(t, st, "SELECT count(*) FROM blocks WHERE unblock_commit_sha = $1 AND cleared_at IS NOT NULL", secondPinSHA); n != 1 {
		t.Fatalf("%d moot unblocks settled, want 1", n)
	}

	clear(t, testDigest)
	assertPending(t, nil, []core.Digest{testDigest})
	mark(t, core.BlockTrailers{Unblocks: []core.Digest{testDigest}}, pinSHA)
	assertPending(t, nil, nil)

	mustBlock(t, st, newBlock(thirdDigest, ""))
	trailers := pending(t)
	clear(t, thirdDigest)
	mark(t, trailers, secondPinSHA)
	assertPending(t, nil, []core.Digest{thirdDigest})

	if err := st.MarkBlocksMirrored(t.Context(), blogProd, core.BlockTrailers{}, core.Commit{SHA: pinSHA}); err != nil {
		t.Errorf("MarkBlocksMirrored(nothing) = %v", err)
	}
	if err := st.MarkBlocksMirrored(t.Context(), blogProd, trailers, core.Commit{SHA: "main"}); err == nil {
		t.Error("MarkBlocksMirrored(bad sha) succeeded")
	}
	if err := st.MarkBlocksMirrored(t.Context(), blogProd, core.BlockTrailers{Blocks: []core.Digest{"x"}}, core.Commit{SHA: pinSHA}); err == nil {
		t.Error("MarkBlocksMirrored(bad digest) succeeded")
	}
}

func TestSchemaRejectsInvalidBlocks(t *testing.T) {
	t.Parallel()

	const insert = "INSERT INTO blocks (app_id, digest, commit_sha, created_by, cleared_at, cleared_by, unblock_commit_sha) VALUES ($1, "
	tests := []struct {
		name       string
		sql        string
		constraint string
	}{
		{name: "no subject", sql: insert + "NULL, NULL, 'ops', NULL, NULL, NULL)", constraint: "blocks_subject_check"},
		{name: "bad digest", sql: insert + "'sha256:x', NULL, 'ops', NULL, NULL, NULL)", constraint: "blocks_digest_check"},
		{name: "bad commit", sql: insert + "NULL, 'main', 'ops', NULL, NULL, NULL)", constraint: "blocks_commit_sha_check"},
		{name: "blank creator", sql: insert + "NULL, '" + testSHA + "', ' ', NULL, NULL, NULL)", constraint: "blocks_created_by_check"},
		{name: "cleared without actor", sql: insert + "NULL, '" + testSHA + "', 'ops', now(), NULL, NULL)", constraint: "blocks_cleared_check"},
		{name: "unblocked while active", sql: insert + "NULL, '" + testSHA + "', 'ops', NULL, NULL, '" + testSHA + "')", constraint: "blocks_unblock_check"},
		{name: "second active digest", sql: insert + "'" + testDigest + "', NULL, 'ops', NULL, NULL, NULL); " + insert + "'" + testDigest + "', NULL, 'ops', NULL, NULL, NULL)", constraint: "blocks_active_digest_key"},
		{name: "second active commit-only", sql: insert + "NULL, '" + testSHA + "', 'ops', NULL, NULL, NULL); " + insert + "NULL, '" + testSHA + "', 'ops', NULL, NULL, NULL)", constraint: "blocks_active_commit_key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			appID, err := st.EnsureApp(t.Context(), blogProd)
			if err != nil {
				t.Fatalf("EnsureApp: %v", err)
			}
			var lastErr error
			for statement := range strings.SplitSeq(tt.sql, "; ") {
				if lastErr = store.ExecSQL(t.Context(), st, statement, appID); lastErr != nil {
					break
				}
			}
			assertConstraint(t, lastErr, tt.constraint)
		})
	}

	st := storetest.Open(t)
	appID, err := st.EnsureApp(t.Context(), blogProd)
	if err != nil {
		t.Fatalf("EnsureApp: %v", err)
	}
	mustExec(t, st, insert+"'"+testDigest+"', NULL, 'ops', now(), 'ops', NULL)", appID)
	mustExec(t, st, insert+"'"+testDigest+"', NULL, 'ops', NULL, NULL, NULL)", appID)
}

func TestLastSuccessfulDeploymentSkipsBlockedDigests(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	succeed := func(digest core.Digest) core.Deployment {
		t.Helper()
		d := mustAdvance(t, st, mustCreate(t, st, deploymentOf("blog", randomSHA(t)), 0), core.StateVerifying)
		got, err := st.TransitionDeployment(t.Context(), store.Transition{
			ID: d.ID, From: core.StateVerifying, To: core.StateSucceeded, Lineage: core.Lineage{Digest: digest},
		})
		if err != nil {
			t.Fatalf("succeed %d: %v", d.ID, err)
		}
		return got
	}
	v1 := succeed(testDigest)
	v2 := succeed(otherDigest)
	v3 := succeed(thirdDigest)

	mustBlock(t, st, newBlock(thirdDigest, ""))
	got, err := st.LastSuccessfulDeployment(t.Context(), blogProd, "")
	if err != nil || got.ID != v2.ID {
		t.Fatalf("LastSuccessfulDeployment = %d, %v, want %d (v3 is blocked)", got.ID, err, v2.ID)
	}
	got, err = st.LastSuccessfulDeployment(t.Context(), blogProd, otherDigest)
	if err != nil || got.ID != v1.ID {
		t.Fatalf("LastSuccessfulDeployment(exclude v2) = %d, %v, want %d", got.ID, err, v1.ID)
	}

	mustBlock(t, st, newBlock("", v1.Commit.SHA))
	if got, err := st.LastSuccessfulDeployment(t.Context(), blogProd, otherDigest); err != nil || got.ID != v1.ID {
		t.Fatalf("a commit-only block does not hide a digest: got %d, %v, want %d", got.ID, err, v1.ID)
	}

	if _, err := st.ClearBlocks(t.Context(), blogProd, core.BlockRef{Digest: thirdDigest}, "user:bob"); err != nil {
		t.Fatalf("ClearBlocks: %v", err)
	}
	if got, err := st.LastSuccessfulDeployment(t.Context(), blogProd, ""); err != nil || got.ID != v3.ID {
		t.Fatalf("after unblock = %d, %v, want %d", got.ID, err, v3.ID)
	}
}

func TestCurrentDeployment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, st *store.Store) core.DeploymentID
	}{
		{
			name:  "nothing deployed",
			setup: func(*testing.T, *store.Store) core.DeploymentID { return 0 },
		},
		{
			name: "only queued, superseded and unpinned failures",
			setup: func(t *testing.T, st *store.Store) core.DeploymentID {
				t.Helper()
				mustCreate(t, st, deploymentOf("blog", randomSHA(t)), 0)
				mustCreateIn(t, st, deploymentOf("blog", randomSHA(t)), core.StateFailed)
				mustCreateIn(t, st, deploymentOf("blog", randomSHA(t)), core.StateCancelled)
				return 0
			},
		},
		{
			name: "latest success",
			setup: func(t *testing.T, st *store.Store) core.DeploymentID {
				t.Helper()
				mustCreateIn(t, st, deploymentOf("blog", randomSHA(t)), core.StateSucceeded)
				want := mustCreateIn(t, st, deploymentOf("blog", randomSHA(t)), core.StateSucceeded)
				mustCreate(t, st, deploymentOf("blog", randomSHA(t)), 0)
				mustCreateIn(t, st, deploymentOf("blog", randomSHA(t)), core.StateFailed)
				return want.ID
			},
		},
		{
			name: "in flight beats an earlier success",
			setup: func(t *testing.T, st *store.Store) core.DeploymentID {
				t.Helper()
				mustCreateIn(t, st, deploymentOf("blog", randomSHA(t)), core.StateSucceeded)
				return mustCreateIn(t, st, deploymentOf("blog", randomSHA(t)), core.StateSyncing).ID
			},
		},
		{
			name: "pinned failure",
			setup: func(t *testing.T, st *store.Store) core.DeploymentID {
				t.Helper()
				mustCreateIn(t, st, deploymentOf("blog", randomSHA(t)), core.StateSucceeded)
				failed := mustCreateIn(t, st, deploymentOf("blog", randomSHA(t)), core.StateFailed)
				mustExec(t, st, "UPDATE deployments SET pin_commit_sha = $2 WHERE id = $1", failed.ID, pinSHA)
				mustCreateIn(t, st, deploymentOf("blog", randomSHA(t)), core.StateFailed)
				return failed.ID
			},
		},
		{
			name: "pinned cancellation",
			setup: func(t *testing.T, st *store.Store) core.DeploymentID {
				t.Helper()
				cancelled := mustAdvance(t, st, mustCreate(t, st, deploymentOf("blog", randomSHA(t)), 0), core.StateSyncing)
				mustExec(t, st, "UPDATE deployments SET pin_commit_sha = $2 WHERE id = $1", cancelled.ID, pinSHA)
				_, err := st.TransitionDeployment(t.Context(), store.Transition{ID: cancelled.ID, From: core.StateSyncing, To: core.StateCancelled})
				if err != nil {
					t.Fatalf("cancel: %v", err)
				}
				return cancelled.ID
			},
		},
		{
			name: "started order beats creation order",
			setup: func(t *testing.T, st *store.Store) core.DeploymentID {
				t.Helper()
				older := mustCreate(t, st, deploymentOf("blog", randomSHA(t)), 0)
				newer := mustCreateIn(t, st, deploymentOf("blog", randomSHA(t)), core.StateSucceeded)
				older = mustAdvance(t, st, older, core.StateSucceeded)
				if !older.StartedAt.After(*newer.StartedAt) {
					t.Fatalf("fixture: %d started %v, %d started %v", older.ID, older.StartedAt, newer.ID, newer.StartedAt)
				}
				return older.ID
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			want := tt.setup(t, st)
			staging := deploymentOf("blog", randomSHA(t))
			staging.Target.Environment = "staging"
			mustCreateIn(t, st, staging, core.StateSucceeded)

			got, err := st.CurrentDeployment(t.Context(), blogProd)
			if want == 0 {
				if !errors.Is(err, core.ErrNotFound) {
					t.Fatalf("CurrentDeployment = %d, %v, want ErrNotFound", got.ID, err)
				}
				return
			}
			if err != nil || got.ID != want {
				t.Fatalf("CurrentDeployment = %d, %v, want %d", got.ID, err, want)
			}
		})
	}

	st := storetest.Open(t)
	if _, err := st.CurrentDeployment(t.Context(), core.Target{App: "blog"}); err == nil {
		t.Error("CurrentDeployment(invalid target) succeeded")
	}
}

func TestCurrentAndLatestDeployments(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	ids := func(t *testing.T, got []core.Deployment, err error) map[core.Target]core.DeploymentID {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out := map[core.Target]core.DeploymentID{}
		for _, d := range got {
			if _, dup := out[d.Target]; dup {
				t.Errorf("%s listed twice", d.Target)
			}
			out[d.Target] = d.ID
		}
		return out
	}
	current, err := st.CurrentDeployments(t.Context())
	if got := ids(t, current, err); len(got) != 0 {
		t.Errorf("CurrentDeployments on an empty store = %v", got)
	}

	blogLive := mustCreateIn(t, st, deploymentOf("blog", randomSHA(t)), core.StateSucceeded)
	blogQueued := mustCreate(t, st, deploymentOf("blog", randomSHA(t)), 0)
	shopFailed := mustCreateIn(t, st, deploymentOf("shop", randomSHA(t)), core.StateFailed)
	staging := deploymentOf("blog", randomSHA(t))
	staging.Target.Environment = "staging"
	stagingSyncing := mustCreateIn(t, st, staging, core.StateSyncing)
	shopProd := core.Target{App: "shop", Environment: "prod"}

	current, err = st.CurrentDeployments(t.Context())
	wantCurrent := map[core.Target]core.DeploymentID{blogProd: blogLive.ID, stagingSyncing.Target: stagingSyncing.ID}
	if got := ids(t, current, err); !maps.Equal(got, wantCurrent) {
		t.Errorf("CurrentDeployments = %v, want %v", got, wantCurrent)
	}
	for _, d := range current {
		single, err := st.CurrentDeployment(t.Context(), d.Target)
		if err != nil || !reflect.DeepEqual(single, d) {
			t.Errorf("CurrentDeployment(%s) = %+v, %v; want %+v", d.Target, single, err, d)
		}
	}

	latest, err := st.LatestDeployments(t.Context())
	wantLatest := map[core.Target]core.DeploymentID{blogProd: blogQueued.ID, shopProd: shopFailed.ID, stagingSyncing.Target: stagingSyncing.ID}
	if got := ids(t, latest, err); !maps.Equal(got, wantLatest) {
		t.Errorf("LatestDeployments = %v, want %v", got, wantLatest)
	}
}

func TestLockTarget(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	err := st.InTx(t.Context(), func(tx *store.Store) error {
		return tx.LockTarget(t.Context(), blogProd)
	})
	if err != nil || countApps(t, st, "blog") != 1 {
		t.Fatalf("LockTarget = %v, apps = %d", err, countApps(t, st, "blog"))
	}
	if err := st.LockTarget(t.Context(), core.Target{}); err == nil {
		t.Error("LockTarget(invalid target) succeeded")
	}
}
