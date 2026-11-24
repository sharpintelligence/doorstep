package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
)

const (
	GateStatusSinkName       = "github:statuses"
	GateStatusContextPrefix  = core.StatusContextPrefix
	DefaultGateStatusPending = 1024

	maxStatusDescriptionRunes = 140
	gateStatusPermissionHint  = "grant the GitHub App 'Commit statuses: Read and write' on the repository, or set apps[].source.statuses: false; see docs/github-app.md"
)

type GateStatusOptions struct {
	Repos      RepoClients
	Apps       map[string]bool
	Links      Links
	Logger     *slog.Logger
	Observer   Observer
	Retry      Policy
	Now        func() time.Time
	MaxPending int
}

type GateStatuses struct {
	repos      RepoClients
	apps       map[string]bool
	links      Links
	logger     *slog.Logger
	observer   Observer
	retry      Policy
	maxPending int
	warnings   *warnThrottle

	runCtx context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	wake   *sync.Cond
	latest map[statusKey]gateStatus
	order  []statusKey
	closed bool
}

type statusKey struct {
	repository string
	sha        string
	context    string
}

type gateStatus struct {
	owner      string
	repo       string
	repository string
	sha        string
	app        string
	deployment core.DeploymentID
	request    github.CommitStatusRequest
}

func NewGateStatuses(opts GateStatusOptions) (*GateStatuses, error) {
	if opts.Repos == nil {
		return nil, errors.New("notify: github commit statuses need a repository client source")
	}
	g := &GateStatuses{
		repos:      opts.Repos,
		apps:       make(map[string]bool, len(opts.Apps)),
		links:      opts.Links,
		logger:     opts.Logger,
		observer:   opts.Observer,
		retry:      opts.Retry.withDefaults(),
		maxPending: opts.MaxPending,
		done:       make(chan struct{}),
		latest:     make(map[statusKey]gateStatus),
	}
	for app, enabled := range opts.Apps {
		g.apps[app] = enabled
	}
	if g.logger == nil {
		g.logger = slog.New(slog.DiscardHandler)
	}
	if g.observer == nil {
		g.observer = nopObserver{}
	}
	if g.maxPending <= 0 {
		g.maxPending = DefaultGateStatusPending
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	g.warnings = newWarnThrottle(permissionWarnInterval, now)
	g.wake = sync.NewCond(&g.mu)
	g.runCtx, g.cancel = context.WithCancel(context.Background())
	go g.run()
	return g, nil
}

func (g *GateStatuses) GateRecorded(ctx context.Context, d core.Deployment, r core.GateResult) error {
	if st, ok := g.status(d, r); ok {
		g.enqueue(ctx, st)
	}
	return nil
}

func (g *GateStatuses) status(d core.Deployment, r core.GateResult) (gateStatus, bool) {
	if !g.apps[d.Target.App] || d.Commit.Validate() != nil || !r.Phase.Valid() || strings.TrimSpace(r.Name) == "" {
		return gateStatus{}, false
	}
	owner, repo, ok := splitRepository(d.Commit.Repository)
	if !ok {
		return gateStatus{}, false
	}
	state, ok := commitStatusState(r.Status)
	if !ok {
		return gateStatus{}, false
	}
	return gateStatus{
		owner:      owner,
		repo:       repo,
		repository: d.Commit.Repository,
		sha:        d.Commit.SHA,
		app:        d.Target.App,
		deployment: d.ID,
		request: github.CommitStatusRequest{
			State:       state,
			Context:     GateStatusContext(d.Target.App, r.Phase, r.Name),
			Description: gateStatusDescription(d, r),
			TargetURL:   g.links.deployment(&d),
		},
	}, true
}

func (g *GateStatuses) enqueue(ctx context.Context, st gateStatus) {
	key := statusKey{repository: strings.ToLower(st.repository), sha: st.sha, context: st.request.Context}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		g.drop(ctx, st, "mirror is closed")
		return
	}
	if _, queued := g.latest[key]; !queued {
		if len(g.order) >= g.maxPending {
			g.mu.Unlock()
			g.drop(ctx, st, "queue is full")
			return
		}
		g.order = append(g.order, key)
	}
	g.latest[key] = st
	g.wake.Signal()
	g.mu.Unlock()
}

func (g *GateStatuses) Close(ctx context.Context) error {
	g.mu.Lock()
	g.closed = true
	g.wake.Broadcast()
	g.mu.Unlock()

	select {
	case <-g.done:
		g.cancel()
		return nil
	case <-ctx.Done():
		g.cancel()
		<-g.done
		return fmt.Errorf("gate commit statuses were not drained before the deadline: %w", ctx.Err())
	}
}

func (g *GateStatuses) run() {
	defer close(g.done)
	for {
		st, ok := g.next()
		if !ok {
			return
		}
		if g.runCtx.Err() != nil {
			g.drop(g.runCtx, st, "mirror stopped before delivery")
			continue
		}
		g.deliver(g.runCtx, st)
	}
}

func (g *GateStatuses) next() (gateStatus, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for len(g.order) == 0 && !g.closed {
		g.wake.Wait()
	}
	if len(g.order) == 0 {
		return gateStatus{}, false
	}
	key := g.order[0]
	g.order = g.order[1:]
	st := g.latest[key]
	delete(g.latest, key)
	return st, true
}

func (g *GateStatuses) deliver(ctx context.Context, st gateStatus) {
	attempts, err := g.retry.Do(ctx, func(ctx context.Context) error { return g.send(ctx, st) })
	attrs := append(gateStatusAttrs(st), slog.Int("attempts", attempts))
	if err == nil {
		g.observer.Delivered(TypeGitHub, ResultSent)
		g.logger.LogAttrs(ctx, slog.LevelDebug, "gate commit status sent", attrs...)
		return
	}
	g.observer.Delivered(TypeGitHub, ResultFailed)
	level := slog.LevelWarn
	if isReported(err) {
		level = slog.LevelDebug
	}
	g.logger.LogAttrs(ctx, level, "gate commit status failed", append(attrs, slog.Any("error", err))...)
}

func (g *GateStatuses) send(ctx context.Context, st gateStatus) error {
	client, err := g.repos.RepoClient(ctx, st.owner, st.repo)
	if err != nil {
		return g.classify(ctx, st.repository, err)
	}
	if err := client.CreateCommitStatus(ctx, st.owner, st.repo, st.sha, st.request); err != nil {
		return g.classify(ctx, st.repository, err)
	}
	return nil
}

func (g *GateStatuses) classify(ctx context.Context, repository string, err error) error {
	return classifyGitHubError(err, func(err error) {
		if g.warnings.allow(repository) {
			g.logger.WarnContext(ctx, "cannot mirror gates to github",
				slog.String("repository", repository),
				slog.String("hint", gateStatusPermissionHint),
				slog.Any("error", err),
			)
		}
	})
}

func (g *GateStatuses) drop(ctx context.Context, st gateStatus, reason string) {
	g.observer.Delivered(TypeGitHub, ResultDropped)
	g.logger.LogAttrs(ctx, slog.LevelWarn, "gate commit status dropped", append(gateStatusAttrs(st), slog.String("reason", reason))...)
}

func gateStatusAttrs(st gateStatus) []slog.Attr {
	return []slog.Attr{
		slog.String("sink", GateStatusSinkName),
		slog.String("app", st.app),
		slog.Int64("deployment_id", int64(st.deployment)),
		slog.String("repository", st.repository),
		slog.String("sha", st.sha),
		slog.String("context", st.request.Context),
		slog.String("state", string(st.request.State)),
	}
}

func GateStatusContext(app string, phase core.GatePhase, gate string) string {
	return GateStatusContextPrefix + app + "/" + string(phase) + "/" + gate
}

func commitStatusState(status core.GateStatus) (github.CommitStatusState, bool) {
	switch status {
	case core.GatePending:
		return github.CommitStatusPending, true
	case core.GatePassed, core.GateSkipped:
		return github.CommitStatusSuccess, true
	case core.GateFailed:
		return github.CommitStatusFailure, true
	}
	return "", false
}

func gateStatusDescription(d core.Deployment, r core.GateResult) string {
	description := strings.TrimSpace(r.Message)
	if description == "" {
		description = string(r.Status)
	}
	if d.Kind == core.KindRollback {
		description = "Rollback " + deploymentRef(d.ID) + ": " + description
	}
	if utf8.RuneCountInString(description) <= maxStatusDescriptionRunes {
		return description
	}
	return string([]rune(description)[:maxStatusDescriptionRunes-1]) + "…"
}
