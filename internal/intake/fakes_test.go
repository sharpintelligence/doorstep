package intake_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/intake"
	"github.com/sharpintelligence/doorstep/internal/store"
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
	errs  map[string]error
	err   error
	calls int
	reads []string
}

func newFakeBranches() *fakeBranches {
	return &fakeBranches{heads: map[string]string{}, errs: map[string]error{}}
}

func (f *fakeBranches) BranchHead(_ context.Context, repository, branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	key := strings.ToLower(repository) + ":" + branch
	f.reads = append(f.reads, key)
	if f.err != nil {
		return "", f.err
	}
	if err := f.errs[key]; err != nil {
		return "", err
	}
	return f.heads[key], nil
}

func (f *fakeBranches) failFor(repository, branch string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[strings.ToLower(repository)+":"+branch] = err
}

func (f *fakeBranches) readKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reads)
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

type reevaluation struct {
	repository string
	sha        string
}

type fakeIntake struct {
	mu            sync.Mutex
	pushes        []intake.Push
	reevaluations []reevaluation
	open          []core.TrackedCommit
	heads         map[string]string
	pushErr       error
	reevaluateErr error
	openErr       error
	tracksErr     error
	ctxErrs       []error
	openCalls     int
}

func fakeHeadKey(repository, branch string) string {
	return strings.ToLower(repository) + ":" + branch
}

func (f *fakeIntake) HandlePush(ctx context.Context, p intake.Push) ([]intake.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes = append(f.pushes, p)
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	if f.pushErr != nil {
		return nil, f.pushErr
	}
	f.trackLocked(p.Repository, p.Branch, p.SHA)
	return []intake.Result{{Commit: core.TrackedCommit{Commit: core.Commit{Repository: p.Repository, SHA: p.SHA}}, Enqueued: true}}, nil
}

func (f *fakeIntake) Reevaluate(ctx context.Context, repository, sha string) ([]intake.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reevaluations = append(f.reevaluations, reevaluation{repository: repository, sha: sha})
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	if f.reevaluateErr != nil {
		return nil, f.reevaluateErr
	}
	return nil, nil
}

func (f *fakeIntake) Open(_ context.Context, limit int) ([]core.TrackedCommit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openCalls++
	if f.openErr != nil {
		return nil, f.openErr
	}
	return slices.Clone(f.open[:min(limit, len(f.open))]), nil
}

func (f *fakeIntake) TracksHead(_ context.Context, repository, branch, sha string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tracksErr != nil {
		return false, f.tracksErr
	}
	return f.heads[fakeHeadKey(repository, branch)] == sha, nil
}

func (f *fakeIntake) track(repository, branch, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trackLocked(repository, branch, sha)
}

func (f *fakeIntake) trackLocked(repository, branch, sha string) {
	if f.heads == nil {
		f.heads = map[string]string{}
	}
	f.heads[fakeHeadKey(repository, branch)] = sha
}

func (f *fakeIntake) setOpen(commits ...core.TrackedCommit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open = commits
}

func (f *fakeIntake) failPushes(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushErr = err
}

func (f *fakeIntake) recorded() ([]intake.Push, []reevaluation, []error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pushes), slices.Clone(f.reevaluations), slices.Clone(f.ctxErrs)
}

func (f *fakeIntake) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes, f.reevaluations, f.ctxErrs = nil, nil, nil
}

type fakeDeliveries struct {
	mu          sync.Mutex
	claimed     map[string]string
	completed   map[string]bool
	released    []string
	staleAfter  []time.Duration
	claimErr    error
	completeErr error
	releaseErr  error
}

func newFakeDeliveries() *fakeDeliveries {
	return &fakeDeliveries{claimed: map[string]string{}, completed: map[string]bool{}}
}

func (f *fakeDeliveries) Claim(_ context.Context, id, event string, staleAfter time.Duration) (store.DeliveryClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.staleAfter = append(f.staleAfter, staleAfter)
	if f.claimErr != nil {
		return "", f.claimErr
	}
	if _, ok := f.claimed[id]; ok {
		if f.completed[id] {
			return store.DeliveryCompleted, nil
		}
		return store.DeliveryInProgress, nil
	}
	f.claimed[id] = event
	return store.DeliveryClaimed, nil
}

func (f *fakeDeliveries) Complete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.completeErr != nil {
		return f.completeErr
	}
	f.completed[id] = true
	return nil
}

func (f *fakeDeliveries) Release(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, id)
	if f.releaseErr != nil {
		return f.releaseErr
	}
	delete(f.claimed, id)
	return nil
}

func (f *fakeDeliveries) state() (map[string]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.claimed), slices.Clone(f.released)
}

func (f *fakeDeliveries) completedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.completed))
}

type fakeInstallations struct {
	mu            sync.Mutex
	installations []int64
	repositories  []string
}

func (f *fakeInstallations) EvictInstallation(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installations = append(f.installations, id)
}

func (f *fakeInstallations) EvictRepository(owner, repo string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repositories = append(f.repositories, owner+"/"+repo)
	return nil
}

func (f *fakeInstallations) evicted() ([]int64, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.installations), slices.Clone(f.repositories)
}

type fakeObserver struct {
	mu         sync.Mutex
	deliveries []string
	cycles     []intake.PollResult
	detections []string
}

func (f *fakeObserver) MigrationDetectionFailed(app string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detections = append(f.detections, app)
}

func (f *fakeObserver) failedDetections() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.detections)
}

func (f *fakeObserver) WebhookDelivery(event string, result intake.WebhookResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliveries = append(f.deliveries, event+"/"+string(result))
}

func (f *fakeObserver) PollCycle(result intake.PollResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cycles = append(f.cycles, result)
}

func (f *fakeObserver) recorded() ([]string, []intake.PollResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.deliveries), slices.Clone(f.cycles)
}

type fakeClaims struct {
	mu        sync.Mutex
	lease     time.Duration
	every     time.Duration
	err       error
	finishErr error
	lost      bool
	calls     int
	finished  []time.Time
	refuse    bool
}

var fakeLease = time.Date(2026, 11, 24, 12, 1, 0, 0, time.UTC)

func (f *fakeClaims) ClaimPoll(_ context.Context, lease time.Duration) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lease = lease
	if f.err != nil {
		return time.Time{}, false, f.err
	}
	if f.refuse {
		return time.Time{}, false, nil
	}
	return fakeLease, true, nil
}

func (f *fakeClaims) FinishPoll(ctx context.Context, leasedUntil time.Time, every time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.finished = append(f.finished, leasedUntil)
	f.every = every
	if f.finishErr != nil {
		return false, f.finishErr
	}
	return !f.lost, nil
}

func (f *fakeClaims) finishedLeases() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.finished)
}

func (f *fakeClaims) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeExpired struct {
	mu      sync.Mutex
	calls   []time.Duration
	deleted int64
	err     error
}

func (f *fakeExpired) DeleteBefore(_ context.Context, olderThan time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, olderThan)
	return f.deleted, f.err
}

func (f *fakeExpired) recorded() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}
