package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	withheldSuffix    = "; automatic rollback withheld: release contains database migrations; roll back with --confirm-migration-rollback"
	shortDigestLength = 12
)

type autoRollbackData struct {
	Outcome                RollbackOutcome     `json:"outcome"`
	RollbackID             *core.DeploymentID  `json:"rollback_id,omitempty"`
	ToDeploymentID         *core.DeploymentID  `json:"to_deployment_id,omitempty"`
	Digest                 core.Digest         `json:"digest,omitempty"`
	MigrationDeploymentIDs []core.DeploymentID `json:"migration_deployment_ids,omitempty"`
}

func (e *Engine) failAndRollBack(ctx context.Context, d core.Deployment, f Failure, decision Decision) (core.Deployment, Result, error) {
	trigger := RollbackTriggerSyncFailed
	if f.Stage == core.StateVerifying {
		trigger = RollbackTriggerGateFailed
	}
	result, err := e.rollbacks.FailAndRollBack(ctx, AutoRollbackRequest{
		Deployment: d,
		Trigger:    trigger,
		Reason:     blockReason(d.ID, f.Reason),
		Fail: func(plan RollbackPlan) (Transition, error) {
			return failureTransition(f, decision, &plan)
		},
	})
	if err != nil {
		return d, Result{}, err
	}
	plan := result.Plan
	attrs := []any{slog.String("outcome", string(plan.Outcome)), slog.String("trigger", string(trigger))}
	if plan.Rollback != nil {
		attrs = append(attrs, slog.Int64("rollback_id", int64(plan.Rollback.ID)))
	}
	if plan.To != nil {
		attrs = append(attrs, slog.Int64("to_id", int64(plan.To.ID)), slog.String("digest", string(plan.To.Digest)))
	}
	if len(plan.Migrations) > 0 {
		attrs = append(attrs, slog.Any("migration_deployment_ids", plan.Migrations))
	}
	e.logFor(d).WarnContext(ctx, "automatic rollback", attrs...)
	e.announceFailure(ctx, result.Failed, f, decision, rollbackMessage(plan))
	if plan.Outcome == RollbackQueued && plan.Rollback != nil {
		e.Queued(ctx, plan.Rollback.ID)
		e.Cancelled(ctx, result.Cancelled...)
	}
	return result.Failed, Result{}, nil
}

func autoRollbackFor(plan *RollbackPlan) *autoRollbackData {
	if plan == nil {
		return nil
	}
	data := &autoRollbackData{Outcome: plan.Outcome, MigrationDeploymentIDs: plan.Migrations}
	if plan.Rollback != nil {
		data.RollbackID = &plan.Rollback.ID
	}
	if plan.To != nil {
		data.ToDeploymentID = &plan.To.ID
		data.Digest = plan.To.Digest
	}
	return data
}

func rollbackMessage(plan RollbackPlan) string {
	switch plan.Outcome {
	case RollbackQueued:
		message := "rolling back automatically"
		if plan.To != nil {
			message += fmt.Sprintf(" to %s (deployment %d)", shortDigest(plan.To.Digest), plan.To.ID)
		}
		if plan.Rollback != nil {
			message += fmt.Sprintf(" as deployment %d", plan.Rollback.ID)
		}
		return message
	case RollbackWithheldMigrations:
		return "automatic rollback withheld: release contains database migrations" + inDeployments(plan.Migrations) + "; roll back with --confirm-migration-rollback"
	case RollbackNoTarget:
		return "automatic rollback not possible: no earlier successful deployment"
	case RollbackSameVersion:
		return "automatic rollback skipped: this digest is the last one that succeeded"
	case RollbackInProgress:
		return "automatic rollback skipped: a rollback is already in progress"
	}
	return "automatic rollback: " + string(plan.Outcome)
}

func inDeployments(ids []core.DeploymentID) string {
	if len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(int64(id), 10)
	}
	word := "deployment "
	if len(ids) > 1 {
		word = "deployments "
	}
	return " (" + word + strings.Join(parts, ", ") + ")"
}

func withheldReason(reason string) string {
	return truncate(reason, maxFailureReason-len([]rune(withheldSuffix))) + withheldSuffix
}

func blockReason(id core.DeploymentID, reason string) string {
	return truncate(fmt.Sprintf("automatic rollback of deployment %d: %s", id, reason), core.MaxBlockReasonLength)
}

func shortDigest(d core.Digest) string {
	algorithm, hex, ok := strings.Cut(string(d), ":")
	if !ok || len(hex) <= shortDigestLength {
		return string(d)
	}
	return algorithm + ":" + hex[:shortDigestLength]
}
