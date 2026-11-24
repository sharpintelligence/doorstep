package pods_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/kube"
	"github.com/sharpintelligence/doorstep/internal/pods"
)

var _ pods.Getter = (*kube.Client)(nil)

type fakeAPI struct {
	mu       sync.Mutex
	lists    map[string]string
	status   int
	requests []string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	key := r.URL.Path
	if token := r.URL.Query().Get("continue"); token != "" {
		key += "#" + token
	}
	body, ok := f.lists[key]
	status := f.status
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case status != 0:
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"kind":"Status","status":"Failure","reason":"Forbidden","message":"pods is forbidden","code":%d}`, status)
	case !ok:
		_, _ = w.Write([]byte(`{"items":[]}`))
	default:
		_, _ = w.Write([]byte(body))
	}
}

func (f *fakeAPI) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func newReader(t *testing.T, api *fakeAPI) *pods.Reader {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	client, err := kube.New(kube.Config{Server: srv.URL}, kube.Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	reader, err := pods.New(client, nil)
	if err != nil {
		t.Fatalf("pods.New: %v", err)
	}
	return reader
}

func list(items ...map[string]any) string {
	return page("", items...)
}

func page(next string, items ...map[string]any) string {
	body, err := json.Marshal(map[string]any{"kind": "PodList", "metadata": map[string]any{"continue": next}, "items": items})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func podJSON(name, namespace, ownerKind, ownerName string, labels map[string]string, extra func(map[string]any)) map[string]any {
	p := map[string]any{
		"metadata": map[string]any{
			"name": name, "namespace": namespace, "labels": labels,
			"ownerReferences": []map[string]any{{"kind": ownerKind, "name": ownerName, "controller": true}},
		},
		"spec": map[string]any{
			"nodeName":   "pi-1",
			"containers": []map[string]any{{"name": "app", "image": "ghcr.io/example-user/blog@sha256:2222222222222222222222222222222222222222222222222222222222222222", "env": []map[string]any{{"name": "TOKEN", "value": "secret"}}}},
		},
		"status": map[string]any{
			"phase": "Running",
			"conditions": []map[string]any{
				{"type": "PodScheduled", "status": "True"},
				{"type": "Ready", "status": "True"},
			},
			"containerStatuses": []map[string]any{{"name": "app", "ready": true, "restartCount": 0, "image": "ghcr.io/example-user/blog@sha256:2222222222222222222222222222222222222222222222222222222222222222", "state": map[string]any{"running": map[string]any{}}}},
		},
	}
	if extra != nil {
		extra(p)
	}
	return p
}

func TestReaderPods(t *testing.T) {
	t.Parallel()

	deployment := core.Workload{Group: "apps", Kind: "Deployment", Namespace: "blog", Name: "web"}
	statefulSet := core.Workload{Group: "apps", Kind: "StatefulSet", Namespace: "blog", Name: "db"}
	daemonSet := core.Workload{Group: "apps", Kind: "DaemonSet", Namespace: "agents", Name: "logs"}
	rollout := core.Workload{Group: "argoproj.io", Kind: "Rollout", Namespace: "blog", Name: "api"}
	api := &fakeAPI{lists: map[string]string{
		"/api/v1/namespaces/blog/pods": list(
			podJSON("web-5d9c-abcde", "blog", "ReplicaSet", "web-5d9c", map[string]string{"pod-template-hash": "5d9c"}, func(p map[string]any) {
				status := p["status"].(map[string]any)
				status["conditions"] = []map[string]any{{"type": "PodScheduled", "status": "True"}, {"type": "Ready", "status": "False"}}
				status["containerStatuses"] = []map[string]any{{
					"name": "app", "ready": false, "restartCount": 5,
					"imageID":   "ghcr.io/example-user/blog@sha256:9999999999999999999999999999999999999999999999999999999999999999",
					"state":     map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff", "message": "back-off 5m0s restarting failed container"}},
					"lastState": map[string]any{"terminated": map[string]any{"reason": "OOMKilled", "exitCode": 137}},
				}}
			}),
			podJSON("web-other-xyz", "blog", "ReplicaSet", "web-other-77", map[string]string{"pod-template-hash": "77"}, nil),
			podJSON("db-0", "blog", "StatefulSet", "db", nil, func(p map[string]any) {
				p["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-24T10:00:00Z"
				p["spec"].(map[string]any)["nodeName"] = ""
				status := p["status"].(map[string]any)
				status["phase"] = "Pending"
				status["conditions"] = []map[string]any{{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": "0/3 nodes are available: 3 Insufficient memory."}}
				status["containerStatuses"] = nil
			}),
			podJSON("api-6f-1", "blog", "ReplicaSet", "api-6f", map[string]string{"rollouts-pod-template-hash": "6f"}, func(p map[string]any) {
				p["status"].(map[string]any)["initContainerStatuses"] = []map[string]any{{"name": "migrate", "image": "busybox:1", "state": map[string]any{"terminated": map[string]any{"reason": "Completed", "exitCode": 0}}}}
				p["spec"].(map[string]any)["initContainers"] = []map[string]any{{"name": "migrate", "image": "busybox:1"}}
			}),
			podJSON("orphan", "blog", "ReplicaSet", "web-5d9c", map[string]string{"pod-template-hash": "5d9c"}, func(p map[string]any) {
				p["metadata"].(map[string]any)["ownerReferences"] = []map[string]any{{"kind": "ReplicaSet", "name": "web-5d9c"}}
			}),
		),
		"/api/v1/namespaces/agents/pods": list(
			podJSON("logs-abc", "agents", "DaemonSet", "logs", nil, func(p map[string]any) {
				status := p["status"].(map[string]any)
				status["phase"] = "Failed"
				status["reason"] = "Evicted"
				status["message"] = "The node was low on resource: memory."
			}),
		),
	}}
	reader := newReader(t, api)

	got, err := reader.Pods(t.Context(), []core.Workload{deployment, statefulSet, daemonSet, rollout})
	if err != nil {
		t.Fatalf("Pods: %v", err)
	}
	newImage := "ghcr.io/example-user/blog@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	want := []core.PodStatus{
		{
			Namespace: "agents", Name: "logs-abc", Node: "pi-1", Workload: daemonSet, Images: []string{newImage},
			Phase: "Failed", Reason: "Evicted", Message: "The node was low on resource: memory.", Ready: true, Scheduled: true,
			Containers: []core.ContainerStatus{{Name: "app", Ready: true}},
		},
		{
			Namespace: "blog", Name: "web-5d9c-abcde", Node: "pi-1", Workload: deployment,
			Images: []string{newImage, "ghcr.io/example-user/blog@sha256:9999999999999999999999999999999999999999999999999999999999999999"},
			Phase:  "Running", Scheduled: true,
			Containers: []core.ContainerStatus{{Name: "app", Restarts: 5, Waiting: "CrashLoopBackOff", Terminated: "OOMKilled", ExitCode: 137, Message: "back-off 5m0s restarting failed container"}},
		},
		{
			Namespace: "blog", Name: "db-0", Workload: statefulSet, Images: []string{newImage},
			Phase: "Pending", Message: "0/3 nodes are available: 3 Insufficient memory.", Terminating: true,
		},
		{
			Namespace: "blog", Name: "api-6f-1", Node: "pi-1", Workload: rollout, Images: []string{"busybox:1", newImage},
			Phase: "Running", Ready: true, Scheduled: true,
			Containers: []core.ContainerStatus{{Name: "migrate", Terminated: "Completed"}, {Name: "app", Ready: true}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Pods =\n%+v\nwant\n%+v", got, want)
	}
	requests := api.seen()
	wantRequests := []string{"GET /api/v1/namespaces/agents/pods?limit=500", "GET /api/v1/namespaces/blog/pods?limit=500"}
	if !reflect.DeepEqual(requests, wantRequests) {
		t.Errorf("requests = %v, want %v", requests, wantRequests)
	}
}

func TestReaderPodsLimits(t *testing.T) {
	t.Parallel()

	var items []map[string]any
	for i := range pods.MaxPods + 50 {
		items = append(items, podJSON(fmt.Sprintf("web-1-%d", i), "blog", "ReplicaSet", "web-1", map[string]string{"pod-template-hash": "1"}, func(p map[string]any) {
			p["status"].(map[string]any)["message"] = strings.Repeat("é", 300)
		}))
	}
	api := &fakeAPI{lists: map[string]string{"/api/v1/namespaces/blog/pods": list(items...)}}
	reader := newReader(t, api)

	got, err := reader.Pods(t.Context(), []core.Workload{{Kind: "Deployment", Namespace: "blog", Name: "web"}})
	if err != nil {
		t.Fatalf("Pods: %v", err)
	}
	if len(got) != pods.MaxPods {
		t.Errorf("got %d pods, want %d", len(got), pods.MaxPods)
	}
	if len(got[0].Message) > pods.MaxMessageBytes || !strings.HasPrefix(got[0].Message, "é") || strings.ContainsRune(got[0].Message, '�') {
		t.Errorf("message is %d bytes, want at most %d valid ones", len(got[0].Message), pods.MaxMessageBytes)
	}
	var workloads []core.Workload
	for i := range pods.MaxNamespaces + 2 {
		workloads = append(workloads, core.Workload{Kind: "Deployment", Namespace: fmt.Sprintf("ns-%d", i), Name: "web"})
	}
	if _, err := reader.Pods(t.Context(), workloads); err != nil {
		t.Fatalf("Pods: %v", err)
	}
	if requests := api.seen(); len(requests) != 1+pods.MaxNamespaces {
		t.Errorf("requests = %v, want one for blog and %d namespaces", requests, pods.MaxNamespaces)
	}
}

func TestReaderPodsErrors(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{status: http.StatusForbidden}
	reader := newReader(t, api)
	_, err := reader.Pods(t.Context(), []core.Workload{{Kind: "Deployment", Namespace: "blog", Name: "web"}})
	if !kube.IsForbidden(err) || !strings.Contains(err.Error(), "list pods in blog") {
		t.Errorf("Pods error = %v, want a forbidden error naming the namespace", err)
	}

	got, err := reader.Pods(t.Context(), nil)
	if err != nil || len(got) != 0 {
		t.Errorf("Pods(nil) = %v, %v", got, err)
	}
	if _, err := reader.Pods(t.Context(), []core.Workload{{Kind: "Deployment", Namespace: "..", Name: "web"}}); err == nil {
		t.Error("Pods accepted a namespace that escapes the path")
	}
	if _, err := pods.New(nil, nil); err == nil {
		t.Error("New(nil) succeeded")
	}
}

func TestReaderPodsPages(t *testing.T) {
	t.Parallel()

	web := func(name string) map[string]any {
		return podJSON(name, "blog", "ReplicaSet", "web-1", map[string]string{"pod-template-hash": "1"}, nil)
	}
	other := podJSON("cron-1", "blog", "Job", "cron", nil, nil)
	workloads := []core.Workload{{Kind: "Deployment", Namespace: "blog", Name: "web"}}

	tests := []struct {
		name         string
		lists        map[string]string
		wantPods     []string
		wantRequests []string
	}{
		{
			name: "follows continue tokens",
			lists: map[string]string{
				"/api/v1/namespaces/blog/pods":         page("p2", other),
				"/api/v1/namespaces/blog/pods#p2":      page("p3", web("web-1-a")),
				"/api/v1/namespaces/blog/pods#p3":      page("", web("web-1-b")),
				"/api/v1/namespaces/blog/pods#unknown": page("", web("never")),
			},
			wantPods: []string{"web-1-a", "web-1-b"},
			wantRequests: []string{
				"GET /api/v1/namespaces/blog/pods?limit=500",
				"GET /api/v1/namespaces/blog/pods?continue=p2&limit=500",
				"GET /api/v1/namespaces/blog/pods?continue=p3&limit=500",
			},
		},
		{
			name: "stops after the page limit",
			lists: func() map[string]string {
				lists := map[string]string{"/api/v1/namespaces/blog/pods": page("p1", other)}
				for i := 1; i <= pods.MaxListPages+2; i++ {
					lists[fmt.Sprintf("/api/v1/namespaces/blog/pods#p%d", i)] = page(fmt.Sprintf("p%d", i+1), web(fmt.Sprintf("web-1-%d", i)))
				}
				return lists
			}(),
			wantPods: func() []string {
				var names []string
				for i := 1; i < pods.MaxListPages; i++ {
					names = append(names, fmt.Sprintf("web-1-%d", i))
				}
				return names
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			api := &fakeAPI{lists: tt.lists}
			got, err := newReader(t, api).Pods(t.Context(), workloads)
			if err != nil {
				t.Fatalf("Pods: %v", err)
			}
			var names []string
			for _, p := range got {
				names = append(names, p.Name)
			}
			if !reflect.DeepEqual(names, tt.wantPods) {
				t.Errorf("pods = %v, want %v", names, tt.wantPods)
			}
			requests := api.seen()
			if tt.wantRequests != nil && !reflect.DeepEqual(requests, tt.wantRequests) {
				t.Errorf("requests = %v, want %v", requests, tt.wantRequests)
			}
			if len(requests) > pods.MaxListPages {
				t.Errorf("made %d requests, want at most %d", len(requests), pods.MaxListPages)
			}
		})
	}
}
