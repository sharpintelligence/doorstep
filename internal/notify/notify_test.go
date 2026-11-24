package notify

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

type fakeSink struct {
	name     string
	sinkType string
	kinds    kindSet
	block    chan struct{}
	started  chan struct{}
	err      error

	mu       sync.Mutex
	received []core.Notification
	attempts int
}

func newFakeSink(name string, kinds ...core.NotificationKind) *fakeSink {
	if len(kinds) == 0 {
		kinds = core.NotificationKinds()
	}
	set, _ := newKindSet(kinds)
	return &fakeSink{name: name, sinkType: TypeWebhook, kinds: set, started: make(chan struct{}, 1024)}
}

func (s *fakeSink) Name() string                     { return s.name }
func (s *fakeSink) Type() string                     { return s.sinkType }
func (s *fakeSink) Accepts(n core.Notification) bool { return s.kinds[n.Kind] }

func (s *fakeSink) Prepare(n core.Notification) Delivery {
	return func(ctx context.Context) error {
		s.started <- struct{}{}
		if s.block != nil {
			select {
			case <-s.block:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.attempts++
		if s.err != nil {
			return s.err
		}
		s.received = append(s.received, n)
		return nil
	}
}

func (s *fakeSink) got() []core.Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.received)
}

func (s *fakeSink) kindsReceived() []core.NotificationKind {
	var kinds []core.NotificationKind
	for _, n := range s.got() {
		kinds = append(kinds, n.Kind)
	}
	return kinds
}

func newTestDispatcher(t *testing.T, opts Options) *Dispatcher {
	t.Helper()
	if opts.Retry.Sleep == nil {
		opts.Retry = fastPolicy()
	}
	d, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_ = d.Close(ctx)
	})
	return d
}

func closeDispatcher(t *testing.T, d *Dispatcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := d.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func forApp(n core.Notification, app string) core.Notification {
	n.Target.App = app
	if n.Deployment != nil {
		d := *n.Deployment
		d.Target.App = app
		n.Deployment = &d
	}
	if n.RollbackOf != nil {
		d := *n.RollbackOf
		d.Target.App = app
		n.RollbackOf = &d
	}
	if n.Lock != nil {
		l := *n.Lock
		l.Target.App = app
		n.Lock = &l
	}
	return n
}

func TestDispatcherRouting(t *testing.T) {
	t.Parallel()

	succeeded := sampleNamed(t, "deploy-succeeded")
	tests := []struct {
		name   string
		app    string
		routes map[string][]string
		want   map[string]int
	}{
		{name: "routed app", app: "blog", routes: map[string][]string{"blog": {"ops"}}, want: map[string]int{"ops": 1, "hook": 0, "global": 1}},
		{name: "several targets", app: "blog", routes: map[string][]string{"blog": {"ops", "hook"}}, want: map[string]int{"ops": 1, "hook": 1, "global": 1}},
		{name: "duplicate route is delivered once", app: "blog", routes: map[string][]string{"blog": {"ops", "ops"}}, want: map[string]int{"ops": 1, "hook": 0, "global": 1}},
		{name: "other app", app: "api", routes: map[string][]string{"blog": {"ops"}, "api": {"hook"}}, want: map[string]int{"ops": 0, "hook": 1, "global": 1}},
		{name: "unknown app only reaches global sinks", app: "unknown", routes: map[string][]string{"blog": {"ops"}}, want: map[string]int{"ops": 0, "hook": 0, "global": 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sinks := map[string]*fakeSink{"ops": newFakeSink("ops"), "hook": newFakeSink("hook"), "global": newFakeSink("global")}
			d := newTestDispatcher(t, Options{
				Sinks:  []Sink{sinks["ops"], sinks["hook"]},
				Global: []Sink{sinks["global"]},
				Routes: tt.routes,
			})
			d.Notify(t.Context(), forApp(succeeded, tt.app))
			closeDispatcher(t, d)
			for name, want := range tt.want {
				if got := len(sinks[name].got()); got != want {
					t.Errorf("sink %s received %d, want %d", name, got, want)
				}
			}
		})
	}
}

func TestDispatcherKindFiltering(t *testing.T) {
	t.Parallel()

	defaults := newFakeSink("defaults", core.DefaultNotificationKinds()...)
	failures := newFakeSink("failures", core.NotificationDeployFailed)
	d := newTestDispatcher(t, Options{Sinks: []Sink{defaults, failures}, Routes: map[string][]string{"blog": {"defaults", "failures"}}})
	var all []core.NotificationKind
	for _, s := range samples() {
		d.Notify(t.Context(), s.n)
		all = append(all, s.n.Kind)
	}
	closeDispatcher(t, d)

	var wantDefaults, wantFailures []core.NotificationKind
	for _, k := range all {
		if slices.Contains(core.DefaultNotificationKinds(), k) {
			wantDefaults = append(wantDefaults, k)
		}
		if k == core.NotificationDeployFailed {
			wantFailures = append(wantFailures, k)
		}
	}
	if got := defaults.kindsReceived(); !slices.Equal(got, wantDefaults) {
		t.Errorf("defaults received %v, want %v", got, wantDefaults)
	}
	if got := failures.kindsReceived(); !slices.Equal(got, wantFailures) {
		t.Errorf("failures received %v, want %v", got, wantFailures)
	}
	for _, k := range []core.NotificationKind{core.NotificationDeployQueued, core.NotificationDeployCancelled, core.NotificationDeploySuperseded} {
		if slices.Contains(defaults.kindsReceived(), k) {
			t.Errorf("%s is delivered by default", k)
		}
	}
}

func TestDispatcherPreservesOrderPerSink(t *testing.T) {
	t.Parallel()

	sink := newFakeSink("ops")
	d := newTestDispatcher(t, Options{Sinks: []Sink{sink}, Routes: map[string][]string{"blog": {"ops"}}})
	base := sampleNamed(t, "deploy-started")
	var want []core.DeploymentID
	for i := range 50 {
		n := base
		dep := *n.Deployment
		dep.ID = core.DeploymentID(i + 1)
		n.Deployment = &dep
		want = append(want, dep.ID)
		d.Notify(t.Context(), n)
	}
	closeDispatcher(t, d)
	var got []core.DeploymentID
	for _, n := range sink.got() {
		got = append(got, n.Deployment.ID)
	}
	if !slices.Equal(got, want) {
		t.Errorf("delivery order = %v, want %v", got, want)
	}
}

func TestDispatcherBlockedSinkDoesNotDelayOthers(t *testing.T) {
	t.Parallel()

	slow := newFakeSink("slow")
	slow.block = make(chan struct{})
	fast := newFakeSink("fast")
	d := newTestDispatcher(t, Options{Sinks: []Sink{slow, fast}, Routes: map[string][]string{"blog": {"slow", "fast"}}})

	start := time.Now()
	for _, s := range samples()[:3] {
		d.Notify(t.Context(), s.n)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Notify blocked for %s while a sink was stuck", elapsed)
	}
	waitFor(t, "fast sink deliveries", func() bool { return len(fast.got()) == 3 })
	if len(slow.got()) != 0 {
		t.Errorf("slow sink delivered while blocked")
	}
	close(slow.block)
	closeDispatcher(t, d)
	if len(slow.got()) != 3 {
		t.Errorf("slow sink received %d after unblocking, want 3", len(slow.got()))
	}
}

func TestDispatcherDropsWhenQueueIsFull(t *testing.T) {
	t.Parallel()

	logger, logs := newTestLogger()
	observer := &recordingObserver{}
	sink := newFakeSink("ops")
	sink.block = make(chan struct{})
	d := newTestDispatcher(t, Options{
		Sinks:     []Sink{sink},
		Routes:    map[string][]string{"blog": {"ops"}},
		QueueSize: 2,
		Logger:    logger,
		Observer:  observer,
	})
	n := sampleNamed(t, "deploy-succeeded")
	d.Notify(t.Context(), n)
	<-sink.started
	for range 4 {
		d.Notify(t.Context(), n)
	}
	if got := observer.count(TypeWebhook, ResultDropped); got != 2 {
		t.Errorf("dropped = %d, want 2", got)
	}
	if got := logs.count(t, "notification dropped", "WARN"); got != 2 {
		t.Errorf("drop warnings = %d, want 2: %s", got, logs)
	}
	if !strings.Contains(logs.String(), `"reason":"queue is full"`) {
		t.Errorf("drop log does not give the reason: %s", logs)
	}
	close(sink.block)
	closeDispatcher(t, d)
	if got := len(sink.got()); got != 3 {
		t.Errorf("delivered %d, want 3", got)
	}
	if got := observer.count(TypeWebhook, ResultSent); got != 3 {
		t.Errorf("sent = %d, want 3", got)
	}
}

func TestDispatcherRecordsFailures(t *testing.T) {
	t.Parallel()

	logger, logs := newTestLogger()
	observer := &recordingObserver{}
	failing := newFakeSink("failing")
	failing.err = errors.New("boom")
	quiet := newFakeSink("quiet")
	quiet.err = reported(permanent(errors.New("already logged")))
	d := newTestDispatcher(t, Options{
		Sinks:    []Sink{failing, quiet},
		Routes:   map[string][]string{"blog": {"failing", "quiet"}},
		Logger:   logger,
		Observer: observer,
	})
	d.Notify(t.Context(), sampleNamed(t, "deploy-failed"))
	closeDispatcher(t, d)

	if failing.attempts != 4 {
		t.Errorf("failing sink attempts = %d, want 4", failing.attempts)
	}
	if quiet.attempts != 1 {
		t.Errorf("permanent failure attempts = %d, want 1", quiet.attempts)
	}
	if got := observer.count(TypeWebhook, ResultFailed); got != 2 {
		t.Errorf("failed = %d, want 2", got)
	}
	if got := logs.count(t, "notification failed", "WARN"); got != 1 {
		t.Errorf("failure warnings = %d, want 1: %s", got, logs)
	}
	if got := logs.count(t, "notification failed", "DEBUG"); got != 1 {
		t.Errorf("reported failure debug logs = %d, want 1: %s", got, logs)
	}
	for _, e := range logs.entries(t) {
		if e["msg"] == "notification failed" && e["level"] == "WARN" {
			if e["sink"] != "failing" || e["kind"] != "deploy.failed" || e["app"] != "blog" || e["deployment_id"] != float64(42) || e["attempts"] != float64(4) || e["error"] != "boom" {
				t.Errorf("failure log = %v", e)
			}
		}
	}
}

func TestDispatcherNotifyDoesNotBlock(t *testing.T) {
	t.Parallel()

	sink := newFakeSink("ops")
	sink.block = make(chan struct{})
	d := newTestDispatcher(t, Options{Sinks: []Sink{sink}, Routes: map[string][]string{"blog": {"ops"}}, QueueSize: 1})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			d.Notify(t.Context(), sampleNamed(t, "deploy-started"))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Notify blocked on a stuck sink")
	}
	close(sink.block)
}

func TestDispatcherValidatesAndStampsNotifications(t *testing.T) {
	t.Parallel()

	logger, logs := newTestLogger()
	sink := newFakeSink("ops")
	stamp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	d := newTestDispatcher(t, Options{
		Sinks:  []Sink{sink},
		Routes: map[string][]string{"blog": {"ops"}},
		Logger: logger,
		Now:    func() time.Time { return stamp },
	})
	invalid := sampleNamed(t, "deploy-started")
	invalid.Deployment = nil
	d.Notify(t.Context(), invalid)

	unstamped := sampleNamed(t, "deploy-started")
	unstamped.OccurredAt = time.Time{}
	d.Notify(t.Context(), unstamped)

	mutated := sampleNamed(t, "deploy-succeeded")
	d.Notify(t.Context(), mutated)
	mutated.Deployment.RequestedBy = "changed after notify"

	closeDispatcher(t, d)
	got := sink.got()
	if len(got) != 2 {
		t.Fatalf("delivered %d, want 2", len(got))
	}
	if !got[0].OccurredAt.Equal(stamp) {
		t.Errorf("OccurredAt = %v, want %v", got[0].OccurredAt, stamp)
	}
	if got[1].Deployment.RequestedBy != "alice" {
		t.Errorf("dispatcher shares the caller's deployment: %q", got[1].Deployment.RequestedBy)
	}
	if logs.count(t, "invalid notification dropped", "ERROR") != 1 {
		t.Errorf("invalid notification not logged: %s", logs)
	}
}

func TestDispatcherCloseDrains(t *testing.T) {
	t.Parallel()

	sink := newFakeSink("ops")
	sink.block = make(chan struct{})
	d := newTestDispatcher(t, Options{Sinks: []Sink{sink}, Routes: map[string][]string{"blog": {"ops"}}})
	for range 5 {
		d.Notify(t.Context(), sampleNamed(t, "deploy-succeeded"))
	}
	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		closed <- d.Close(ctx)
	}()
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v before the queue drained", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(sink.block)
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(sink.got()); got != 5 {
		t.Errorf("delivered %d, want 5", got)
	}
}

func TestDispatcherCloseHonoursDeadline(t *testing.T) {
	t.Parallel()

	observer := &recordingObserver{}
	sink := newFakeSink("ops")
	sink.block = make(chan struct{})
	defer close(sink.block)
	d := newTestDispatcher(t, Options{Sinks: []Sink{sink}, Routes: map[string][]string{"blog": {"ops"}}, Observer: observer})
	for range 3 {
		d.Notify(t.Context(), sampleNamed(t, "deploy-succeeded"))
	}
	<-sink.started

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := d.Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Close took %s past a 50ms deadline", elapsed)
	}
	if got := observer.count(TypeWebhook, ResultFailed) + observer.count(TypeWebhook, ResultDropped); got != 3 {
		t.Errorf("failed+dropped = %d, want 3 (%v)", got, observer.seen)
	}
	if len(sink.got()) != 0 {
		t.Errorf("delivered %d after the deadline", len(sink.got()))
	}
}

func TestDispatcherNotifyAfterClose(t *testing.T) {
	t.Parallel()

	logger, logs := newTestLogger()
	observer := &recordingObserver{}
	sink := newFakeSink("ops")
	d := newTestDispatcher(t, Options{Sinks: []Sink{sink}, Routes: map[string][]string{"blog": {"ops"}}, Logger: logger, Observer: observer})
	closeDispatcher(t, d)
	d.Notify(t.Context(), sampleNamed(t, "deploy-succeeded"))
	closeDispatcher(t, d)
	if len(sink.got()) != 0 {
		t.Errorf("delivered after Close")
	}
	if observer.count(TypeWebhook, ResultDropped) != 1 || !strings.Contains(logs.String(), "dispatcher is closed") {
		t.Errorf("notification after Close was not reported as dropped: %v %s", observer.seen, logs)
	}
}

func TestNewDispatcherErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    Options
		wantErr string
	}{
		{name: "duplicate sink", opts: Options{Sinks: []Sink{newFakeSink("ops"), newFakeSink("ops")}}, wantErr: `duplicate sink name "ops"`},
		{name: "duplicate across global", opts: Options{Sinks: []Sink{newFakeSink("github")}, Global: []Sink{newFakeSink("github")}}, wantErr: `duplicate sink name "github"`},
		{name: "unknown route", opts: Options{Sinks: []Sink{newFakeSink("ops")}, Routes: map[string][]string{"blog": {"pager"}}}, wantErr: `app "blog" routes to unknown sink "pager"`},
		{name: "route to global sink", opts: Options{Global: []Sink{newFakeSink("github")}, Routes: map[string][]string{"blog": {"github"}}}, wantErr: `unknown sink "github"`},
		{name: "nil sink", opts: Options{Sinks: []Sink{nil}}, wantErr: "must not be nil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("New() = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestDispatcherWithoutSinks(t *testing.T) {
	t.Parallel()

	d := newTestDispatcher(t, Options{})
	d.Notify(t.Context(), sampleNamed(t, "deploy-succeeded"))
	closeDispatcher(t, d)
}
