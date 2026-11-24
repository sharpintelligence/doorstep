package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/sharpintelligence/doorstep/internal/argocd"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/intake"
	"github.com/sharpintelligence/doorstep/internal/locks"
	"github.com/sharpintelligence/doorstep/internal/metrics"
	"github.com/sharpintelligence/doorstep/internal/notify"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type gateDeps struct {
	store     *store.Store
	githubApp *github.App
	locks     *locks.Service
	watcher   *argocd.Watcher
	catalog   config.Catalog
	config    config.Config
	recorder  *metrics.Recorder
	logger    *slog.Logger
	lookupEnv LookupEnv

	statusMirror *notify.GateStatuses
}

type gateRegistrar func(r *gates.Registry, deps gateDeps) error

func gateRegistrars() []gateRegistrar {
	return []gateRegistrar{registerBuiltinGates, registerPrometheusGateFromDeps, registerExternalGateFromDeps}
}

func gateObservers(deps gateDeps) []gates.Observer {
	if deps.statusMirror == nil {
		return nil
	}
	return []gates.Observer{deps.statusMirror}
}

func registerBuiltinGates(r *gates.Registry, deps gateDeps) error {
	return gates.RegisterBuiltins(r, gates.Builtins{
		Tracked:      deps.store,
		Live:         intake.LiveChecks{Source: intake.GitHub{Clients: deps.githubApp}},
		Freezes:      deps.locks,
		Applications: newArgoApplications(deps.watcher, deps.catalog),
	})
}

func registerPrometheusGateFromDeps(r *gates.Registry, deps gateDeps) error {
	return registerPrometheusGate(r, deps.catalog, secretSource{lookupEnv: deps.lookupEnv, readFile: config.ReadSecretFile})
}

func registerExternalGateFromDeps(r *gates.Registry, deps gateDeps) error {
	return gates.RegisterExternal(r, deps.store)
}

func newGateEvaluator(deps gateDeps) (*gates.Evaluator, error) {
	registry := gates.NewRegistry()
	for _, register := range gateRegistrars() {
		if err := register(registry, deps); err != nil {
			return nil, err
		}
	}
	plans, err := registry.Plans(deps.catalog)
	if err != nil {
		return nil, fmt.Errorf("gates: %w", err)
	}
	return gates.NewEvaluator(gates.Options{
		Store:     deps.store,
		Plans:     plans,
		Metrics:   deps.recorder,
		Observers: gateObservers(deps),
		Logger:    deps.logger,
	})
}

type syncStatusReader interface {
	Status(ctx context.Context, t core.Target) (core.SyncStatus, error)
}

type argoApplications struct {
	watcher syncStatusReader
	targets map[string]core.Target
}

func newArgoApplications(watcher syncStatusReader, catalog config.Catalog) argoApplications {
	targets := make(map[string]core.Target, len(catalog.Apps))
	for _, a := range catalog.Apps {
		w := appWiring{app: a}
		app := w.argocd()
		targets[app.String()] = w.target()
	}
	return argoApplications{watcher: watcher, targets: targets}
}

func (a argoApplications) Application(ctx context.Context, namespace, name string) (gates.Application, error) {
	key := argocd.Application{Namespace: namespace, Name: name}.String()
	t, ok := a.targets[key]
	if !ok {
		return gates.Application{}, fmt.Errorf("%w: %s is not configured for any app", gates.ErrApplicationNotFound, key)
	}
	status, err := a.watcher.Status(ctx, t)
	if errors.Is(err, core.ErrSyncTargetNotFound) {
		return gates.Application{}, fmt.Errorf("%w: %w", gates.ErrApplicationNotFound, err)
	}
	if err != nil {
		return gates.Application{}, err
	}
	return gates.Application{
		SyncStatus:     gateSyncStatus(status.State),
		Revision:       status.Revision,
		Health:         gateHealth(status.Health.Status),
		HealthMessage:  status.Health.Message,
		OperationPhase: status.OperationPhase,
	}, nil
}

func gateSyncStatus(s core.SyncState) string {
	switch s {
	case core.SyncSynced:
		return gates.SyncSynced
	case core.SyncOutOfSync:
		return gates.SyncOutOfSync
	case core.SyncUnknown:
		return gates.SyncUnknown
	default:
		return gates.SyncUnknown
	}
}

func gateHealth(h core.HealthStatus) string {
	switch h {
	case core.HealthHealthy:
		return gates.HealthHealthy
	case core.HealthProgressing:
		return gates.HealthProgressing
	case core.HealthDegraded:
		return gates.HealthDegraded
	case core.HealthSuspended:
		return gates.HealthSuspended
	case core.HealthMissing:
		return gates.HealthMissing
	case core.HealthUnknown:
		return gates.HealthUnknown
	default:
		return gates.HealthUnknown
	}
}
