package rollback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const (
	MaxAttempts           = 3
	cancelledByRollback   = "cancelled by rollback request"
	cancelledBlockedQueue = "cancelled by rollback request: commit is blocked"
)

type Observer interface {
	RollbackRequested(app string, trigger Trigger)
}

type Options struct {
	Repository Repository
	Observer   Observer
	Logger     *slog.Logger
}

type Service struct {
	repo     Repository
	observer Observer
	logger   *slog.Logger
}

type Request struct {
	Target           core.Target
	ToDeployment     *core.DeploymentID
	ToDigest         core.Digest
	Reason           string
	ConfirmMigration bool
	Actor            core.Actor
	Role             core.Role
}

type Result struct {
	Rollback   core.Deployment
	RolledBack core.Deployment
	To         core.Deployment
	Block      *core.Block
	Cancelled  []core.DeploymentID
	Migrations []core.Deployment
}

type BlockRequest struct {
	Target core.Target
	Ref    core.BlockRef
	Reason string
	Actor  core.Actor
	Role   core.Role
}

func New(opts Options) (*Service, error) {
	if opts.Repository == nil {
		return nil, errors.New("rollback: repository is required")
	}
	s := &Service{repo: opts.Repository, observer: opts.Observer, logger: opts.Logger}
	if s.observer == nil {
		s.observer = nopObserver{}
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

func (s *Service) Rollback(ctx context.Context, req Request) (Result, error) {
	req.Reason = strings.TrimSpace(req.Reason)
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	var (
		result Result
		err    error
	)
	for range MaxAttempts {
		result, err = s.rollbackOnce(ctx, req)
		if !errors.Is(err, core.ErrStateConflict) {
			break
		}
	}
	if err != nil {
		return Result{}, fmt.Errorf("roll back %s: %w", req.Target, err)
	}
	attrs := []any{
		slog.String("app", req.Target.String()),
		slog.Int64("rollback_id", int64(result.Rollback.ID)),
		slog.Int64("rolled_back_id", int64(result.RolledBack.ID)),
		slog.Int64("to_id", int64(result.To.ID)),
		slog.String("digest", string(result.To.Digest)),
		slog.String("actor", req.Actor.String()),
	}
	if result.Block != nil {
		attrs = append(attrs, slog.String("blocked", result.Block.Subject()))
	}
	if len(result.Migrations) > 0 {
		attrs = append(attrs, slog.Any("migration_deployment_ids", deploymentIDs(result.Migrations)))
	}
	s.logger.InfoContext(ctx, "rollback requested", attrs...)
	s.observer.RollbackRequested(req.Target.App, TriggerManual)
	return result, nil
}

func (r Request) validate() error {
	errs := []error{r.Target.Validate(), r.Actor.Validate()}
	if r.ToDeployment != nil && r.ToDigest != "" {
		errs = append(errs, errors.New("name at most one of a deployment and a digest to roll back to"))
	}
	if r.ToDeployment != nil && *r.ToDeployment <= 0 {
		errs = append(errs, fmt.Errorf("deployment id %d must be positive", *r.ToDeployment))
	}
	if r.ToDigest != "" {
		errs = append(errs, r.ToDigest.Validate())
	}
	errs = append(errs, core.ValidateBlockReason(r.Reason))
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", core.ErrInvalidRollback, err)
	}
	return nil
}

func (s *Service) rollbackOnce(ctx context.Context, req Request) (Result, error) {
	var result Result
	err := s.repo.InTx(ctx, func(tx Repository) error {
		if err := tx.LockTarget(ctx, req.Target); err != nil {
			return err
		}
		if err := openRollback(ctx, tx, req.Target); err != nil {
			return err
		}
		bad, err := tx.CurrentDeployment(ctx, req.Target)
		if errors.Is(err, core.ErrNotFound) {
			return fmt.Errorf("%w: nothing has been deployed to %s", core.ErrNoRollbackTarget, req.Target)
		}
		if err != nil {
			return err
		}
		to, err := selectTarget(ctx, tx, req, bad)
		if err != nil {
			return err
		}
		crossed, err := tx.MigrationsBetween(ctx, req.Target, to, bad)
		if err != nil {
			return err
		}
		if len(crossed) > 0 && !req.ConfirmMigration {
			return &core.MigrationRollbackError{To: to, Migrations: crossed}
		}

		actor := req.Actor.String()
		reason := req.Reason
		if reason == "" {
			reason = "rolled back by " + actor
		}
		var cancelled []core.DeploymentID
		if !bad.State.Terminal() {
			if _, err := tx.TransitionDeployment(ctx, store.Transition{
				ID: bad.ID, From: bad.State, To: core.StateCancelled, Actor: actor, Message: cancelledByRollback,
			}); err != nil {
				return err
			}
			cancelled = append(cancelled, bad.ID)
		}

		rb, _, err := tx.EnqueueDeployment(ctx, core.Deployment{
			Target:          req.Target,
			Kind:            core.KindRollback,
			Source:          core.SourceAPI,
			RequestedBy:     actor,
			Commit:          to.Commit,
			Image:           to.Image,
			Digest:          to.Digest,
			RollbackOfID:    &bad.ID,
			Migration:       len(crossed) > 0,
			MigrationReason: crossingReason(crossed),
		}, core.PriorityRollback)
		if err != nil {
			return err
		}
		recorded, ids, err := blockReplaced(ctx, tx, blockFor(req.Target, reason, req.Actor, bad, to), rb.ID)
		if err != nil {
			return err
		}
		cancelled = append(cancelled, ids...)

		bad, err = tx.GetDeployment(ctx, bad.ID)
		if err != nil {
			return err
		}
		result = Result{Rollback: rb, RolledBack: bad, To: to, Block: recorded, Cancelled: cancelled, Migrations: crossed}
		entry, err := rollbackAudit(req, result)
		if err != nil {
			return err
		}
		return tx.RecordAudit(ctx, entry)
	})
	return result, err
}

func blockReplaced(ctx context.Context, tx Repository, block core.Block, rollbackID core.DeploymentID) (*core.Block, []core.DeploymentID, error) {
	var cancelled []core.DeploymentID
	if block.Commit.SHA != "" {
		ids, err := cancelQueuedDeploys(ctx, tx, block.Target, block.Commit.SHA, *block.DeploymentID, block.CreatedBy)
		if err != nil {
			return nil, nil, err
		}
		cancelled = ids
	}
	if block.Digest == "" && block.Commit.SHA == "" {
		return nil, cancelled, nil
	}
	block.RollbackID = &rollbackID
	recorded, _, err := tx.CreateBlock(ctx, block)
	if err != nil {
		return nil, nil, err
	}
	return &recorded, cancelled, nil
}

func openRollback(ctx context.Context, tx Repository, t core.Target) error {
	page, err := tx.ListDeployments(ctx, store.DeploymentFilter{
		App:         t.App,
		Environment: t.Environment,
		Kinds:       []core.Kind{core.KindRollback},
		States:      core.ClaimableStates(),
		Limit:       1,
	})
	if err != nil {
		return err
	}
	if len(page.Items) > 0 {
		return &core.RollbackInProgressError{Deployment: page.Items[0]}
	}
	return nil
}

func selectTarget(ctx context.Context, tx Repository, req Request, bad core.Deployment) (core.Deployment, error) {
	if req.ToDeployment == nil && req.ToDigest == "" {
		to, err := tx.LastSuccessfulDeployment(ctx, req.Target, bad.Digest)
		if errors.Is(err, core.ErrNotFound) {
			return core.Deployment{}, fmt.Errorf("%w: no earlier successful deployment of %s with another unblocked digest", core.ErrNoRollbackTarget, req.Target)
		}
		return to, err
	}

	var to core.Deployment
	if req.ToDeployment != nil {
		d, err := tx.GetDeployment(ctx, *req.ToDeployment)
		if errors.Is(err, core.ErrNotFound) {
			return core.Deployment{}, fmt.Errorf("%w: deployment %d does not exist", core.ErrInvalidRollback, *req.ToDeployment)
		}
		if err != nil {
			return core.Deployment{}, err
		}
		to = d
	} else {
		page, err := tx.ListDeployments(ctx, store.DeploymentFilter{
			App:         req.Target.App,
			Environment: req.Target.Environment,
			States:      []core.State{core.StateSucceeded},
			Digest:      req.ToDigest,
			Limit:       1,
		})
		if err != nil {
			return core.Deployment{}, err
		}
		if len(page.Items) == 0 {
			return core.Deployment{}, fmt.Errorf("%w: %s never deployed %s successfully", core.ErrInvalidRollback, req.Target, req.ToDigest)
		}
		to = page.Items[0]
	}

	switch {
	case to.Target != req.Target:
		return core.Deployment{}, fmt.Errorf("%w: deployment %d belongs to %s, not %s", core.ErrInvalidRollback, to.ID, to.Target, req.Target)
	case to.State != core.StateSucceeded:
		return core.Deployment{}, fmt.Errorf("%w: deployment %d is %s; only a succeeded deployment can be rolled back to", core.ErrInvalidRollback, to.ID, to.State)
	case to.Digest == "":
		return core.Deployment{}, fmt.Errorf("%w: deployment %d has no digest", core.ErrInvalidRollback, to.ID)
	case to.ID == bad.ID, to.Digest == bad.Digest:
		return core.Deployment{}, fmt.Errorf("%w: %s is what deployment %d runs; pick an earlier version", core.ErrInvalidRollback, to.Digest, bad.ID)
	}
	block, found, err := tx.ActiveBlock(ctx, req.Target, core.BlockRef{Digest: to.Digest})
	if err != nil {
		return core.Deployment{}, err
	}
	if found {
		return core.Deployment{}, &core.BlockedError{Block: block}
	}
	return to, nil
}

func blockFor(t core.Target, reason string, actor core.Actor, bad, to core.Deployment) core.Block {
	b := core.Block{
		Target:       t,
		Digest:       bad.Digest,
		Reason:       reason,
		CreatedBy:    actor.String(),
		DeploymentID: &bad.ID,
	}
	if bad.Commit.SHA != to.Commit.SHA {
		b.Commit = bad.Commit
	}
	return b
}

func cancelQueuedDeploys(ctx context.Context, tx Repository, t core.Target, sha string, skip core.DeploymentID, actor string) ([]core.DeploymentID, error) {
	page, err := tx.ListDeployments(ctx, store.DeploymentFilter{
		App:         t.App,
		Environment: t.Environment,
		Kinds:       []core.Kind{core.KindDeploy},
		States:      []core.State{core.StateQueued},
		CommitSHA:   sha,
		Limit:       store.MaxPageSize,
	})
	if err != nil {
		return nil, err
	}
	var ids []core.DeploymentID
	for _, d := range page.Items {
		if d.ID == skip {
			continue
		}
		if _, err := tx.TransitionDeployment(ctx, store.Transition{
			ID: d.ID, From: core.StateQueued, To: core.StateCancelled, Actor: actor, Message: cancelledBlockedQueue,
		}); err != nil {
			return nil, err
		}
		ids = append(ids, d.ID)
	}
	return ids, nil
}

func rollbackAudit(req Request, r Result) (core.AuditEntry, error) {
	detail := rollbackDetail(r, req.Reason)
	if len(r.Migrations) > 0 {
		detail["migration_confirmed"] = true
		detail["migration_deployment_ids"] = deploymentIDs(r.Migrations)
	}
	return rollbackEntry(req.Target, req.Actor, req.Role, r.Rollback.ID, detail)
}

func rollbackEntry(t core.Target, actor core.Actor, role core.Role, rollbackID core.DeploymentID, detail map[string]any) (core.AuditEntry, error) {
	entry, err := auditEntry(core.AuditRollbackRequest, t, actor, role, detail)
	entry.DeploymentID = &rollbackID
	return entry, err
}

func rollbackDetail(r Result, reason string) map[string]any {
	detail := map[string]any{
		"rolled_back_deployment_id": r.RolledBack.ID,
		"to_deployment_id":          r.To.ID,
		"digest":                    r.To.Digest,
		"cancelled":                 nonNil(r.Cancelled),
	}
	if r.Block != nil {
		detail["block_id"] = r.Block.ID
		if r.Block.Digest != "" {
			detail["blocked_digest"] = r.Block.Digest
		}
		if r.Block.Commit.SHA != "" {
			detail["blocked_commit"] = r.Block.Commit.SHA
		}
	}
	if reason != "" {
		detail["reason"] = reason
	}
	return detail
}

func (s *Service) Block(ctx context.Context, req BlockRequest) (core.Block, bool, error) {
	req.Reason = strings.TrimSpace(req.Reason)
	errs := []error{req.Target.Validate(), req.Ref.Validate(), req.Actor.Validate(), core.ValidateBlockReason(req.Reason)}
	if req.Reason == "" {
		errs = append(errs, errors.New("reason is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return core.Block{}, false, fmt.Errorf("%w: %w", core.ErrInvalidBlock, err)
	}
	var (
		block   core.Block
		created bool
	)
	err := s.repo.InTx(ctx, func(tx Repository) error {
		var err error
		block, created, err = tx.CreateBlock(ctx, core.Block{
			Target:    req.Target,
			Digest:    req.Ref.Digest,
			Commit:    core.Commit{SHA: req.Ref.CommitSHA},
			Reason:    req.Reason,
			CreatedBy: req.Actor.String(),
		})
		if err != nil || !created {
			return err
		}
		entry, err := auditEntry(core.AuditBlockCreate, req.Target, req.Actor, req.Role, map[string]any{
			"block_id": block.ID,
			"ref":      req.Ref.String(),
			"reason":   req.Reason,
		})
		if err != nil {
			return err
		}
		return tx.RecordAudit(ctx, entry)
	})
	if err != nil {
		return core.Block{}, false, err
	}
	if created {
		s.logger.InfoContext(ctx, "blocked",
			slog.String("app", req.Target.String()),
			slog.String("subject", block.Subject()),
			slog.String("actor", req.Actor.String()),
			slog.String("reason", req.Reason),
		)
	}
	return block, created, nil
}

func (s *Service) Unblock(ctx context.Context, t core.Target, ref core.BlockRef, actor core.Actor, role core.Role) ([]core.Block, error) {
	if err := errors.Join(t.Validate(), ref.Validate(), actor.Validate()); err != nil {
		return nil, fmt.Errorf("%w: %w", core.ErrInvalidBlock, err)
	}
	var cleared []core.Block
	err := s.repo.InTx(ctx, func(tx Repository) error {
		var err error
		cleared, err = tx.ClearBlocks(ctx, t, ref, actor.String())
		if err != nil {
			return err
		}
		ids := make([]core.BlockID, 0, len(cleared))
		for _, b := range cleared {
			ids = append(ids, b.ID)
		}
		entry, err := auditEntry(core.AuditBlockClear, t, actor, role, map[string]any{
			"block_ids": ids,
			"ref":       ref.String(),
		})
		if err != nil {
			return err
		}
		return tx.RecordAudit(ctx, entry)
	})
	if err != nil {
		return nil, err
	}
	for _, b := range cleared {
		s.logger.InfoContext(ctx, "unblocked",
			slog.String("app", t.String()),
			slog.String("subject", b.Subject()),
			slog.String("actor", actor.String()),
		)
	}
	return cleared, nil
}

func (s *Service) MigrationsCrossed(ctx context.Context, t core.Target, to, current core.Deployment) ([]core.Deployment, error) {
	return s.repo.MigrationsBetween(ctx, t, to, current)
}

func (s *Service) ListBlocks(ctx context.Context, t core.Target) ([]core.Block, error) {
	return s.repo.ListBlocks(ctx, t, false)
}

func (s *Service) Blocked(ctx context.Context, t core.Target, commit core.Commit, digest core.Digest) (core.Block, bool, error) {
	return s.repo.ActiveBlock(ctx, t, core.BlockRef{Digest: digest, CommitSHA: commit.SHA})
}

func (s *Service) PendingTrailers(ctx context.Context, t core.Target) (core.BlockTrailers, error) {
	return s.repo.PendingBlockTrailers(ctx, t)
}

func (s *Service) MarkMirrored(ctx context.Context, t core.Target, trailers core.BlockTrailers, commit core.Commit) error {
	return s.repo.MarkBlocksMirrored(ctx, t, trailers, commit)
}

func auditEntry(action core.AuditAction, t core.Target, actor core.Actor, role core.Role, detail map[string]any) (core.AuditEntry, error) {
	data, err := json.Marshal(detail)
	if err != nil {
		return core.AuditEntry{}, fmt.Errorf("encode %s audit detail: %w", action, err)
	}
	return core.AuditEntry{Actor: actor, Role: role, Action: action, Target: &t, Detail: data}, nil
}

func crossingReason(crossed []core.Deployment) string {
	if len(crossed) == 0 {
		return ""
	}
	ids := make([]string, len(crossed))
	for i, d := range crossed {
		ids[i] = strconv.FormatInt(int64(d.ID), 10)
	}
	word := "deployment "
	if len(crossed) > 1 {
		word = "deployments "
	}
	return core.CleanMigrationReason("rolls back across " + word + strings.Join(ids, ", "))
}

func deploymentIDs(ds []core.Deployment) []core.DeploymentID {
	ids := make([]core.DeploymentID, len(ds))
	for i, d := range ds {
		ids[i] = d.ID
	}
	return ids
}

func nonNil(ids []core.DeploymentID) []core.DeploymentID {
	if ids == nil {
		return []core.DeploymentID{}
	}
	return ids
}

type nopObserver struct{}

func (nopObserver) RollbackRequested(string, Trigger) {}
