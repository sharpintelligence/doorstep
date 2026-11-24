package argocdtest

import (
	"strconv"
	"time"
)

type Application struct {
	Namespace     string
	Name          string
	RepoURL       string
	Sources       []string
	Automated     bool
	Sync          string
	Revision      string
	Revisions     []string
	Health        string
	HealthMessage string
	Operation     *Operation
	Conditions    []Condition
	Resources     []Resource
	ReconciledAt  time.Time
}

type Operation struct {
	Phase         string
	Message       string
	Revision      string
	Revisions     []string
	Resources     []SyncResource
	FinishedAt    time.Time
	WithoutResult bool
}

type SyncResource struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
	Status    string
	Message   string
	HookPhase string
}

type Condition struct {
	Type    string
	Message string
}

type Resource struct {
	Group         string
	Kind          string
	Namespace     string
	Name          string
	Status        string
	Health        string
	HealthMessage string
}

func Synced(namespace, name, repoURL, revision string) Application {
	return Application{
		Namespace: namespace,
		Name:      name,
		RepoURL:   repoURL,
		Automated: true,
		Sync:      "Synced",
		Revision:  revision,
		Health:    "Healthy",
		Operation: &Operation{Phase: "Succeeded", Message: "successfully synced (all tasks run)", Revision: revision},
	}
}

func (a Application) Clone() Application {
	out := a
	out.Sources = append([]string(nil), a.Sources...)
	out.Revisions = append([]string(nil), a.Revisions...)
	out.Conditions = append([]Condition(nil), a.Conditions...)
	out.Resources = append([]Resource(nil), a.Resources...)
	if a.Operation != nil {
		op := *a.Operation
		op.Revisions = append([]string(nil), a.Operation.Revisions...)
		op.Resources = append([]SyncResource(nil), a.Operation.Resources...)
		out.Operation = &op
	}
	return out
}

func (a Application) Object(resourceVersion int) map[string]any {
	spec := map[string]any{"project": "default", "destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": a.Name}}
	if len(a.Sources) > 0 {
		sources := make([]any, 0, len(a.Sources))
		for _, repoURL := range a.Sources {
			sources = append(sources, map[string]any{"repoURL": repoURL, "targetRevision": "main"})
		}
		spec["sources"] = sources
	} else {
		spec["source"] = map[string]any{"repoURL": a.RepoURL, "path": "apps/" + a.Name, "targetRevision": "main"}
	}
	if a.Automated {
		spec["syncPolicy"] = map[string]any{"automated": map[string]any{"prune": true, "selfHeal": true}}
	}

	sync := map[string]any{"status": a.Sync}
	if len(a.Sources) > 0 {
		sync["revisions"] = a.Revisions
	} else {
		sync["revision"] = a.Revision
	}
	status := map[string]any{
		"sync":   sync,
		"health": map[string]any{"status": a.Health, "message": a.HealthMessage},
	}
	if !a.ReconciledAt.IsZero() {
		status["reconciledAt"] = a.ReconciledAt.UTC().Format(time.RFC3339)
	}
	if a.Operation != nil {
		results := make([]any, 0, len(a.Operation.Resources))
		for _, r := range a.Operation.Resources {
			results = append(results, map[string]any{
				"group": r.Group, "kind": r.Kind, "namespace": r.Namespace, "name": r.Name,
				"status": r.Status, "message": r.Message, "hookPhase": r.HookPhase,
			})
		}
		result := map[string]any{"resources": results}
		requested := map[string]any{"revision": a.Operation.Revision}
		if len(a.Sources) > 0 {
			result["revisions"] = a.Operation.Revisions
			requested = map[string]any{"revisions": a.Operation.Revisions}
		} else {
			result["revision"] = a.Operation.Revision
		}
		state := map[string]any{
			"phase":     a.Operation.Phase,
			"message":   a.Operation.Message,
			"startedAt": "2026-01-01T00:00:00Z",
			"operation": map[string]any{"sync": requested},
		}
		if !a.Operation.WithoutResult {
			state["syncResult"] = result
		}
		if !a.Operation.FinishedAt.IsZero() {
			state["finishedAt"] = a.Operation.FinishedAt.UTC().Format(time.RFC3339)
		}
		status["operationState"] = state
	}
	if len(a.Conditions) > 0 {
		conditions := make([]any, 0, len(a.Conditions))
		for _, c := range a.Conditions {
			conditions = append(conditions, map[string]any{"type": c.Type, "message": c.Message})
		}
		status["conditions"] = conditions
	}
	if len(a.Resources) > 0 {
		resources := make([]any, 0, len(a.Resources))
		for _, r := range a.Resources {
			resource := map[string]any{
				"group": r.Group, "version": "v1", "kind": r.Kind, "namespace": r.Namespace, "name": r.Name, "status": r.Status,
			}
			if r.Health != "" {
				resource["health"] = map[string]any{"status": r.Health, "message": r.HealthMessage}
			}
			resources = append(resources, resource)
		}
		status["resources"] = resources
	}
	return map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":            a.Name,
			"namespace":       a.Namespace,
			"resourceVersion": strconv.Itoa(resourceVersion),
		},
		"spec":   spec,
		"status": status,
	}
}
