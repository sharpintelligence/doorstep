package argocd

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/url"
	"strconv"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/kube"
)

const (
	DefaultSyncTimeout    = 10 * time.Minute
	DefaultResyncInterval = 30 * time.Second

	watchGrace     = 5 * time.Second
	initialBackoff = time.Second
	maxBackoff     = 30 * time.Second
	apiGroupPath   = "/apis/argoproj.io/v1alpha1/namespaces"
)

type Application struct {
	Namespace   string
	Name        string
	Repository  string
	SyncTimeout time.Duration
}

func (a Application) String() string {
	return a.Namespace + "/" + a.Name
}

type Ancestry interface {
	Contains(ctx context.Context, repository, ancestor, descendant string) (bool, error)
}

type API interface {
	Get(ctx context.Context, path string, query url.Values, out any) error
	Stream(ctx context.Context, path string, query url.Values) (io.ReadCloser, error)
}

type Options struct {
	Client         API
	Applications   map[core.Target]Application
	Ancestry       Ancestry
	ResyncInterval time.Duration
	Wait           func(ctx context.Context, d time.Duration) error
	Now            func() time.Time
	Logger         *slog.Logger
}

type Watcher struct {
	client   API
	apps     map[core.Target]Application
	ancestry Ancestry
	resync   time.Duration
	wait     func(ctx context.Context, d time.Duration) error
	now      func() time.Time
	logger   *slog.Logger
}

func New(opts Options) (*Watcher, error) {
	if opts.Client == nil {
		return nil, errors.New("argocd: a kubernetes client is required")
	}
	if opts.ResyncInterval < 0 {
		return nil, fmt.Errorf("argocd: resync interval must not be negative, got %s", opts.ResyncInterval)
	}
	apps := make(map[core.Target]Application, len(opts.Applications))
	for target, app := range opts.Applications {
		if err := target.Validate(); err != nil {
			return nil, fmt.Errorf("argocd: %w", err)
		}
		switch {
		case app.Namespace == "" || app.Name == "":
			return nil, fmt.Errorf("argocd: application for %s needs a namespace and a name", target)
		case app.SyncTimeout < 0:
			return nil, fmt.Errorf("argocd: sync timeout for %s must be positive, got %s", target, app.SyncTimeout)
		case app.SyncTimeout == 0:
			app.SyncTimeout = DefaultSyncTimeout
		}
		apps[target] = app
	}
	w := &Watcher{
		client:   opts.Client,
		apps:     apps,
		ancestry: opts.Ancestry,
		resync:   opts.ResyncInterval,
		wait:     opts.Wait,
		now:      opts.Now,
		logger:   opts.Logger,
	}
	if w.resync == 0 {
		w.resync = DefaultResyncInterval
	}
	if w.wait == nil {
		w.wait = sleep
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.logger == nil {
		w.logger = slog.New(slog.DiscardHandler)
	}
	return w, nil
}

func (w *Watcher) Application(t core.Target) (Application, bool) {
	app, ok := w.apps[t]
	return app, ok
}

func (w *Watcher) Health(ctx context.Context, t core.Target) (core.Health, error) {
	status, err := w.read(ctx, t, "")
	return status.Health, err
}

func (w *Watcher) Status(ctx context.Context, t core.Target) (core.SyncStatus, error) {
	return w.read(ctx, t, "")
}

func (w *Watcher) Check(ctx context.Context, t core.Target, revision string) (core.SyncStatus, error) {
	if err := core.ValidateCommitSHA(revision); err != nil {
		return core.SyncStatus{}, fmt.Errorf("argocd: %w", err)
	}
	return w.read(ctx, t, revision)
}

func (w *Watcher) read(ctx context.Context, t core.Target, pin string) (core.SyncStatus, error) {
	app, ok := w.apps[t]
	if !ok {
		return core.SyncStatus{}, unknownTarget(t)
	}
	var obj application
	if err := w.client.Get(ctx, applicationPath(app), nil, &obj); err != nil {
		return apiFailure(app, err)
	}
	contains := newContainment(w, app, pin)
	return assess(obj, app.Repository, pin, contains.check(ctx), w.now()), nil
}

func (w *Watcher) WaitSynced(ctx context.Context, t core.Target, revision string) (core.SyncStatus, error) {
	if err := core.ValidateCommitSHA(revision); err != nil {
		return core.SyncStatus{}, fmt.Errorf("argocd: %w", err)
	}
	app, ok := w.apps[t]
	if !ok {
		return core.SyncStatus{}, unknownTarget(t)
	}
	waitCtx, cancel := context.WithTimeoutCause(ctx, app.SyncTimeout, core.ErrSyncTimeout)
	defer cancel()
	run := &waitRun{watcher: w, app: app, target: t, pin: revision, contains: newContainment(w, app, revision)}
	status, err := run.loop(waitCtx)
	switch {
	case err == nil:
		return status, nil
	case ctx.Err() != nil:
		return run.last, ctx.Err()
	case errors.Is(context.Cause(waitCtx), core.ErrSyncTimeout):
		return run.timedOut(waitCtx)
	default:
		return status, err
	}
}

type waitRun struct {
	watcher  *Watcher
	app      Application
	target   core.Target
	pin      string
	contains *containment

	last     core.SyncStatus
	observed bool
	lastErr  error
	reads    int
	watches  int
	revision string
	reached  bool
}

func (r *waitRun) loop(ctx context.Context) (core.SyncStatus, error) {
	for {
		var obj application
		if err := r.watcher.client.Get(ctx, applicationPath(r.app), nil, &obj); err != nil {
			if ctx.Err() != nil {
				return r.last, ctx.Err()
			}
			if kube.IsNotFound(err) || kube.IsForbidden(err) {
				return apiFailure(r.app, err)
			}
			r.lastErr = err
			r.watcher.logger.WarnContext(ctx, "argocd application read failed", r.attrs(slog.String("error", err.Error()))...)
			r.reads++
			if err := r.watcher.wait(ctx, backoff(r.reads)); err != nil {
				return r.last, err
			}
			continue
		}
		r.reads = 0
		if status, done, err := r.observe(ctx, obj); done {
			return status, err
		}
		status, done, err := r.watch(ctx, obj.Metadata.ResourceVersion)
		if done {
			return status, err
		}
	}
}

func (r *waitRun) observe(ctx context.Context, obj application) (core.SyncStatus, bool, error) {
	status := assess(obj, r.app.Repository, r.pin, r.contains.check(ctx), r.watcher.now())
	r.last, r.observed = status, true
	r.logTransitions(ctx, status)
	switch {
	case status.Ready():
		r.watcher.logger.InfoContext(ctx, "argocd application synced and healthy", r.attrs(slog.String("argocd_revision", status.Revision))...)
		return status, true, nil
	case status.Failure != nil && status.Failure.Reason == core.SyncReasonFailed:
		r.watcher.logger.WarnContext(ctx, "argocd sync operation failed", r.attrs(slog.String("argocd_revision", status.Revision), slog.String("message", status.Failure.Message))...)
		return status, true, fmt.Errorf("argocd: %w for %s at %s: %s", core.ErrSyncFailed, r.app, status.Revision, status.Failure.Message)
	default:
		return status, false, nil
	}
}

func (r *waitRun) watch(ctx context.Context, resourceVersion string) (core.SyncStatus, bool, error) {
	streamCtx, cancel := context.WithTimeout(ctx, r.watcher.resync+watchGrace)
	defer cancel()
	query := url.Values{
		"watch":               {"1"},
		"fieldSelector":       {"metadata.name=" + r.app.Name},
		"resourceVersion":     {resourceVersion},
		"timeoutSeconds":      {strconv.Itoa(max(1, int(r.watcher.resync.Round(time.Second)/time.Second)))},
		"allowWatchBookmarks": {"true"},
	}
	started := r.watcher.now()
	stream, err := r.watcher.client.Stream(streamCtx, collectionPath(r.app), query)
	if err != nil {
		if ctx.Err() != nil {
			return r.last, true, ctx.Err()
		}
		r.watcher.logger.DebugContext(ctx, "argocd watch failed; polling", r.attrs(slog.String("error", err.Error()))...)
		if kube.IsForbidden(err) {
			return r.retryAfter(ctx, r.watcher.resync)
		}
		return r.watchFailed(ctx)
	}
	defer func() { _ = stream.Close() }()
	decoder := json.NewDecoder(stream)
	for {
		var event watchEvent
		if err := decoder.Decode(&event); err != nil {
			if ctx.Err() != nil {
				return r.last, true, ctx.Err()
			}
			if streamCtx.Err() != nil || (errors.Is(err, io.EOF) && r.watcher.now().Sub(started) >= r.watcher.resync/2) {
				r.watches = 0
				return r.last, false, nil
			}
			return r.watchFailed(ctx)
		}
		switch event.Type {
		case "ADDED", "MODIFIED":
			r.watches = 0
			var obj application
			if err := json.Unmarshal(event.Object, &obj); err != nil || obj.Metadata.Name != r.app.Name {
				continue
			}
			if status, done, err := r.observe(ctx, obj); done {
				return status, true, err
			}
		case "DELETED":
			return r.last, false, nil
		case "ERROR":
			status := kube.ParseStatus(0, event.Object)
			r.watcher.logger.DebugContext(ctx, "argocd watch expired; reading again", r.attrs(slog.String("error", status.Error()))...)
			if kube.IsGone(status) && r.watches == 0 {
				r.watches++
				return r.last, false, nil
			}
			return r.watchFailed(ctx)
		}
	}
}

func (r *waitRun) retryAfter(ctx context.Context, d time.Duration) (core.SyncStatus, bool, error) {
	if err := r.watcher.wait(ctx, d); err != nil {
		return r.last, true, err
	}
	return r.last, false, nil
}

func (r *waitRun) watchFailed(ctx context.Context) (core.SyncStatus, bool, error) {
	r.watches++
	return r.retryAfter(ctx, min(backoff(r.watches), r.watcher.resync))
}

func (r *waitRun) timedOut(ctx context.Context) (core.SyncStatus, error) {
	status := r.last
	timeout := r.app.SyncTimeout
	if !r.observed {
		detail := "no response from the Kubernetes API"
		if r.lastErr != nil {
			detail = r.lastErr.Error()
		}
		status.Failure = &core.SyncFailure{
			Reason:  core.SyncReasonUnavailable,
			Class:   core.SyncReasonUnavailable.Class(true),
			Message: cut("could not read application " + r.app.String() + ": " + detail),
		}
		r.watcher.logger.WarnContext(ctx, "argocd sync timed out", r.attrs(slog.String("reason", string(status.Failure.Reason)))...)
		return status, fmt.Errorf("argocd: %w after %s: %w: %s", core.ErrSyncTimeout, timeout, core.ErrSyncUnavailable, status.Failure.Message)
	}
	if status.Failure == nil {
		status.Failure = &core.SyncFailure{Reason: core.SyncReasonProgressing, Class: core.FailureUnknown, Message: "application did not settle"}
	}
	r.watcher.logger.WarnContext(ctx, "argocd sync timed out", r.attrs(
		slog.String("reason", string(status.Failure.Reason)),
		slog.String("argocd_revision", status.Revision),
		slog.String("message", status.Failure.Message),
	)...)
	return status, fmt.Errorf("argocd: %w after %s: %s", core.ErrSyncTimeout, timeout, status.Failure.Message)
}

func (r *waitRun) logTransitions(ctx context.Context, status core.SyncStatus) {
	if status.Revision != r.revision {
		r.revision = status.Revision
		r.watcher.logger.DebugContext(ctx, "argocd revision observed", r.attrs(slog.String("argocd_revision", status.Revision))...)
	}
	if status.Reached && !r.reached {
		r.reached = true
		r.watcher.logger.InfoContext(ctx, "argocd application reached revision", r.attrs(slog.String("argocd_revision", status.Revision))...)
	}
}

func (r *waitRun) attrs(extra ...any) []any {
	return append([]any{
		slog.String("app", r.target.App),
		slog.String("environment", r.target.Environment),
		slog.String("namespace", r.app.Namespace),
		slog.String("application", r.app.Name),
		slog.String("revision", r.pin),
	}, extra...)
}

type containment struct {
	watcher *Watcher
	app     Application
	pin     string
	known   map[string]bool
}

func newContainment(w *Watcher, app Application, pin string) *containment {
	return &containment{watcher: w, app: app, pin: pin, known: make(map[string]bool)}
}

func (c *containment) check(ctx context.Context) func(string) bool {
	if c.watcher.ancestry == nil || c.app.Repository == "" {
		return nil
	}
	return func(revision string) bool {
		if contained, ok := c.known[revision]; ok {
			return contained
		}
		contained, err := c.watcher.ancestry.Contains(ctx, c.app.Repository, c.pin, revision)
		if err != nil {
			if ctx.Err() == nil {
				c.watcher.logger.WarnContext(ctx, "argocd revision ancestry check failed",
					slog.String("namespace", c.app.Namespace),
					slog.String("application", c.app.Name),
					slog.String("revision", c.pin),
					slog.String("argocd_revision", revision),
					slog.String("error", err.Error()),
				)
			}
			return false
		}
		c.known[revision] = contained
		return contained
	}
}

func apiFailure(app Application, err error) (core.SyncStatus, error) {
	var reason core.SyncFailureReason
	switch {
	case kube.IsNotFound(err):
		reason, err = core.SyncReasonNotFound, fmt.Errorf("%w: %w", core.ErrSyncTargetNotFound, err)
	case kube.IsForbidden(err):
		reason = core.SyncReasonForbidden
	default:
		return core.SyncStatus{}, fmt.Errorf("argocd: read application %s: %w", app, err)
	}
	status := core.SyncStatus{Failure: &core.SyncFailure{
		Reason:  reason,
		Class:   reason.Class(true),
		Message: cut("application " + app.String() + ": " + err.Error()),
	}}
	return status, fmt.Errorf("argocd: application %s: %w", app, err)
}

func unknownTarget(t core.Target) error {
	return fmt.Errorf("argocd: %w: no Argo CD application is configured for %s", core.ErrSyncTargetNotFound, t)
}

func applicationPath(app Application) string {
	return collectionPath(app) + "/" + url.PathEscape(app.Name)
}

func collectionPath(app Application) string {
	return apiGroupPath + "/" + url.PathEscape(app.Namespace) + "/applications"
}

func backoff(attempt int) time.Duration {
	d := maxBackoff
	if shift := attempt - 1; shift < 16 {
		d = min(initialBackoff<<max(shift, 0), maxBackoff)
	}
	jitter, err := rand.Int(rand.Reader, big.NewInt(int64(d/2)+1))
	if err != nil {
		return d
	}
	return d/2 + time.Duration(jitter.Int64())
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
