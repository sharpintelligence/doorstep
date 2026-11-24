package gates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
)

type appCondition int

const (
	appWaiting appCondition = iota
	appReady
	appDegraded
)

type applicationRef struct {
	namespace string
	name      string
}

func applicationOf(app config.App) applicationRef {
	return applicationRef{namespace: app.ArgoCD.Namespace, name: app.ArgoCD.Application}
}

func (a applicationRef) String() string {
	return a.namespace + "/" + a.name
}

type observation struct {
	condition appCondition
	found     bool
	app       Application
	message   string
	failure   string
}

type appEvidence struct {
	Application    string `json:"application"`
	Found          bool   `json:"found"`
	Sync           string `json:"sync,omitempty"`
	Health         string `json:"health,omitempty"`
	HealthMessage  string `json:"health_message,omitempty"`
	Revision       string `json:"revision,omitempty"`
	OperationPhase string `json:"operation_phase,omitempty"`
	DegradedSince  string `json:"degraded_since,omitempty"`
}

func observe(ctx context.Context, reader ApplicationReader, ref applicationRef) (observation, error) {
	app, err := reader.Application(ctx, ref.namespace, ref.name)
	if errors.Is(err, ErrApplicationNotFound) {
		return observation{condition: appWaiting, message: fmt.Sprintf("Argo CD Application %s not found", ref)}, nil
	}
	if err != nil {
		return observation{}, fmt.Errorf("read Argo CD Application %s: %w", ref, err)
	}
	o := observation{found: true, app: app}
	switch {
	case app.Health == HealthDegraded:
		o.condition = appDegraded
		o.message = fmt.Sprintf("Argo CD Application %s is degraded", ref)
		o.failure = fmt.Sprintf("Argo CD Application %s stayed degraded", ref)
	case app.SyncStatus == SyncSynced && app.Health == HealthHealthy && app.OperationPhase != OperationRunning:
		o.condition = appReady
		o.message = fmt.Sprintf("Argo CD Application %s is synced and healthy", ref)
	default:
		o.condition = appWaiting
		o.message = fmt.Sprintf("waiting for Argo CD Application %s: %s", ref, describe(app))
	}
	if o.condition != appReady {
		o.message = withDetail(o.message, app.HealthMessage)
	}
	return o, nil
}

func withDetail(message, detail string) string {
	if detail == "" {
		return message
	}
	return message + ": " + detail
}

func describe(app Application) string {
	parts := []string{"sync " + orUnknown(app.SyncStatus), "health " + orUnknown(app.Health)}
	if app.OperationPhase == OperationRunning {
		parts = append(parts, "operation running")
	}
	return strings.Join(parts, ", ")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func (o observation) evidence(ref applicationRef) appEvidence {
	return appEvidence{
		Application:    ref.String(),
		Found:          o.found,
		Sync:           clean(o.app.SyncStatus),
		Health:         clean(o.app.Health),
		HealthMessage:  clean(o.app.HealthMessage),
		Revision:       clean(o.app.Revision),
		OperationPhase: clean(o.app.OperationPhase),
	}
}

type degradedVerdict struct {
	since  time.Time
	failed bool
}

func judgeDegraded(previousSince string, now time.Time, grace time.Duration) degradedVerdict {
	since, ok := parseTimestamp(previousSince)
	if !ok || since.After(now) {
		since = now
	}
	return degradedVerdict{since: since, failed: now.Sub(since) >= grace}
}

func carriedDegradedSince(previousSince string, now time.Time) string {
	since, ok := parseTimestamp(previousSince)
	if !ok || since.After(now) {
		return ""
	}
	return timestamp(since)
}

func pendingState[T any](previous *core.GateResult) T {
	var state T
	if previous == nil || previous.Status != core.GatePending || len(previous.Evidence) == 0 {
		return state
	}
	if err := json.Unmarshal(previous.Evidence, &state); err != nil {
		var fresh T
		return fresh
	}
	return state
}
