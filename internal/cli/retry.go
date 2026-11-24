package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/engine"
	"github.com/sharpintelligence/doorstep/internal/pods"
	"github.com/sharpintelligence/doorstep/internal/retry"
	"github.com/sharpintelligence/doorstep/internal/store"
)

func newRetryPolicy(in engineInput) (engine.FailurePolicy, error) {
	opts := retry.Options{
		Settings: in.apps.RetrySettings,
		History:  retryHistory{store: in.store},
		Queue:    retryQueue{store: in.store},
		Logger:   in.logger,
	}
	if in.config.Kubernetes.PodDiagnostics {
		client, err := newKubeClient(in.config.Kubernetes, in.lookupEnv, os.ReadFile)
		if err != nil {
			return nil, fmt.Errorf("pod diagnostics: %w", err)
		}
		reader, err := pods.New(client)
		if err != nil {
			return nil, fmt.Errorf("pod diagnostics: %w", err)
		}
		opts.Diagnostics = podDiagnostics{workloads: in.watcher, pods: reader}
	}
	return retry.New(opts)
}

type workloadLister interface {
	Workloads(ctx context.Context, t core.Target) ([]core.Workload, error)
}

type podLister interface {
	Pods(ctx context.Context, workloads []core.Workload) ([]core.PodStatus, error)
}

type podDiagnostics struct {
	workloads workloadLister
	pods      podLister
}

func (p podDiagnostics) Pods(ctx context.Context, d core.Deployment) ([]core.PodStatus, error) {
	workloads, err := p.workloads.Workloads(ctx, d.Target)
	if err != nil {
		return nil, fmt.Errorf("list the workloads of %s: %w", d.Target, err)
	}
	if len(workloads) == 0 {
		return nil, nil
	}
	return p.pods.Pods(ctx, workloads)
}

type diagnosesReader interface {
	Diagnoses(ctx context.Context, id core.DeploymentID, window time.Duration, limit int) (store.Diagnoses, error)
}

type retryHistory struct {
	store diagnosesReader
}

func (h retryHistory) Diagnoses(ctx context.Context, id core.DeploymentID) ([]json.RawMessage, []json.RawMessage, error) {
	found, err := h.store.Diagnoses(ctx, id, store.DefaultDiagnosisWindow, store.MaxDiagnoses)
	if err != nil {
		return nil, nil, err
	}
	return found.Deployment, found.Recent, nil
}

type queueReader interface {
	QueueEntries(ctx context.Context, app string) ([]core.QueueEntry, error)
}

type retryQueue struct {
	store queueReader
}

func (q retryQueue) Waiting(ctx context.Context, t core.Target, except core.DeploymentID) (bool, error) {
	entries, err := q.store.QueueEntries(ctx, t.App)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Deployment.Target == t && e.Deployment.ID != except {
			return true, nil
		}
	}
	return false, nil
}
