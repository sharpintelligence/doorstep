package rollback

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type Trigger string

const (
	TriggerManual     Trigger = "manual"
	TriggerGateFailed Trigger = "gate_failed"
	TriggerSyncFailed Trigger = "sync_failed"
)

type Outcome string

const (
	OutcomeQueued             Outcome = "queued"
	OutcomeWithheldMigrations Outcome = "withheld_migrations"
	OutcomeNoTarget           Outcome = "no_target"
	OutcomeSameVersion        Outcome = "same_version"
	OutcomeRollbackInProgress Outcome = "rollback_in_progress"
)

type Plan struct {
	Outcome    Outcome
	Rollback   *core.Deployment
	To         *core.Deployment
	Migrations []core.Deployment
}

type AutomaticRequest struct {
	Deployment core.Deployment
	Trigger    Trigger
	Reason     string
	Actor      core.Actor
	Fail       func(Plan) (store.Transition, error)
}

type AutomaticResult struct {
	Plan      Plan
	Failed    core.Deployment
	Block     *core.Block
	Cancelled []core.DeploymentID
}

var errNotFailed = errors.New("the failure transition must move the failing deployment to failed")

func (s *Service) Automatic(ctx context.Context, req AutomaticRequest) (AutomaticResult, error) {
	if err := req.validate(); err != nil {
		return AutomaticResult{}, err
	}
	var (
		result AutomaticResult
		err    error
	)
	for range MaxAttempts {
		result, err = s.automaticOnce(ctx, req)
		if !errors.Is(err, core.ErrStateConflict) {
			break
		}
	}
	d := req.Deployment
	if err != nil {
		return AutomaticResult{}, fmt.Errorf("automatic rollback of deployment %d of %s: %w", d.ID, d.Target, err)
	}
	attrs := []any{
		slog.String("app", d.Target.String()),
		slog.Int64("failed_id", int64(d.ID)),
		slog.String("trigger", string(req.Trigger)),
		slog.String("outcome", string(result.Plan.Outcome)),
	}
	if result.Plan.To != nil {
		attrs = append(attrs, slog.Int64("to_id", int64(result.Plan.To.ID)), slog.String("digest", string(result.Plan.To.Digest)))
	}
	if len(result.Plan.Migrations) > 0 {
		attrs = append(attrs, slog.Any("migration_deployment_ids", deploymentIDs(result.Plan.Migrations)))
	}
	if result.Plan.Outcome != OutcomeQueued {
		s.logger.WarnContext(ctx, "automatic rollback withheld", attrs...)
		return result, nil
	}
	attrs = append(attrs, slog.Int64("rollback_id", int64(result.Plan.Rollback.ID)))
	if result.Block != nil {
		attrs = append(attrs, slog.String("blocked", result.Block.Subject()))
	}
	s.logger.WarnContext(ctx, "automatic rollback queued", attrs...)
	s.observer.RollbackRequested(d.Target.App, req.Trigger)
	return result, nil
}

func (r AutomaticRequest) validate() error {
	d := r.Deployment
	errs := []error{d.Target.Validate(), r.Actor.Validate(), core.ValidateBlockReason(r.Reason)}
	if d.ID <= 0 {
		errs = append(errs, fmt.Errorf("deployment id %d must be positive", d.ID))
	}
	if d.Kind != core.KindDeploy {
		errs = append(errs, fmt.Errorf("only a deploy is rolled back automatically, not a %s", d.Kind))
	}
	if d.State.Terminal() {
		errs = append(errs, fmt.Errorf("deployment %d is already %s", d.ID, d.State))
	}
	if r.Trigger != TriggerGateFailed && r.Trigger != TriggerSyncFailed {
		errs = append(errs, fmt.Errorf("unknown automatic rollback trigger %q", r.Trigger))
	}
	if r.Fail == nil {
		errs = append(errs, errors.New("a failure transition is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", core.ErrInvalidRollback, err)
	}
	return nil
}

func (s *Service) automaticOnce(ctx context.Context, req AutomaticRequest) (AutomaticResult, error) {
	var result AutomaticResult
	err := s.repo.InTx(ctx, func(tx Repository) error {
		bad := req.Deployment
		if err := tx.LockTarget(ctx, bad.Target); err != nil {
			return err
		}
		plan, err := planAutomatic(ctx, tx, bad)
		if err != nil {
			return err
		}
		if plan.Outcome != OutcomeQueued {
			failed, err := failDeployment(ctx, tx, req, plan)
			result = AutomaticResult{Plan: plan, Failed: failed}
			return err
		}

		actor := req.Actor.String()
		to := *plan.To
		rb, _, err := tx.EnqueueDeployment(ctx, core.Deployment{
			Target:       bad.Target,
			Kind:         core.KindRollback,
			Source:       core.SourceSystem,
			RequestedBy:  actor,
			Commit:       to.Commit,
			Image:        to.Image,
			Digest:       to.Digest,
			RollbackOfID: &bad.ID,
		}, core.PriorityRollback)
		if err != nil {
			return err
		}
		plan.Rollback = &rb
		failed, err := failDeployment(ctx, tx, req, plan)
		if err != nil {
			return err
		}
		block, cancelled, err := blockReplaced(ctx, tx, blockFor(bad.Target, req.Reason, req.Actor, bad, to), rb.ID)
		if err != nil {
			return err
		}
		result = AutomaticResult{Plan: plan, Failed: failed, Block: block, Cancelled: cancelled}
		detail := rollbackDetail(Result{Rollback: rb, RolledBack: failed, To: to, Block: block, Cancelled: cancelled}, req.Reason)
		detail["automatic"] = true
		detail["trigger"] = req.Trigger
		entry, err := rollbackEntry(bad.Target, req.Actor, "", rb.ID, detail)
		if err != nil {
			return err
		}
		return tx.RecordAudit(ctx, entry)
	})
	return result, err
}

func planAutomatic(ctx context.Context, tx Repository, bad core.Deployment) (Plan, error) {
	err := openRollback(ctx, tx, bad.Target)
	if _, ok := errors.AsType[*core.RollbackInProgressError](err); ok {
		return Plan{Outcome: OutcomeRollbackInProgress}, nil
	}
	if err != nil {
		return Plan{}, err
	}
	to, err := tx.LastSuccessfulDeployment(ctx, bad.Target, "")
	if errors.Is(err, core.ErrNotFound) {
		return Plan{Outcome: OutcomeNoTarget}, nil
	}
	if err != nil {
		return Plan{}, err
	}
	if to.Digest == bad.Digest {
		return Plan{Outcome: OutcomeSameVersion, To: &to}, nil
	}
	crossed, err := tx.MigrationsBetween(ctx, bad.Target, to, bad)
	if err != nil {
		return Plan{}, err
	}
	if len(crossed) > 0 {
		return Plan{Outcome: OutcomeWithheldMigrations, To: &to, Migrations: crossed}, nil
	}
	return Plan{Outcome: OutcomeQueued, To: &to}, nil
}

func failDeployment(ctx context.Context, tx Repository, req AutomaticRequest, plan Plan) (core.Deployment, error) {
	t, err := req.Fail(plan)
	if err != nil {
		return core.Deployment{}, err
	}
	if t.ID != req.Deployment.ID || t.To != core.StateFailed {
		return core.Deployment{}, fmt.Errorf("%w: got deployment %d -> %s", errNotFailed, t.ID, t.To)
	}
	return tx.TransitionDeployment(ctx, t)
}
