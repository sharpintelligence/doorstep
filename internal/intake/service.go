package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sharpintelligence/doorstep/internal/core"
)

var zeroSHA = strings.Repeat("0", 40)

type Options struct {
	Apps    []App
	Commits Commits
	Checks  CheckSource
	Logger  *slog.Logger
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
}

type Service struct {
	apps    []App
	targets map[core.Target]App
	commits Commits
	checks  CheckSource
	logger  *slog.Logger
}

func New(opts Options) (*Service, error) {
	if opts.Commits == nil {
		return nil, errors.New("intake: commits store is required")
	}
	if opts.Checks == nil {
		return nil, errors.New("intake: check source is required")
	}
	s := &Service{
		targets: make(map[core.Target]App, len(opts.Apps)),
		commits: opts.Commits,
		checks:  opts.Checks,
		logger:  opts.Logger,
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
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
		c, err := s.commits.Track(ctx, core.TrackedCommit{
			Target:      app.Target,
			Commit:      core.Commit{Repository: app.Repository, SHA: p.SHA},
			Branch:      app.Branch,
			Source:      p.Source,
			PushedBy:    p.PushedBy,
			ChecksState: core.ChecksPending,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", app.Target, err))
			continue
		}
		s.logger.InfoContext(ctx, "tracking commit", "app", app.Target.String(), "repository", app.Repository, "sha", p.SHA, "branch", app.Branch)
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
		if app, ok := s.targets[c.Target]; ok && c.Open() && strings.EqualFold(app.Repository, c.Commit.Repository) {
			open = append(open, c)
		}
	}
	return s.evaluate(ctx, open)
}

func (s *Service) Pending(ctx context.Context, limit int) ([]core.TrackedCommit, error) {
	return s.commits.Pending(ctx, limit)
}

type snapshotKey struct {
	repository string
	sha        string
}

func (s *Service) evaluate(ctx context.Context, commits []core.TrackedCommit) ([]Result, error) {
	if len(commits) == 0 {
		return nil, nil
	}
	withSuites := map[snapshotKey]bool{}
	for _, c := range commits {
		if c.Open() {
			key := snapshotKey{strings.ToLower(c.Commit.Repository), c.Commit.SHA}
			withSuites[key] = withSuites[key] || len(s.targets[c.Target].RequiredChecks) == 0
		}
	}

	snapshots := map[snapshotKey]Snapshot{}
	failed := map[snapshotKey]bool{}
	var errs []error
	results := make([]Result, 0, len(commits))
	for _, c := range commits {
		if !c.Open() {
			results = append(results, Result{Commit: c})
			continue
		}
		key := snapshotKey{strings.ToLower(c.Commit.Repository), c.Commit.SHA}
		snapshot, ok := snapshots[key]
		if !ok && !failed[key] {
			var err error
			snapshot, err = s.checks.Checks(ctx, c.Commit.Repository, c.Commit.SHA, withSuites[key])
			if err != nil {
				failed[key] = true
				errs = append(errs, fmt.Errorf("read checks for %s@%s: %w", c.Commit.Repository, c.Commit.SHA, err))
				s.logger.WarnContext(ctx, "reading checks failed", "repository", c.Commit.Repository, "sha", c.Commit.SHA, "error", err)
			} else {
				snapshots[key] = snapshot
			}
		}
		if failed[key] {
			results = append(results, Result{Commit: c})
			continue
		}
		result, err := s.settle(ctx, c, snapshot)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.Target, err))
		}
		results = append(results, result)
	}
	return results, errors.Join(errs...)
}

func (s *Service) settle(ctx context.Context, c core.TrackedCommit, snapshot Snapshot) (Result, error) {
	app := s.targets[c.Target]
	state, checks := Evaluate(app.RequiredChecks, snapshot)
	saved, err := s.commits.SaveChecks(ctx, c.ID, state, checks)
	if err != nil {
		return Result{Commit: c}, fmt.Errorf("save checks for %s: %w", c.Commit.SHA, err)
	}
	log := s.logger.With("app", c.Target.String(), "repository", c.Commit.Repository, "sha", c.Commit.SHA, "state", string(saved.ChecksState))
	if saved.ChecksState != core.ChecksSuccess || !saved.Open() {
		log.InfoContext(ctx, "evaluated commit checks", "enqueued", false)
		return Result{Commit: saved}, nil
	}

	promoted, enqueued, err := s.commits.Promote(ctx, saved.ID, func(ctx context.Context, q Queue) (core.DeploymentID, error) {
		d, err := q.Enqueue(ctx, app.deployment(saved))
		return d.ID, err
	})
	if err != nil {
		log.WarnContext(ctx, "enqueueing deployment failed", "error", err)
		return Result{Commit: saved}, fmt.Errorf("enqueue %s: %w", c.Commit.SHA, err)
	}
	if enqueued {
		log.InfoContext(ctx, "evaluated commit checks", "enqueued", true, "deployment_id", int64(*promoted.DeploymentID))
	} else {
		log.InfoContext(ctx, "evaluated commit checks", "enqueued", false)
	}
	return Result{Commit: promoted, Enqueued: enqueued}, nil
}
