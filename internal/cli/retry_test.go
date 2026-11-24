package cli

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/engine"
	"github.com/sharpintelligence/doorstep/internal/retry"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const retryCatalog = `
apps:
  - name: blog
    source: {repo: example-user/blog}
    image: {repository: ghcr.io/example-user/blog}
    gitops: {repo: example-user/homelab, path: apps/blog}
  - name: shop
    source: {repo: example-user/shop}
    image: {repository: ghcr.io/example-user/shop}
    gitops: {repo: example-user/homelab, path: apps/shop}
    retry:
      enabled: false
      max_attempts: 5
      backoff: 30s
      max_backoff: 2m
`

func TestCatalogAppsRetrySettings(t *testing.T) {
	t.Parallel()

	apps := newCatalogApps(loadCatalog(t, retryCatalog))
	tests := []struct {
		name   string
		target core.Target
		want   retry.Settings
		wantOK bool
	}{
		{name: "defaults", target: core.AppTarget("blog"), want: retry.Settings{Enabled: true, MaxAttempts: 3, Backoff: time.Minute, MaxBackoff: 10 * time.Minute}, wantOK: true},
		{name: "configured", target: core.AppTarget("shop"), want: retry.Settings{MaxAttempts: 5, Backoff: 30 * time.Second, MaxBackoff: 2 * time.Minute}, wantOK: true},
		{name: "unknown app", target: core.AppTarget("wiki")},
		{name: "other environment", target: core.Target{App: "blog", Environment: "staging"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := apps.RetrySettings(tt.target)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("RetrySettings = %+v, %t, want %+v, %t", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

type fakeWorkloads struct {
	workloads []core.Workload
	err       error
}

func (f fakeWorkloads) Workloads(context.Context, core.Target) ([]core.Workload, error) {
	return f.workloads, f.err
}

type fakePods struct {
	called *[]core.Workload
	pods   []core.PodStatus
	err    error
}

func (f fakePods) Pods(_ context.Context, workloads []core.Workload) ([]core.PodStatus, error) {
	*f.called = append(*f.called, workloads...)
	return f.pods, f.err
}

func TestPodDiagnostics(t *testing.T) {
	t.Parallel()

	web := core.Workload{Group: "apps", Kind: "Deployment", Namespace: "blog", Name: "web"}
	pod := core.PodStatus{Namespace: "blog", Name: "web-1", Node: "pi-1"}
	tests := []struct {
		name       string
		workloads  fakeWorkloads
		pods       []core.PodStatus
		podsErr    error
		want       []core.PodStatus
		wantErr    string
		wantListed bool
	}{
		{name: "pods of the workloads", workloads: fakeWorkloads{workloads: []core.Workload{web}}, pods: []core.PodStatus{pod}, want: []core.PodStatus{pod}, wantListed: true},
		{name: "no workloads, no pod reads", workloads: fakeWorkloads{}},
		{name: "application read fails", workloads: fakeWorkloads{err: errors.New("forbidden")}, wantErr: "list the workloads of blog/default: forbidden"},
		{name: "pod read fails", workloads: fakeWorkloads{workloads: []core.Workload{web}}, podsErr: errors.New("pods is forbidden"), wantErr: "pods is forbidden", wantListed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var listed []core.Workload
			p := podDiagnostics{workloads: tt.workloads, pods: fakePods{called: &listed, pods: tt.pods, err: tt.podsErr}}
			got, err := p.Pods(t.Context(), core.Deployment{Target: core.AppTarget("blog")})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Pods error = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Pods = %+v, %v, want %+v", got, err, tt.want)
			}
			if (len(listed) > 0) != tt.wantListed {
				t.Errorf("pods listed for %v, want listed %t", listed, tt.wantListed)
			}
		})
	}
}

type fakeDiagnosesStore struct {
	got    []any
	result store.Diagnoses
	err    error
}

func (f *fakeDiagnosesStore) Diagnoses(_ context.Context, id core.DeploymentID, window time.Duration, limit int) (store.Diagnoses, error) {
	f.got = []any{id, window, limit}
	return f.result, f.err
}

func TestRetryHistory(t *testing.T) {
	t.Parallel()

	fake := &fakeDiagnosesStore{result: store.Diagnoses{
		Deployment: []json.RawMessage{json.RawMessage(`{"attempt":1}`)},
		Recent:     []json.RawMessage{json.RawMessage(`{"nodes":["pi-1"]}`)},
	}}
	earlier, recent, err := retryHistory{store: fake}.Diagnoses(t.Context(), 42)
	if err != nil || len(earlier) != 1 || len(recent) != 1 || string(recent[0]) != `{"nodes":["pi-1"]}` {
		t.Fatalf("Diagnoses = %s, %s, %v", earlier, recent, err)
	}
	if want := []any{core.DeploymentID(42), store.DefaultDiagnosisWindow, store.MaxDiagnoses}; !reflect.DeepEqual(fake.got, want) {
		t.Errorf("store asked %v, want %v", fake.got, want)
	}
	if _, _, err := (retryHistory{store: &fakeDiagnosesStore{err: errors.New("down")}}).Diagnoses(t.Context(), 42); err == nil {
		t.Error("store error was swallowed")
	}
}

type fakeQueueStore struct {
	entries []core.QueueEntry
	err     error
}

func (f fakeQueueStore) QueueEntries(context.Context, string) ([]core.QueueEntry, error) {
	return f.entries, f.err
}

func TestRetryQueue(t *testing.T) {
	t.Parallel()

	blog := core.AppTarget("blog")
	staging := core.Target{App: "blog", Environment: "staging"}
	entry := func(id core.DeploymentID, t core.Target) core.QueueEntry {
		return core.QueueEntry{Deployment: core.Deployment{ID: id, Target: t}}
	}
	tests := []struct {
		name    string
		entries []core.QueueEntry
		err     error
		want    bool
		wantErr bool
	}{
		{name: "only the retrying deployment", entries: []core.QueueEntry{entry(7, blog)}},
		{name: "a newer deployment waits", entries: []core.QueueEntry{entry(7, blog), entry(9, blog)}, want: true},
		{name: "other environments do not count", entries: []core.QueueEntry{entry(7, blog), entry(9, staging)}},
		{name: "empty queue"},
		{name: "store error", err: errors.New("down"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := retryQueue{store: fakeQueueStore{entries: tt.entries, err: tt.err}}.Waiting(t.Context(), blog, 7)
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Errorf("Waiting = %t, %v, want %t (error %t)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestNewRetryPolicy(t *testing.T) {
	t.Parallel()

	catalog := loadCatalog(t, retryCatalog)
	in := engineInput{config: config.Default(), catalog: catalog, apps: newCatalogApps(catalog), logger: slog.New(slog.DiscardHandler), lookupEnv: envFrom(nil)}
	policy, err := newRetryPolicy(in)
	if err != nil {
		t.Fatalf("newRetryPolicy: %v", err)
	}
	decision, err := policy.Decide(t.Context(), engine.Failure{
		Deployment: core.Deployment{ID: 1, Target: core.AppTarget("blog"), Kind: core.KindDeploy},
		Stage:      core.StateVerifying,
		Class:      core.FailureVersion,
	})
	if err != nil || decision.Action != engine.FailureActionFail || decision.Reason != engine.DecisionNotRetryable {
		t.Errorf("Decide = %+v, %v", decision, err)
	}

	in.config.Kubernetes.PodDiagnostics = true
	if _, err := newRetryPolicy(in); err == nil || !strings.Contains(err.Error(), "pod diagnostics") {
		t.Errorf("newRetryPolicy without a Kubernetes connection = %v, want a pod diagnostics error", err)
	}
}
