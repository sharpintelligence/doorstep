package intake_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/intake"
)

type fakeCommits struct {
	mu        sync.Mutex
	rows      []core.TrackedCommit
	queue     *fakeQueue
	blocks    *fakeBlocks
	trackErr  map[core.Target]error
	settleErr error
	openCalls [][]core.FollowedBranch
}

func newFakeCommits() *fakeCommits {
	return &fakeCommits{queue: &fakeQueue{}, blocks: &fakeBlocks{}, trackErr: map[core.Target]error{}}
}

func (f *fakeCommits) Track(ctx context.Context, c core.TrackedCommit, isHead intake.HeadCheck) (core.TrackedCommit, core.TrackOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.trackErr[c.Target]; err != nil {
		return core.TrackedCommit{}, "", err
	}
	if err := c.Validate(); err != nil {
		return core.TrackedCommit{}, "", err
	}
	head, err := isHead(ctx)
	if err != nil {
		return core.TrackedCommit{}, "", err
	}
	if !head {
		return core.TrackedCommit{}, core.TrackStale, nil
	}
	outcome := core.TrackCreated
	index := slices.IndexFunc(f.rows, func(row core.TrackedCommit) bool {
		return row.Target == c.Target && row.Commit.SHA == c.Commit.SHA
	})
	switch {
	case index < 0:
		c.ID = core.TrackedCommitID(len(f.rows) + 1)
		f.rows = append(f.rows, c)
		index = len(f.rows) - 1
	case f.rows[index].SupersededByID != nil && f.rows[index].EnqueuedAt == nil:
		f.rows[index].SupersededByID = nil
		outcome = core.TrackReopened
	default:
		outcome = core.TrackExisting
	}
	id := f.rows[index].ID
	for i := range f.rows {
		if i != index && f.rows[i].Target == c.Target && f.rows[i].Open() {
			f.rows[i].SupersededByID = &id
		}
	}
	return f.rows[index], outcome, nil
}

func (f *fakeCommits) Tracked(_ context.Context, repository, sha string) ([]core.TrackedCommit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.TrackedCommit
	for _, row := range f.rows {
		if row.Commit.SHA == sha && strings.EqualFold(row.Commit.Repository, repository) {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeCommits) Settle(ctx context.Context, id core.TrackedCommitID, eval core.ChecksEvaluation, enqueue intake.EnqueueFunc) (core.TrackedCommit, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.settleErr != nil {
		return core.TrackedCommit{}, false, f.settleErr
	}
	if err := eval.Validate(); err != nil {
		return core.TrackedCommit{}, false, err
	}
	row := f.rows[id-1]
	if !row.Open() {
		return row, false, nil
	}
	if row.EvaluatedAt == nil || row.EvaluatedAt.Before(eval.ReadAt) {
		readAt := eval.ReadAt
		row.ChecksState, row.Checks, row.EvaluatedAt = eval.State, slices.Clone(eval.Checks), &readAt
	}
	if row.ChecksState != core.ChecksSuccess {
		f.rows[id-1] = row
		return row, false, nil
	}
	deploymentID, err := enqueue(ctx, f.queue, f.blocks)
	if errors.Is(err, core.ErrBlocked) {
		f.rows[id-1] = row
		return row, false, nil
	}
	if err != nil {
		return core.TrackedCommit{}, false, err
	}
	enqueuedAt := eval.ReadAt
	row.DeploymentID, row.EnqueuedAt = &deploymentID, &enqueuedAt
	f.rows[id-1] = row
	return row, true, nil
}

func (f *fakeCommits) Open(_ context.Context, followed []core.FollowedBranch, limit int) ([]core.TrackedCommit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openCalls = append(f.openCalls, slices.Clone(followed))
	var out []core.TrackedCommit
	for _, row := range f.rows {
		follows := slices.ContainsFunc(followed, func(b core.FollowedBranch) bool {
			return b.Target == row.Target && strings.EqualFold(b.Repository, row.Commit.Repository) && b.Branch == row.Branch
		})
		if follows && row.Open() && len(out) < limit {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeCommits) get(id core.TrackedCommitID) core.TrackedCommit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[id-1]
}

func (f *fakeCommits) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

type fakeQueue struct {
	mu          sync.Mutex
	deployments []core.Deployment
	err         error
}

func (q *fakeQueue) Enqueue(_ context.Context, d core.Deployment) (core.Deployment, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return core.Deployment{}, q.err
	}
	if d.State == "" {
		d.State = core.StateQueued
	}
	if err := d.Validate(); err != nil {
		return core.Deployment{}, err
	}
	d.ID = core.DeploymentID(len(q.deployments) + 100)
	q.deployments = append(q.deployments, d)
	return d, nil
}

func (q *fakeQueue) all() []core.Deployment {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.deployments)
}

func (q *fakeQueue) fail(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.err = err
}

type fakeBlocks struct {
	mu      sync.Mutex
	blocked map[string]core.Block
	err     error
	calls   int
}

func (f *fakeBlocks) Blocked(_ context.Context, t core.Target, commit core.Commit) (core.Block, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return core.Block{}, false, f.err
	}
	b, ok := f.blocked[t.String()+"@"+commit.SHA]
	return b, ok, nil
}

func (f *fakeBlocks) block(b core.Block) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blocked == nil {
		f.blocked = map[string]core.Block{}
	}
	f.blocked[b.Target.String()+"@"+b.Commit.SHA] = b
}

func (f *fakeBlocks) unblock(t core.Target, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.blocked, t.String()+"@"+sha)
}

func (f *fakeBlocks) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeBlocks) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type checksCall struct {
	repository string
	sha        string
	names      []string
}

type fakeChecks struct {
	mu        sync.Mutex
	snapshots map[string]intake.Snapshot
	err       error
	calls     []checksCall
	onRead    func(call int, s intake.Snapshot) intake.Snapshot
}

func newFakeChecks() *fakeChecks {
	return &fakeChecks{snapshots: map[string]intake.Snapshot{}}
}

func (f *fakeChecks) Checks(_ context.Context, repository, sha string, names []string) (intake.Snapshot, error) {
	f.mu.Lock()
	f.calls = append(f.calls, checksCall{repository: repository, sha: sha, names: slices.Clone(names)})
	call, onRead, err := len(f.calls), f.onRead, f.err
	snapshot := f.snapshots[strings.ToLower(repository)+"@"+sha]
	f.mu.Unlock()
	if err != nil {
		return intake.Snapshot{}, err
	}
	if onRead != nil {
		snapshot = onRead(call, snapshot)
	}
	return snapshot, nil
}

func (f *fakeChecks) set(repository, sha string, s intake.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshots[strings.ToLower(repository)+"@"+sha] = s
}

func (f *fakeChecks) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeChecks) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeChecks) lastCall() checksCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return checksCall{}
	}
	return f.calls[len(f.calls)-1]
}

type fakeBranches struct {
	mu    sync.Mutex
	heads map[string]string
	err   error
	calls int
}

func newFakeBranches() *fakeBranches {
	return &fakeBranches{heads: map[string]string{}}
}

func (f *fakeBranches) BranchHead(_ context.Context, repository, branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.heads[strings.ToLower(repository)+":"+branch], nil
}

func (f *fakeBranches) set(repository, branch, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heads[strings.ToLower(repository)+":"+branch] = sha
}

func (f *fakeBranches) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

type tickingClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTickingClock() *tickingClock {
	return &tickingClock{now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
}

func (c *tickingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Second)
	return c.now
}

var errBoom = errors.New("boom")
