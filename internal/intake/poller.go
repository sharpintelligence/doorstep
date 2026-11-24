package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	PollOpenLimit = 100
	PollerActor   = "doorstep-poller"
	MaxPollTick   = 15 * time.Second
)

type Opener interface {
	Open(ctx context.Context, limit int) ([]core.TrackedCommit, error)
}

type PollIntake interface {
	Pusher
	Opener
}

type PollClaims interface {
	ClaimPoll(ctx context.Context, every time.Duration) (bool, error)
}

type PollerOptions struct {
	Apps     []App
	Intake   PollIntake
	Branches Branches
	Claims   PollClaims
	Interval time.Duration
	Observer Observer
	Logger   *slog.Logger
	Now      func() time.Time
}

type polledBranch struct {
	repository string
	branch     string
}

type Poller struct {
	branches []polledBranch
	intake   PollIntake
	source   Branches
	claims   PollClaims
	interval time.Duration
	observer Observer
	logger   *slog.Logger
	now      func() time.Time

	mu       sync.Mutex
	lastSeen map[string]string
}

func NewPoller(opts PollerOptions) (*Poller, error) {
	switch {
	case opts.Intake == nil:
		return nil, errors.New("intake: poller intake is required")
	case opts.Branches == nil:
		return nil, errors.New("intake: poller branch source is required")
	case opts.Claims == nil:
		return nil, errors.New("intake: poller claims are required")
	case opts.Interval <= 0:
		return nil, fmt.Errorf("intake: poll interval must be positive, got %s", opts.Interval)
	}
	p := &Poller{
		intake:   opts.Intake,
		source:   opts.Branches,
		claims:   opts.Claims,
		interval: opts.Interval,
		observer: opts.Observer,
		logger:   opts.Logger,
		now:      opts.Now,
		lastSeen: map[string]string{},
	}
	if p.observer == nil {
		p.observer = nopObserver{}
	}
	if p.logger == nil {
		p.logger = slog.New(slog.DiscardHandler)
	}
	if p.now == nil {
		p.now = time.Now
	}
	seen := map[string]bool{}
	for _, app := range opts.Apps {
		key := branchKey(app.Repository, app.Branch)
		if seen[key] {
			continue
		}
		seen[key] = true
		p.branches = append(p.branches, polledBranch{repository: app.Repository, branch: app.Branch})
	}
	return p, nil
}

func (p *Poller) Tick() time.Duration {
	return min(p.interval, MaxPollTick)
}

func (p *Poller) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.Tick())
	defer ticker.Stop()
	for {
		if err := p.RunOnce(ctx); err != nil && ctx.Err() == nil {
			p.logger.ErrorContext(ctx, "intake poll failed", slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (p *Poller) RunOnce(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	claimed, err := p.claims.ClaimPoll(ctx, p.interval)
	if err != nil {
		return fmt.Errorf("claim intake poll: %w", err)
	}
	if !claimed {
		return nil
	}
	started := p.now()
	err = p.cycle(ctx)
	result := PollSucceeded
	if err != nil {
		result = PollFailed
	}
	p.observer.PollCycle(result)
	p.logger.DebugContext(ctx, "intake poll cycle finished",
		slog.String("result", string(result)),
		slog.Duration("duration", p.now().Sub(started)),
	)
	return err
}

func (p *Poller) cycle(ctx context.Context) error {
	evaluated := map[snapshotKey]bool{}
	var errs []error
	for _, b := range p.branches {
		if err := p.pollBranch(ctx, b, evaluated); err != nil {
			errs = append(errs, err)
		}
	}
	open, err := p.intake.Open(ctx, PollOpenLimit)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("list open commits: %w", err))...)
	}
	fresh := p.now().Add(-p.interval / 2)
	for _, c := range open {
		key := snapshotKey{strings.ToLower(c.Commit.Repository), c.Commit.SHA}
		if evaluated[key] {
			continue
		}
		evaluated[key] = true
		if c.EvaluatedAt != nil && c.EvaluatedAt.After(fresh) {
			continue
		}
		if _, err := p.intake.Reevaluate(ctx, c.Commit.Repository, c.Commit.SHA); err != nil {
			errs = append(errs, fmt.Errorf("re-evaluate %s@%s: %w", c.Commit.Repository, c.Commit.SHA, err))
		}
	}
	return errors.Join(errs...)
}

func (p *Poller) pollBranch(ctx context.Context, b polledBranch, evaluated map[snapshotKey]bool) error {
	head, err := p.source.BranchHead(ctx, b.repository, b.branch)
	if err != nil {
		return fmt.Errorf("read head of %s branch %s: %w", b.repository, b.branch, err)
	}
	log := p.logger.With(slog.String("repository", b.repository), slog.String("branch", b.branch))
	if head == "" {
		log.WarnContext(ctx, "polled branch does not exist")
		return nil
	}
	key := branchKey(b.repository, b.branch)
	if p.lastSeen[key] == head {
		return nil
	}
	evaluated[snapshotKey{strings.ToLower(b.repository), head}] = true
	results, err := p.intake.HandlePush(ctx, Push{
		Repository: b.repository,
		Branch:     b.branch,
		SHA:        head,
		Source:     core.SourcePoll,
		PushedBy:   PollerActor,
	})
	if err != nil {
		return fmt.Errorf("handle polled head %s of %s branch %s: %w", head, b.repository, b.branch, err)
	}
	p.lastSeen[key] = head
	log.DebugContext(ctx, "polled branch head handled", slog.String("sha", head), slog.Int("tracked", len(results)))
	return nil
}

func branchKey(repository, branch string) string {
	return strings.ToLower(repository) + "\x00" + branch
}
