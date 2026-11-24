package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const DefaultQueueSize = 64

const (
	TypeSlack   = "slack"
	TypeWebhook = "webhook"
	TypeGitHub  = "github"
)

type Notifier interface {
	Notify(ctx context.Context, n core.Notification)
}

type Delivery func(ctx context.Context) error

type Sink interface {
	Name() string
	Type() string
	Accepts(n core.Notification) bool
	Prepare(n core.Notification) Delivery
}

type Options struct {
	Sinks     []Sink
	Routes    map[string][]string
	Global    []Sink
	Logger    *slog.Logger
	Observer  Observer
	QueueSize int
	Retry     Policy
	Now       func() time.Time
}

type Dispatcher struct {
	logger   *slog.Logger
	observer Observer
	retry    Policy
	now      func() time.Time
	routes   map[string][]*worker
	global   []*worker
	workers  []*worker

	runCtx context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.RWMutex
	closed bool
}

type worker struct {
	sink  Sink
	queue chan core.Notification
}

var _ Notifier = (*Dispatcher)(nil)

func New(opts Options) (*Dispatcher, error) {
	queueSize := opts.QueueSize
	if queueSize <= 0 {
		queueSize = DefaultQueueSize
	}
	d := &Dispatcher{
		logger:   opts.Logger,
		observer: opts.Observer,
		retry:    opts.Retry.withDefaults(),
		now:      opts.Now,
		routes:   make(map[string][]*worker, len(opts.Routes)),
	}
	if d.logger == nil {
		d.logger = slog.New(slog.DiscardHandler)
	}
	if d.observer == nil {
		d.observer = nopObserver{}
	}
	if d.now == nil {
		d.now = time.Now
	}

	byName := make(map[string]*worker, len(opts.Sinks)+len(opts.Global))
	add := func(s Sink) (*worker, error) {
		if s == nil {
			return nil, errors.New("notify: sink must not be nil")
		}
		if _, dup := byName[s.Name()]; dup {
			return nil, fmt.Errorf("notify: duplicate sink name %q", s.Name())
		}
		w := &worker{sink: s, queue: make(chan core.Notification, queueSize)}
		byName[s.Name()] = w
		d.workers = append(d.workers, w)
		return w, nil
	}
	for _, s := range opts.Global {
		w, err := add(s)
		if err != nil {
			return nil, err
		}
		d.global = append(d.global, w)
	}
	routed := make(map[string]bool, len(opts.Sinks))
	for _, s := range opts.Sinks {
		w, err := add(s)
		if err != nil {
			return nil, err
		}
		routed[w.sink.Name()] = true
	}
	for app, names := range opts.Routes {
		seen := make(map[string]bool, len(names))
		for _, name := range names {
			if !routed[name] {
				return nil, fmt.Errorf("notify: app %q routes to unknown sink %q", app, name)
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			d.routes[app] = append(d.routes[app], byName[name])
		}
	}

	d.runCtx, d.cancel = context.WithCancel(context.Background())
	for _, w := range d.workers {
		d.wg.Go(func() { d.run(w) })
	}
	return d, nil
}

func (d *Dispatcher) Notify(ctx context.Context, n core.Notification) {
	if n.OccurredAt.IsZero() {
		n.OccurredAt = d.now()
	}
	if err := n.Validate(); err != nil {
		d.logger.ErrorContext(ctx, "invalid notification dropped",
			slog.String("kind", n.Kind.String()),
			slog.String("app", n.Target.App),
			slog.Any("error", err),
		)
		return
	}
	n = clone(n)

	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, w := range d.targets(n.Target.App) {
		if !w.sink.Accepts(n) {
			continue
		}
		if d.closed {
			d.drop(ctx, w, n, "dispatcher is closed")
			continue
		}
		select {
		case w.queue <- n:
		default:
			d.drop(ctx, w, n, "queue is full")
		}
	}
}

func (d *Dispatcher) Close(ctx context.Context) error {
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		for _, w := range d.workers {
			close(w.queue)
		}
	}
	d.mu.Unlock()

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		d.cancel()
		return nil
	case <-ctx.Done():
		d.cancel()
		<-done
		return fmt.Errorf("notifications were not drained before the deadline: %w", ctx.Err())
	}
}

func (d *Dispatcher) targets(app string) []*worker {
	routed := d.routes[app]
	targets := make([]*worker, 0, len(d.global)+len(routed))
	targets = append(targets, d.global...)
	return append(targets, routed...)
}

func (d *Dispatcher) run(w *worker) {
	ctx := d.runCtx
	for n := range w.queue {
		if ctx.Err() != nil {
			d.drop(ctx, w, n, "dispatcher stopped before delivery")
			continue
		}
		d.deliver(ctx, w, n)
	}
}

func (d *Dispatcher) deliver(ctx context.Context, w *worker, n core.Notification) {
	attempts, err := d.retry.Do(ctx, w.sink.Prepare(n))
	attrs := append(notificationAttrs(w, n), slog.Int("attempts", attempts))
	if err == nil {
		d.observer.Delivered(w.sink.Type(), ResultSent)
		d.logger.LogAttrs(ctx, slog.LevelDebug, "notification sent", attrs...)
		return
	}
	d.observer.Delivered(w.sink.Type(), ResultFailed)
	level := slog.LevelWarn
	if isReported(err) {
		level = slog.LevelDebug
	}
	d.logger.LogAttrs(ctx, level, "notification failed", append(attrs, slog.Any("error", err))...)
}

func (d *Dispatcher) drop(ctx context.Context, w *worker, n core.Notification, reason string) {
	d.observer.Delivered(w.sink.Type(), ResultDropped)
	d.logger.LogAttrs(ctx, slog.LevelWarn, "notification dropped", append(notificationAttrs(w, n), slog.String("reason", reason))...)
}

func notificationAttrs(w *worker, n core.Notification) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("sink", w.sink.Name()),
		slog.String("sink_type", w.sink.Type()),
		slog.String("kind", n.Kind.String()),
		slog.String("app", n.Target.App),
	}
	if n.Deployment != nil && n.Deployment.ID != 0 {
		attrs = append(attrs, slog.Int64("deployment_id", int64(n.Deployment.ID)))
	}
	return attrs
}

func clone(n core.Notification) core.Notification {
	n.Deployment = cloneDeployment(n.Deployment)
	n.RollbackOf = cloneDeployment(n.RollbackOf)
	if n.Lock != nil {
		l := *n.Lock
		l.DeploymentID = clonePointer(l.DeploymentID)
		l.ExpiresAt = clonePointer(l.ExpiresAt)
		n.Lock = &l
	}
	return n
}

func cloneDeployment(d *core.Deployment) *core.Deployment {
	if d == nil {
		return nil
	}
	c := *d
	c.ParentID = clonePointer(d.ParentID)
	c.RollbackOfID = clonePointer(d.RollbackOfID)
	c.SupersededByID = clonePointer(d.SupersededByID)
	c.StartedAt = clonePointer(d.StartedAt)
	c.FinishedAt = clonePointer(d.FinishedAt)
	return &c
}

func clonePointer[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
