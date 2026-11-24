package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
)

const (
	GitHubSinkName = "github:deployments"

	githubTask             = "deploy"
	defaultCacheSize       = 1024
	permissionWarnInterval = time.Hour
	permissionHint         = "grant the GitHub App 'Deployments: Read and write' on the repository, or set apps[].source.deployments: false; see docs/github-app.md"
)

type RepoClients interface {
	RepoClient(ctx context.Context, owner, repo string) (*github.Client, error)
}

type GitHubOptions struct {
	Repos     RepoClients
	Apps      map[string]bool
	Links     Links
	Logger    *slog.Logger
	Now       func() time.Time
	CacheSize int
}

type GitHub struct {
	repos     RepoClients
	apps      map[string]bool
	links     Links
	logger    *slog.Logger
	now       func() time.Time
	cacheSize int

	warnings *warnThrottle

	mu  sync.Mutex
	ids map[string]int64
}

type githubPayload struct {
	DoorstepDeploymentID int64  `json:"doorstep_deployment_id"`
	Kind                 string `json:"kind"`
	RollbackOfID         *int64 `json:"rollback_of_id,omitempty"`
}

func NewGitHub(opts GitHubOptions) (*GitHub, error) {
	if opts.Repos == nil {
		return nil, errors.New("notify: github sink needs a repository client source")
	}
	g := &GitHub{
		repos:     opts.Repos,
		apps:      make(map[string]bool, len(opts.Apps)),
		links:     opts.Links,
		logger:    opts.Logger,
		now:       opts.Now,
		cacheSize: opts.CacheSize,
		ids:       make(map[string]int64),
	}
	for app, enabled := range opts.Apps {
		g.apps[app] = enabled
	}
	if g.logger == nil {
		g.logger = slog.New(slog.DiscardHandler)
	}
	if g.now == nil {
		g.now = time.Now
	}
	if g.cacheSize <= 0 {
		g.cacheSize = defaultCacheSize
	}
	g.warnings = newWarnThrottle(permissionWarnInterval, g.now)
	return g, nil
}

func (g *GitHub) Name() string { return GitHubSinkName }

func (g *GitHub) Type() string { return TypeGitHub }

func (g *GitHub) Accepts(n core.Notification) bool {
	if _, ok := githubState(n.Kind); !ok || !g.apps[n.Target.App] || n.Deployment == nil || n.Deployment.ID == 0 {
		return false
	}
	if n.Deployment.Commit.Validate() != nil {
		return false
	}
	_, _, ok := splitRepository(n.Deployment.Commit.Repository)
	return ok
}

func (g *GitHub) Prepare(n core.Notification) Delivery {
	return func(ctx context.Context) error { return g.report(ctx, n) }
}

func (g *GitHub) report(ctx context.Context, n core.Notification) error {
	state, ok := githubState(n.Kind)
	if !ok {
		return nil
	}
	d := n.Deployment
	repository := d.Commit.Repository
	owner, repo, ok := splitRepository(repository)
	if !ok {
		return permanent(fmt.Errorf("%w: %q", github.ErrInvalidRepository, repository))
	}
	client, err := g.repos.RepoClient(ctx, owner, repo)
	if err != nil {
		return g.classify(ctx, repository, err)
	}
	key := cacheKey(repository, d.ID)
	id, err := g.deploymentID(ctx, client, owner, repo, key, n)
	if err != nil {
		return g.classify(ctx, repository, err)
	}
	err = client.CreateDeploymentStatus(ctx, owner, repo, id, github.DeploymentStatusRequest{
		State:        state,
		Description:  statusDescription(n),
		LogURL:       g.links.deployment(d),
		AutoInactive: true,
	})
	if err != nil || isTerminal(n.Kind) {
		g.forget(key)
	}
	if err != nil {
		return g.classify(ctx, repository, err)
	}
	return nil
}

func (g *GitHub) deploymentID(ctx context.Context, client *github.Client, owner, repo, key string, n core.Notification) (int64, error) {
	if id, ok := g.cached(key); ok {
		return id, nil
	}
	d := n.Deployment
	existing, err := client.ListDeployments(ctx, owner, repo, github.DeploymentFilter{
		SHA:         d.Commit.SHA,
		Environment: n.Target.App,
		Task:        githubTask,
	})
	if err != nil {
		return 0, err
	}
	for _, e := range existing {
		var payload githubPayload
		if json.Unmarshal(e.Payload, &payload) == nil && payload.DoorstepDeploymentID == int64(d.ID) {
			g.remember(key, e.ID)
			return e.ID, nil
		}
	}
	payload := githubPayload{DoorstepDeploymentID: int64(d.ID), Kind: string(d.Kind)}
	if rollbackOf, ok := rollbackOfID(n); ok {
		payload.RollbackOfID = new(int64(rollbackOf))
	}
	created, err := client.CreateDeployment(ctx, owner, repo, github.DeploymentRequest{
		Ref:         d.Commit.SHA,
		Environment: n.Target.App,
		Task:        githubTask,
		Description: deploymentDescription(n),
		Payload:     payload,
	})
	if err != nil {
		return 0, err
	}
	g.remember(key, created.ID)
	return created.ID, nil
}

func (g *GitHub) classify(ctx context.Context, repository string, err error) error {
	return classifyGitHubError(err, func(err error) {
		if g.warnings.allow(repository) {
			g.logger.WarnContext(ctx, "cannot report deployments to github",
				slog.String("repository", repository),
				slog.String("hint", permissionHint),
				slog.Any("error", err),
			)
		}
	})
}

func classifyGitHubError(err error, denied func(error)) error {
	switch {
	case isRateLimited(err):
		return err
	case errors.Is(err, github.ErrNotInstalled), errors.Is(err, github.ErrInstallationSuspended),
		hasAPIStatus(err, http.StatusForbidden), hasAPIStatus(err, http.StatusNotFound):
		denied(err)
		return reported(permanent(err))
	case errors.Is(err, github.ErrInvalidRepository), errors.Is(err, github.ErrInvalidCommitSHA),
		hasAPIStatus(err, http.StatusUnprocessableEntity), hasAPIStatus(err, http.StatusConflict):
		return permanent(err)
	default:
		return err
	}
}

type warnThrottle struct {
	interval time.Duration
	now      func() time.Time

	mu     sync.Mutex
	warned map[string]time.Time
}

func newWarnThrottle(interval time.Duration, now func() time.Time) *warnThrottle {
	return &warnThrottle{interval: interval, now: now, warned: make(map[string]time.Time)}
}

func (w *warnThrottle) allow(key string) bool {
	key = strings.ToLower(key)
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if last, seen := w.warned[key]; seen && now.Sub(last) < w.interval {
		return false
	}
	w.warned[key] = now
	return true
}

func (g *GitHub) cached(key string) (int64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	id, ok := g.ids[key]
	return id, ok
}

func (g *GitHub) remember(key string, id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.ids[key]; !ok && len(g.ids) >= g.cacheSize {
		for evict := range g.ids {
			delete(g.ids, evict)
			break
		}
	}
	g.ids[key] = id
}

func (g *GitHub) forget(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.ids, key)
}

func (g *GitHub) cacheLen() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.ids)
}

func cacheKey(repository string, id core.DeploymentID) string {
	return fmt.Sprintf("%s#%d", strings.ToLower(repository), id)
}

func hasAPIStatus(err error, status int) bool {
	apiErr, ok := errors.AsType[*github.APIError](err)
	return ok && apiErr.StatusCode == status
}

func splitRepository(repository string) (string, string, bool) {
	owner, repo, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", false
	}
	return owner, repo, true
}

func githubState(kind core.NotificationKind) (github.DeploymentState, bool) {
	switch kind {
	case core.NotificationDeployQueued:
		return github.DeploymentQueued, true
	case core.NotificationDeployStarted:
		return github.DeploymentInProgress, true
	case core.NotificationDeploySucceeded, core.NotificationDeployRolledBack:
		return github.DeploymentSuccess, true
	case core.NotificationDeployFailed:
		return github.DeploymentFailure, true
	case core.NotificationDeployCancelled, core.NotificationDeploySuperseded:
		return github.DeploymentInactive, true
	case core.NotificationLockChanged, core.NotificationLockReminder:
		return "", false
	}
	return "", false
}

func isTerminal(kind core.NotificationKind) bool {
	switch kind {
	case core.NotificationDeploySucceeded, core.NotificationDeployRolledBack, core.NotificationDeployFailed,
		core.NotificationDeployCancelled, core.NotificationDeploySuperseded:
		return true
	case core.NotificationDeployQueued, core.NotificationDeployStarted, core.NotificationLockChanged, core.NotificationLockReminder:
		return false
	}
	return false
}

func deploymentDescription(n core.Notification) string {
	if id, ok := rollbackOfID(n); ok {
		return "Rollback of " + deploymentRef(id)
	}
	return "doorstep " + deploymentNoun(n.Deployment)
}

func statusDescription(n core.Notification) string {
	d := n.Deployment
	switch n.Kind {
	case core.NotificationDeployQueued:
		return "Queued"
	case core.NotificationDeployStarted:
		if d.Kind == core.KindRollback {
			return "Rolling back"
		}
		return "Deploying"
	case core.NotificationDeploySucceeded:
		return "Deployed"
	case core.NotificationDeployRolledBack:
		if id, ok := rollbackOfID(n); ok {
			return "Rolled back " + deploymentRef(id)
		}
		return "Rolled back"
	case core.NotificationDeployFailed:
		if d.Kind == core.KindRollback {
			return "Rollback failed"
		}
		return "Failed"
	case core.NotificationDeployCancelled:
		return "Cancelled"
	case core.NotificationDeploySuperseded:
		if d.SupersededByID != nil {
			return "Superseded by " + deploymentRef(*d.SupersededByID)
		}
		return "Superseded"
	case core.NotificationLockChanged, core.NotificationLockReminder:
		return ""
	}
	return ""
}
