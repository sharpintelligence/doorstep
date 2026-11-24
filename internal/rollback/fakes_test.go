package rollback_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/rollback"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type fakeState struct {
	deployments []core.Deployment
	priorities  map[core.DeploymentID]core.Priority
	blocks      []core.Block
	audit       []core.AuditEntry
	transitions []store.Transition
	mirrored    []mirrorCall
	nextID      core.DeploymentID
	nextBlockID core.BlockID
}

type mirrorCall struct {
	target   core.Target
	trailers core.BlockTrailers
	commit   core.Commit
}

func (s fakeState) clone() fakeState {
	c := s
	c.deployments = slices.Clone(s.deployments)
	c.priorities = make(map[core.DeploymentID]core.Priority, len(s.priorities))
	for k, v := range s.priorities {
		c.priorities[k] = v
	}
	c.blocks = slices.Clone(s.blocks)
	c.audit = slices.Clone(s.audit)
	c.transitions = slices.Clone(s.transitions)
	c.mirrored = slices.Clone(s.mirrored)
	return c
}

type fakeRepo struct {
	mu        sync.Mutex
	state     fakeState
	conflicts int
	errs      map[string]error
	locked    []core.Target
	txs       int
	trailers  core.BlockTrailers
	now       time.Time
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		state:  fakeState{priorities: map[core.DeploymentID]core.Priority{}, nextID: 1, nextBlockID: 1},
		errs:   map[string]error{},
		now:    time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC),
		locked: nil,
	}
}

func (f *fakeRepo) fail(op string) error {
	return f.errs[op]
}

func (f *fakeRepo) InTx(ctx context.Context, fn func(rollback.Repository) error) error {
	f.mu.Lock()
	f.txs++
	snapshot := f.state.clone()
	f.mu.Unlock()
	if err := fn(f); err != nil {
		f.mu.Lock()
		f.state = snapshot
		f.mu.Unlock()
		return err
	}
	return nil
}

func (f *fakeRepo) LockTarget(_ context.Context, t core.Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.locked = append(f.locked, t)
	return f.fail("LockTarget")
}

func (f *fakeRepo) add(d core.Deployment) core.Deployment {
	f.mu.Lock()
	defer f.mu.Unlock()
	d.ID = f.state.nextID
	f.state.nextID++
	if d.Kind == "" {
		d.Kind = core.KindDeploy
	}
	if d.Source == "" {
		d.Source = core.SourceWebhook
	}
	if d.RequestedBy == "" {
		d.RequestedBy = "octocat"
	}
	if d.Commit.Repository == "" {
		d.Commit.Repository = "acme/blog"
	}
	if d.Image.Repository == "" {
		d.Image = core.ImageRef{Repository: "ghcr.io/acme/blog", Tag: d.Commit.SHA}
	}
	d.CreatedAt = f.now.Add(time.Duration(d.ID) * time.Minute)
	if d.State != core.StateQueued {
		started := d.CreatedAt
		d.StartedAt = &started
	}
	f.state.deployments = append(f.state.deployments, d)
	f.state.priorities[d.ID] = core.PriorityNormal
	return d
}

func (f *fakeRepo) addBlock(b core.Block) core.Block {
	f.mu.Lock()
	defer f.mu.Unlock()
	b.ID = f.state.nextBlockID
	f.state.nextBlockID++
	f.state.blocks = append(f.state.blocks, b)
	return b
}

func (f *fakeRepo) snapshot() fakeState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.clone()
}

func (f *fakeRepo) deployment(id core.DeploymentID) core.Deployment {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.state.deployments {
		if d.ID == id {
			return d
		}
	}
	return core.Deployment{}
}

func newestFirst(ds []core.Deployment) {
	slices.SortFunc(ds, func(a, b core.Deployment) int { return cmp.Compare(b.ID, a.ID) })
}

func (f *fakeRepo) CurrentDeployment(_ context.Context, t core.Target) (core.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("CurrentDeployment"); err != nil {
		return core.Deployment{}, err
	}
	var candidates []core.Deployment
	for _, d := range f.state.deployments {
		pinnedFailure := (d.State == core.StateFailed || d.State == core.StateCancelled) && d.PinCommitSHA != ""
		if d.Target == t && (d.State.Active() || d.State == core.StateSucceeded || pinnedFailure) {
			candidates = append(candidates, d)
		}
	}
	slices.SortFunc(candidates, func(a, b core.Deployment) int {
		return cmp.Or(b.StartedAt.Compare(*a.StartedAt), cmp.Compare(b.ID, a.ID))
	})
	if len(candidates) == 0 {
		return core.Deployment{}, fmt.Errorf("current deployment of %s: %w", t, core.ErrNotFound)
	}
	return candidates[0], nil
}

func (f *fakeRepo) LastSuccessfulDeployment(_ context.Context, t core.Target, exclude core.Digest) (core.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ds := slices.Clone(f.state.deployments)
	newestFirst(ds)
	for _, d := range ds {
		if d.Target != t || d.State != core.StateSucceeded || d.Digest == "" || d.Digest == exclude {
			continue
		}
		blocked := slices.ContainsFunc(f.state.blocks, func(b core.Block) bool {
			return b.Target == t && b.Active() && b.Digest == d.Digest
		})
		if !blocked {
			return d, nil
		}
	}
	return core.Deployment{}, fmt.Errorf("last successful deployment of %s: %w", t, core.ErrNotFound)
}

func (f *fakeRepo) MigrationsBetween(_ context.Context, t core.Target, to, current core.Deployment) ([]core.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("MigrationsBetween"); err != nil {
		return nil, err
	}
	var firstSeen core.DeploymentID
	for _, d := range f.state.deployments {
		if d.Target == t && d.Kind == core.KindDeploy && d.Commit.SHA == to.Commit.SHA && (firstSeen == 0 || d.ID < firstSeen) {
			firstSeen = d.ID
		}
	}
	var crossed []core.Deployment
	for _, d := range f.state.deployments {
		if d.Target == t && d.Kind == core.KindDeploy && d.Migration && d.Commit.SHA != to.Commit.SHA && d.ID > firstSeen && d.ID <= current.ID {
			crossed = append(crossed, d)
		}
	}
	slices.SortFunc(crossed, func(a, b core.Deployment) int { return cmp.Compare(a.ID, b.ID) })
	return crossed, nil
}

func (f *fakeRepo) GetDeployment(_ context.Context, id core.DeploymentID) (core.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.state.deployments {
		if d.ID == id {
			return d, nil
		}
	}
	return core.Deployment{}, fmt.Errorf("deployment %d: %w", id, core.ErrNotFound)
}

func (f *fakeRepo) ListDeployments(_ context.Context, filter store.DeploymentFilter) (store.DeploymentPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("ListDeployments"); err != nil {
		return store.DeploymentPage{}, err
	}
	ds := slices.Clone(f.state.deployments)
	newestFirst(ds)
	var page store.DeploymentPage
	for _, d := range ds {
		switch {
		case filter.App != "" && d.Target.App != filter.App,
			filter.Environment != "" && d.Target.Environment != filter.Environment,
			len(filter.States) > 0 && !slices.Contains(filter.States, d.State),
			len(filter.Kinds) > 0 && !slices.Contains(filter.Kinds, d.Kind),
			filter.CommitSHA != "" && d.Commit.SHA != filter.CommitSHA,
			filter.Digest != "" && d.Digest != filter.Digest:
			continue
		}
		page.Items = append(page.Items, d)
		if filter.Limit > 0 && len(page.Items) == filter.Limit {
			break
		}
	}
	return page, nil
}

func (f *fakeRepo) TransitionDeployment(_ context.Context, t store.Transition) (core.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conflicts > 0 {
		f.conflicts--
		return core.Deployment{}, fmt.Errorf("transition deployment %d: %w", t.ID, core.ErrStateConflict)
	}
	for i, d := range f.state.deployments {
		if d.ID != t.ID {
			continue
		}
		if d.State != t.From {
			return core.Deployment{}, fmt.Errorf("deployment %d is %s: %w", d.ID, d.State, core.ErrStateConflict)
		}
		if err := t.From.Transition(t.To); err != nil {
			return core.Deployment{}, err
		}
		d.State = t.To
		if t.FailureReason != "" {
			d.FailureReason = t.FailureReason
		}
		if t.To.Terminal() {
			finished := f.now
			d.FinishedAt = &finished
			delete(f.state.priorities, d.ID)
		}
		f.state.deployments[i] = d
		f.state.transitions = append(f.state.transitions, t)
		return d, nil
	}
	return core.Deployment{}, fmt.Errorf("deployment %d: %w", t.ID, core.ErrNotFound)
}

func (f *fakeRepo) EnqueueDeployment(_ context.Context, d core.Deployment, p core.Priority) (core.Deployment, bool, error) {
	if err := f.fail("EnqueueDeployment"); err != nil {
		return core.Deployment{}, false, err
	}
	d.State = core.StateQueued
	if err := d.Validate(); err != nil {
		return core.Deployment{}, false, err
	}
	created := f.add(d)
	f.mu.Lock()
	f.state.priorities[created.ID] = p
	f.mu.Unlock()
	return created, true, nil
}

func (f *fakeRepo) CreateBlock(_ context.Context, b core.Block) (core.Block, bool, error) {
	if err := f.fail("CreateBlock"); err != nil {
		return core.Block{}, false, err
	}
	if err := b.Validate(); err != nil {
		return core.Block{}, false, err
	}
	f.mu.Lock()
	for i, existing := range f.state.blocks {
		if existing.Target != b.Target || !existing.Active() {
			continue
		}
		sameDigest := b.Digest != "" && existing.Digest == b.Digest
		sameCommit := b.Digest == "" && b.Commit.SHA != "" && existing.Commit.SHA == b.Commit.SHA
		if sameDigest || sameCommit {
			if existing.Commit.SHA == "" {
				existing.Commit = b.Commit
			}
			if existing.RollbackID == nil {
				existing.RollbackID = b.RollbackID
			}
			f.state.blocks[i] = existing
			f.mu.Unlock()
			return existing, false, nil
		}
	}
	f.mu.Unlock()
	b.CreatedAt = f.now
	return f.addBlock(b), true, nil
}

func (f *fakeRepo) ClearBlocks(_ context.Context, t core.Target, ref core.BlockRef, actor string) ([]core.Block, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var cleared []core.Block
	for i, b := range f.state.blocks {
		if b.Target != t || !b.Active() || !matches(b, ref) {
			continue
		}
		at := f.now
		b.ClearedAt, b.ClearedBy = &at, actor
		f.state.blocks[i] = b
		cleared = append(cleared, b)
	}
	if len(cleared) == 0 {
		return nil, fmt.Errorf("unblock %s: %w", ref, core.ErrNotFound)
	}
	return cleared, nil
}

func matches(b core.Block, ref core.BlockRef) bool {
	return (ref.Digest != "" && b.Digest == ref.Digest) || (ref.CommitSHA != "" && b.Commit.SHA == ref.CommitSHA)
}

func (f *fakeRepo) ActiveBlock(_ context.Context, t core.Target, ref core.BlockRef) (core.Block, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("ActiveBlock"); err != nil {
		return core.Block{}, false, err
	}
	if err := ref.Validate(); err != nil {
		return core.Block{}, false, err
	}
	for _, b := range f.state.blocks {
		if b.Target == t && b.Active() && matches(b, ref) {
			return b, true, nil
		}
	}
	return core.Block{}, false, nil
}

func (f *fakeRepo) ListBlocks(_ context.Context, t core.Target, includeCleared bool) ([]core.Block, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.Block
	for _, b := range slices.Backward(f.state.blocks) {
		if b.Target == t && (includeCleared || b.Active()) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (f *fakeRepo) PendingBlockTrailers(context.Context, core.Target) (core.BlockTrailers, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.trailers, f.fail("PendingBlockTrailers")
}

func (f *fakeRepo) MarkBlocksMirrored(_ context.Context, t core.Target, trailers core.BlockTrailers, commit core.Commit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.mirrored = append(f.state.mirrored, mirrorCall{target: t, trailers: trailers, commit: commit})
	return nil
}

func (f *fakeRepo) RecordAudit(_ context.Context, e core.AuditEntry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if err := f.fail("RecordAudit"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.audit = append(f.state.audit, e)
	return nil
}

type recordingObserver struct {
	mu       sync.Mutex
	apps     []string
	triggers []rollback.Trigger
}

func (o *recordingObserver) RollbackRequested(app string, trigger rollback.Trigger) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.apps = append(o.apps, app)
	o.triggers = append(o.triggers, trigger)
}

func (o *recordingObserver) calls() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.apps)
}

func (o *recordingObserver) triggered() []rollback.Trigger {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.triggers)
}

var errBoom = errors.New("boom")
