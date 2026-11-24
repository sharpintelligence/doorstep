package deploys_test

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/deploys"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type fakeState struct {
	deployments []core.Deployment
	priorities  map[core.DeploymentID]core.Priority
	audit       []core.AuditEntry
	nextID      core.DeploymentID
}

func (s fakeState) clone() fakeState {
	c := s
	c.deployments = slices.Clone(s.deployments)
	c.priorities = make(map[core.DeploymentID]core.Priority, len(s.priorities))
	for k, v := range s.priorities {
		c.priorities[k] = v
	}
	c.audit = slices.Clone(s.audit)
	return c
}

type fakeRepo struct {
	mu               sync.Mutex
	state            fakeState
	blocks           []core.Block
	conflicts        int
	errs             map[string]error
	locked           []core.Target
	beforeTransition func(f *fakeRepo)
	external         map[core.DeploymentID]core.State
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		state: fakeState{priorities: map[core.DeploymentID]core.Priority{}, nextID: 1},
		errs:  map[string]error{},
	}
}

func (f *fakeRepo) InTx(_ context.Context, fn func(deploys.Repository) error) error {
	f.mu.Lock()
	snapshot := f.state.clone()
	f.mu.Unlock()
	if err := fn(f); err != nil {
		f.mu.Lock()
		f.state = snapshot
		for id, state := range f.external {
			f.setStateLocked(id, state)
		}
		f.mu.Unlock()
		return err
	}
	return nil
}

func (f *fakeRepo) LockTarget(_ context.Context, t core.Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["LockTarget"]; err != nil {
		return err
	}
	f.locked = append(f.locked, t)
	return nil
}

func (f *fakeRepo) Blocked(_ context.Context, t core.Target, commit core.Commit) (core.Block, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["Blocked"]; err != nil {
		return core.Block{}, false, err
	}
	for _, b := range f.blocks {
		if b.Target == t && b.Commit.SHA == commit.SHA && b.Active() {
			return b, true, nil
		}
	}
	return core.Block{}, false, nil
}

func (f *fakeRepo) EnqueueDeployment(_ context.Context, d core.Deployment, priority core.Priority) (core.Deployment, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["EnqueueDeployment"]; err != nil {
		return core.Deployment{}, false, err
	}
	for _, existing := range f.state.deployments {
		if existing.Target == d.Target && existing.Kind == core.KindDeploy && existing.Commit.SHA == d.Commit.SHA && existing.State.Claimable() {
			if existing.State == core.StateQueued && f.state.priorities[existing.ID] < priority {
				f.state.priorities[existing.ID] = priority
			}
			return existing, false, nil
		}
	}
	d.ID = f.state.nextID
	f.state.nextID++
	d.State = core.StateQueued
	if err := d.Validate(); err != nil {
		return core.Deployment{}, false, err
	}
	f.state.deployments = append(f.state.deployments, d)
	f.state.priorities[d.ID] = priority
	return d, true, nil
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

func (f *fakeRepo) TransitionDeployment(_ context.Context, t store.Transition) (core.Deployment, error) {
	f.mu.Lock()
	hook := f.beforeTransition
	f.mu.Unlock()
	if hook != nil {
		hook(f)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conflicts > 0 {
		f.conflicts--
		return core.Deployment{}, fmt.Errorf("transition: %w", core.ErrStateConflict)
	}
	if err := t.From.Transition(t.To); err != nil {
		return core.Deployment{}, err
	}
	for i, d := range f.state.deployments {
		if d.ID != t.ID {
			continue
		}
		if d.State != t.From {
			return core.Deployment{}, fmt.Errorf("deployment %d is %s: %w", d.ID, d.State, core.ErrStateConflict)
		}
		d.State = t.To
		f.state.deployments[i] = d
		if t.To.Terminal() {
			delete(f.state.priorities, d.ID)
		}
		return d, nil
	}
	return core.Deployment{}, core.ErrNotFound
}

func (f *fakeRepo) RecordAudit(_ context.Context, e core.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["RecordAudit"]; err != nil {
		return err
	}
	if err := e.Validate(); err != nil {
		return err
	}
	f.state.audit = append(f.state.audit, e)
	return nil
}

func (f *fakeRepo) add(d core.Deployment) core.Deployment {
	f.mu.Lock()
	defer f.mu.Unlock()
	d.ID = f.state.nextID
	f.state.nextID++
	f.state.deployments = append(f.state.deployments, d)
	if d.State.Claimable() {
		f.state.priorities[d.ID] = core.PriorityNormal
	}
	return d
}

func (f *fakeRepo) snapshot() fakeState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.clone()
}

func (f *fakeRepo) moveExternally(id core.DeploymentID, state core.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.external == nil {
		f.external = map[core.DeploymentID]core.State{}
	}
	f.external[id] = state
	f.setStateLocked(id, state)
}

func (f *fakeRepo) setStateLocked(id core.DeploymentID, state core.State) {
	for i, d := range f.state.deployments {
		if d.ID == id {
			f.state.deployments[i].State = state
		}
	}
}

type fakeCommits struct {
	mu       sync.Mutex
	onBranch map[string]bool
	err      error
	calls    []string
}

func (f *fakeCommits) OnBranch(_ context.Context, repository, branch, sha string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, repository+"@"+branch+":"+sha)
	if f.err != nil {
		return false, f.err
	}
	on, known := f.onBranch[sha]
	if !known {
		return false, fmt.Errorf("compare %s: %w", sha, deploys.ErrCommitNotFound)
	}
	return on, nil
}

type fakeMigrations struct {
	flagged bool
	reason  string
	err     error
}

func (f fakeMigrations) MigrationFlag(context.Context, core.Target, core.Commit) (bool, string, error) {
	return f.flagged, f.reason, f.err
}
