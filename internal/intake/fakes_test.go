package intake_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/intake"
)

type fakeCommits struct {
	mu       sync.Mutex
	rows     []core.TrackedCommit
	queue    *fakeQueue
	trackErr map[core.Target]error
	saveErr  error
}

func newFakeCommits() *fakeCommits {
	return &fakeCommits{queue: &fakeQueue{}, trackErr: map[core.Target]error{}}
}

func (f *fakeCommits) Track(_ context.Context, c core.TrackedCommit) (core.TrackedCommit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.trackErr[c.Target]; err != nil {
		return core.TrackedCommit{}, err
	}
	if err := c.Validate(); err != nil {
		return core.TrackedCommit{}, err
	}
	for _, row := range f.rows {
		if row.Target == c.Target && row.Commit.SHA == c.Commit.SHA {
			return row, nil
		}
	}
	c.ID = core.TrackedCommitID(len(f.rows) + 1)
	for i := range f.rows {
		if f.rows[i].Target == c.Target && f.rows[i].Open() {
			id := c.ID
			f.rows[i].SupersededByID = &id
		}
	}
	f.rows = append(f.rows, c)
	return c, nil
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

func (f *fakeCommits) SaveChecks(_ context.Context, id core.TrackedCommitID, state core.ChecksState, checks []core.Check) (core.TrackedCommit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return core.TrackedCommit{}, f.saveErr
	}
	row := &f.rows[id-1]
	if row.Open() {
		row.ChecksState = state
		row.Checks = slices.Clone(checks)
	}
	return *row, nil
}

func (f *fakeCommits) Promote(ctx context.Context, id core.TrackedCommitID, enqueue intake.EnqueueFunc) (core.TrackedCommit, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := &f.rows[id-1]
	if row.ChecksState != core.ChecksSuccess || !row.Open() {
		return *row, false, nil
	}
	deploymentID, err := enqueue(ctx, f.queue)
	if err != nil {
		return core.TrackedCommit{}, false, err
	}
	row.DeploymentID = &deploymentID
	return *row, true, nil
}

func (f *fakeCommits) Pending(_ context.Context, limit int) ([]core.TrackedCommit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.TrackedCommit
	for _, row := range f.rows {
		if row.ChecksState == core.ChecksPending && row.Open() && len(out) < limit {
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

type checksCall struct {
	repository string
	sha        string
	withSuites bool
}

type fakeChecks struct {
	mu        sync.Mutex
	snapshots map[string]intake.Snapshot
	err       error
	calls     []checksCall
}

func newFakeChecks() *fakeChecks {
	return &fakeChecks{snapshots: map[string]intake.Snapshot{}}
}

func (f *fakeChecks) Checks(_ context.Context, repository, sha string, withSuites bool) (intake.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, checksCall{repository: repository, sha: sha, withSuites: withSuites})
	if f.err != nil {
		return intake.Snapshot{}, f.err
	}
	return f.snapshots[strings.ToLower(repository)+"@"+sha], nil
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

var errBoom = errors.New("boom")
