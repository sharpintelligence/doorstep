package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

var zeroSHA = strings.Repeat("0", 40)

type Options struct {
	Apps     []App
	Commits  Commits
	Checks   CheckSource
	Branches Branches
	Logger   *slog.Logger
	Now      func() time.Time
}

type Push struct {
	Repository string
	Branch     string
	SHA        string
	Source     core.Source
	PushedBy   string
}

type Result struct {
	Commit   core.TrackedCommit
	Enqueued bool
	Blocked  *core.Block
}

type Service struct {
	apps     []App
	targets  map[core.Target]App
	followed []core.FollowedBranch
	commits  Commits
	checks   CheckSource
	branches Branches
	logger   *slog.Logger
	now      func() time.Time
}

func New(opts Options) (*Service, error) {
	if opts.Commits == nil {
		return nil, errors.New("intake: commits store is required")
	}
	if opts.Checks == nil {
		return nil, errors.New("intake: check source is required")
	}
	if opts.Branches == nil {
		return nil, errors.New("intake: branch source is required")
	}
	s := &Service{
		targets:  make(map[core.Target]App, len(opts.Apps)),
		commits:  opts.Commits,
		checks:   opts.Checks,
		branches: opts.Branches,
		logger:   opts.Logger,
		now:      opts.Now,
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	var errs []error
	for i, app := range opts.Apps {
		if err := app.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("apps[%d] (%s): %w", i, app.Target, err))
			continue
		}
		if _, dup := s.targets[app.Target]; dup {
			errs = append(errs, fmt.Errorf("apps[%d]: duplicate target %s", i, app.Target))
			continue
		}
		app.RequiredChecks = append([]string(nil), app.RequiredChecks...)
		s.targets[app.Target] = app
		s.apps = append(s.apps, app)
		s.followed = append(s.followed, core.FollowedBranch{Target: app.Target, Repository: app.Repository, Branch: app.Branch})
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("intake: %w", err)
	}
	return s, nil
}

func (s *Service) HandlePush(ctx context.Context, p Push) ([]Result, error) {
	if p.SHA == zeroSHA {
		return nil, nil
	}
	if err := core.ValidateCommitSHA(p.SHA); err != nil {
		return nil, fmt.Errorf("handle push to %s: %w", p.Repository, err)
	}
	if !p.Source.Valid() {
		return nil, fmt.Errorf("handle push to %s: unknown source %q", p.Repository, p.Source)
	}

	var (
		tracked []core.TrackedCommit
		errs    []error
	)
	for _, app := range s.apps {
		if !app.matches(p.Repository, p.Branch) {
			continue
		}
		c, outcome, err := s.commits.Track(ctx, core.TrackedCommit{
			Target:      app.Target,
			Commit:      core.Commit{Repository: app.Repository, SHA: p.SHA},
			Branch:      app.Branch,
			Source:      p.Source,
			PushedBy:    p.PushedBy,
			ChecksState: core.ChecksPending,
		}, s.isHead(app, p.SHA))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", app.Target, err))
			continue
		}
		log := s.logger.With("app", app.Target.String(), "repository", app.Repository, "sha", p.SHA, "branch", app.Branch)
		switch outcome {
		case core.TrackStale:
			log.InfoContext(ctx, "ignoring push that is no longer the branch head")
			continue
		case core.TrackCreated, core.TrackReopened:
			log.InfoContext(ctx, "tracking commit", "outcome", string(outcome))
		case core.TrackExisting:
			log.DebugContext(ctx, "commit already tracked")
		}
		tracked = append(tracked, c)
	}
	results, err := s.evaluate(ctx, tracked)
	return results, errors.Join(append(errs, err)...)
}

func (s *Service) Reevaluate(ctx context.Context, repository, sha string) ([]Result, error) {
	if err := core.ValidateCommitSHA(sha); err != nil {
		return nil, fmt.Errorf("re-evaluate %s: %w", repository, err)
	}
	commits, err := s.commits.Tracked(ctx, repository, sha)
	if err != nil {
		return nil, fmt.Errorf("re-evaluate %s@%s: %w", repository, sha, err)
	}
	var open []core.TrackedCommit
	for _, c := range commits {
		if s.follows(c) {
			open = append(open, c)
		}
	}
	return s.evaluate(ctx, open)
}

func (s *Service) Open(ctx context.Context, limit int) ([]core.TrackedCommit, error) {
	if len(s.followed) == 0 {
		return nil, nil
	}
	return s.commits.Open(ctx, s.followed, limit)
}

func (s *Service) follows(c core.TrackedCommit) bool {
	app, ok := s.targets[c.Target]
	return ok && c.Open() && app.matches(c.Commit.Repository, c.Branch)
}

func (s *Service) isHead(app App, sha string) HeadCheck {
	return func(ctx context.Context) (bool, error) {
		head, err := s.branches.BranchHead(ctx, app.Repository, app.Branch)
		if err != nil {
			return false, err
		}
		return head == sha, nil
	}
}

type snapshotKey struct {
	repository string
	sha        string
}

type snapshotQuery struct {
	all   bool
	names []string
}

type readSnapshot struct {
	snapshot Snapshot
	readAt   time.Time
	err      error
}

func (s *Service) evaluate(ctx context.Context, commits []core.TrackedCommit) ([]Result, error) {
	if len(commits) == 0 {
		return nil, nil
	}
	queries := map[snapshotKey]*snapshotQuery{}
	for _, c := range commits {
		if !c.Open() {
			continue
		}
		key := snapshotKey{strings.ToLower(c.Commit.Repository), c.Commit.SHA}
		q := queries[key]
		if q == nil {
			q = &snapshotQuery{}
			queries[key] = q
		}
		required := s.targets[c.Target].RequiredChecks
		q.all = q.all || len(required) == 0
		q.names = append(q.names, required...)
	}

	reads := map[snapshotKey]readSnapshot{}
	var errs []error
	results := make([]Result, 0, len(commits))
	for _, c := range commits {
		if !c.Open() {
			results = append(results, Result{Commit: c})
			continue
		}
		key := snapshotKey{strings.ToLower(c.Commit.Repository), c.Commit.SHA}
		read, ok := reads[key]
		if !ok {
			read = s.read(ctx, c.Commit, queries[key])
			reads[key] = read
			if read.err != nil {
				errs = append(errs, read.err)
			}
		}
		if read.err != nil {
			results = append(results, Result{Commit: c})
			continue
		}
		result, err := s.settle(ctx, c, read)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.Target, err))
		}
		results = append(results, result)
	}
	return results, errors.Join(errs...)
}

func (s *Service) read(ctx context.Context, commit core.Commit, q *snapshotQuery) readSnapshot {
	var names []string
	if !q.all {
		names = uniqueNames(q.names)
	}
	readAt := s.now()
	snapshot, err := s.checks.Checks(ctx, commit.Repository, commit.SHA, names)
	if err != nil {
		s.logger.WarnContext(ctx, "reading checks failed", "repository", commit.Repository, "sha", commit.SHA, "error", err)
		return readSnapshot{err: fmt.Errorf("read checks for %s@%s: %w", commit.Repository, commit.SHA, err)}
	}
	return readSnapshot{snapshot: snapshot, readAt: readAt}
}

func (s *Service) settle(ctx context.Context, c core.TrackedCommit, read readSnapshot) (Result, error) {
	app := s.targets[c.Target]
	state, checks := Evaluate(app.RequiredChecks, read.snapshot)
	eval := core.ChecksEvaluation{State: state, Checks: checks, ReadAt: read.readAt}
	var blocked *core.Block
	settled, enqueued, err := s.commits.Settle(ctx, c.ID, eval, func(ctx context.Context, q Queue, b Blocks) (core.DeploymentID, error) {
		blocked = nil
		block, found, err := b.Blocked(ctx, c.Target, c.Commit)
		if err != nil {
			return 0, fmt.Errorf("check blocks: %w", err)
		}
		if found {
			blocked = &block
			return 0, &core.BlockedError{Block: block}
		}
		d, err := q.Enqueue(ctx, app.deployment(c))
		return d.ID, err
	})
	log := s.logger.With("app", c.Target.String(), "repository", c.Commit.Repository, "sha", c.Commit.SHA, "state", string(state))
	if err != nil {
		log.WarnContext(ctx, "settling commit checks failed", "error", err)
		return Result{Commit: c}, fmt.Errorf("settle %s: %w", c.Commit.SHA, err)
	}
	switch {
	case enqueued:
		log.InfoContext(ctx, "evaluated commit checks", "enqueued", true, "deployment_id", int64(*settled.DeploymentID))
	case blocked != nil:
		log.InfoContext(ctx, "commit is blocked; not enqueued",
			"block_id", int64(blocked.ID), "reason", blocked.Reason, "created_by", blocked.CreatedBy)
	default:
		log.InfoContext(ctx, "evaluated commit checks", "enqueued", false, "saved_state", string(settled.ChecksState))
	}
	return Result{Commit: settled, Enqueued: enqueued, Blocked: blocked}, nil
}
