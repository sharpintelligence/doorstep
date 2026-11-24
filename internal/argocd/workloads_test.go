package argocd

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/argocd/argocdtest"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/kube"
)

func TestWorkloads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		resources []argocdtest.Resource
		want      []core.Workload
	}{
		{name: "no resources"},
		{
			name: "workload kinds only",
			resources: []argocdtest.Resource{
				{Group: "apps", Kind: "Deployment", Namespace: "blog", Name: "web", Health: "Degraded"},
				{Group: "apps", Kind: "StatefulSet", Namespace: "blog", Name: "db", Health: "Healthy"},
				{Group: "apps", Kind: "DaemonSet", Namespace: "agents", Name: "logs"},
				{Group: "argoproj.io", Kind: "Rollout", Namespace: "blog", Name: "api"},
				{Group: "apps", Kind: "ReplicaSet", Namespace: "blog", Name: "web-1"},
				{Kind: "Service", Namespace: "blog", Name: "web"},
				{Group: "batch", Kind: "Job", Namespace: "blog", Name: "migrate"},
				{Group: "argoproj.io", Kind: "Application", Namespace: "argocd", Name: "child"},
				{Group: "extensions", Kind: "Deployment", Namespace: "blog", Name: "legacy"},
			},
			want: []core.Workload{
				{Group: "apps", Kind: "Deployment", Namespace: "blog", Name: "web"},
				{Group: "apps", Kind: "StatefulSet", Namespace: "blog", Name: "db"},
				{Group: "apps", Kind: "DaemonSet", Namespace: "agents", Name: "logs"},
				{Group: "argoproj.io", Kind: "Rollout", Namespace: "blog", Name: "api"},
			},
		},
		{
			name: "cluster scoped and duplicates are dropped",
			resources: []argocdtest.Resource{
				{Group: "apps", Kind: "Deployment", Name: "nowhere"},
				{Group: "apps", Kind: "Deployment", Namespace: "blog", Name: "web"},
				{Group: "apps", Kind: "Deployment", Namespace: "blog", Name: "web"},
			},
			want: []core.Workload{{Group: "apps", Kind: "Deployment", Namespace: "blog", Name: "web"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app := decode(t, with(synced(pinSHA), func(a *argocdtest.Application) { a.Resources = tt.resources }))
			if got := workloads(app); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("workloads = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestWorkloadsAreCapped(t *testing.T) {
	t.Parallel()

	var resources []argocdtest.Resource
	for i := range MaxResources + 5 {
		resources = append(resources, argocdtest.Resource{Group: "apps", Kind: "Deployment", Namespace: "blog", Name: fmt.Sprintf("web-%d", i)})
	}
	app := decode(t, with(synced(pinSHA), func(a *argocdtest.Application) { a.Resources = resources }))
	if got := workloads(app); len(got) != MaxResources {
		t.Errorf("got %d workloads, want %d", len(got), MaxResources)
	}
}

func TestWatcherWorkloads(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.srv.Set(with(synced(pinSHA), func(a *argocdtest.Application) {
		a.Resources = []argocdtest.Resource{{Group: "apps", Kind: "Deployment", Namespace: "blog", Name: "web", Health: "Degraded"}}
	}))

	got, err := f.watcher.Workloads(t.Context(), blogTarget)
	if err != nil || !reflect.DeepEqual(got, []core.Workload{{Group: "apps", Kind: "Deployment", Namespace: "blog", Name: "web"}}) {
		t.Fatalf("Workloads = %+v, %v", got, err)
	}
	if _, err := f.watcher.Workloads(t.Context(), shopTarget); !errors.Is(err, core.ErrSyncTargetNotFound) || !kube.IsNotFound(err) {
		t.Errorf("Workloads(missing) error = %v", err)
	}
	if _, err := f.watcher.Workloads(t.Context(), core.Target{App: "x", Environment: "x"}); !errors.Is(err, core.ErrSyncTargetNotFound) {
		t.Errorf("Workloads(unknown) error = %v", err)
	}
	f.srv.FailNext(argocdtest.Get, http.StatusForbidden, 1)
	if _, err := f.watcher.Workloads(t.Context(), blogTarget); !errors.Is(err, core.ErrSyncForbidden) {
		t.Errorf("Workloads(403) error = %v", err)
	}
	if got := f.srv.Requests(argocdtest.Watch); got != 0 {
		t.Errorf("Workloads opened %d watches", got)
	}
}
