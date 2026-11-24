package engine

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/sharpintelligence/doorstep/internal/core"
)

func (e *Engine) Queued(ctx context.Context, id core.DeploymentID) {
	d, ok := e.load(ctx, id, "queued")
	if !ok {
		return
	}
	e.notifier.Notify(ctx, core.Notification{
		Kind:       core.NotificationDeployQueued,
		Target:     d.Target,
		Deployment: &d,
		RollbackOf: e.rollbackOf(ctx, d),
		Actor:      d.RequestedBy,
		OccurredAt: e.now(),
	})
}

func (e *Engine) Cancelled(ctx context.Context, ids ...core.DeploymentID) {
	for _, id := range ids {
		d, ok := e.load(ctx, id, "cancelled")
		if !ok || d.State != core.StateCancelled {
			continue
		}
		e.metrics.DeploymentFinished(d.Target.App, OutcomeCancelled, runDuration(d))
		e.notify(ctx, core.NotificationDeployCancelled, d, "")
		if d.StartedAt != nil {
			e.abandon(ctx, d)
		}
	}
}

func (e *Engine) announceSuperseded(ctx context.Context, id core.DeploymentID) {
	d, ok := e.load(ctx, id, "superseded")
	if !ok || d.State != core.StateSuperseded {
		return
	}
	message := ""
	if d.SupersededByID != nil {
		message = "superseded by deployment " + strconv.FormatInt(int64(*d.SupersededByID), 10)
	}
	e.metrics.DeploymentFinished(d.Target.App, OutcomeSuperseded, runDuration(d))
	e.notify(ctx, core.NotificationDeploySuperseded, d, message)
}

func (e *Engine) load(ctx context.Context, id core.DeploymentID, event string) (core.Deployment, bool) {
	d, err := e.store.Get(ctx, id)
	if err != nil {
		e.logger.WarnContext(ctx, "load deployment to announce",
			slog.Int64("deployment_id", int64(id)),
			slog.String("event", event),
			slog.String("error", err.Error()),
		)
		return core.Deployment{}, false
	}
	return d, true
}

func (e *Engine) notify(ctx context.Context, kind core.NotificationKind, d core.Deployment, message string) {
	n := core.Notification{
		Kind:       kind,
		Target:     d.Target,
		Deployment: &d,
		RollbackOf: e.rollbackOf(ctx, d),
		Actor:      Actor,
		Message:    message,
		OccurredAt: e.now(),
	}
	if kind == core.NotificationDeployRolledBack && n.RollbackOf == nil {
		n.Kind = core.NotificationDeploySucceeded
	}
	e.notifier.Notify(ctx, n)
}

func (e *Engine) rollbackOf(ctx context.Context, d core.Deployment) *core.Deployment {
	if d.Kind != core.KindRollback || d.RollbackOfID == nil {
		return nil
	}
	of, ok := e.load(ctx, *d.RollbackOfID, "rollback of")
	if !ok || of.Target != d.Target {
		return nil
	}
	return &of
}
