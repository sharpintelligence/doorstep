package deploys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const (
	MaxAttempts     = 3
	MaxReasonLength = 500
)

var (
	ErrUnknownApp        = errors.New("unknown app")
	ErrInvalidRequest    = errors.New("invalid deploy request")
	ErrCommitNotFound    = errors.New("commit not found")
	ErrCommitNotOnBranch = errors.New("commit is not on the app's branch")
	ErrNotCancellable    = errors.New("deployment is not cancellable")
	ErrUnavailable       = errors.New("deploy request could not be checked")
)

type App struct {
	Target          core.Target
	Repository      string
	Branch          string
	ImageRepository string
}

type Commits interface {
	OnBranch(ctx context.Context, repository, branch, sha string) (bool, error)
}

type Migrations interface {
	MigrationFlag(ctx context.Context, t core.Target, commit core.Commit) (flagged bool, reason string, err error)
}

type Options struct {
	Repository Repository
	Apps       []App
	Commits    Commits
	Migrations Migrations
	Logger     *slog.Logger
}

type Request struct {
	Target    core.Target
	CommitSHA string
	Urgent    bool
	Reason    string
	Actor     core.Actor
	Role      core.Role
}

type Result struct {
	Deployment core.Deployment
	Created    bool
}

type CancelRequest struct {
	ID     core.DeploymentID
	Reason string
	Actor  core.Actor
	Role   core.Role
}

type Service struct {
	repo       Repository
	apps       map[core.Target]App
	commits    Commits
	migrations Migrations
	logger     *slog.Logger
}

func New(opts Options) (*Service, error) {
	var errs []error
	if opts.Repository == nil {
		errs = append(errs, errors.New("deploys: repository is required"))
	}
	if opts.Commits == nil {
		errs = append(errs, errors.New("deploys: commits are required"))
	}
	if opts.Migrations == nil {
		errs = append(errs, errors.New("deploys: migrations are required"))
	}
	apps := make(map[core.Target]App, len(opts.Apps))
	for i, app := range opts.Apps {
		if err := app.validate(); err != nil {
			errs = append(errs, fmt.Errorf("deploys: apps[%d] (%s): %w", i, app.Target, err))
			continue
		}
		if _, dup := apps[app.Target]; dup {
			errs = append(errs, fmt.Errorf("deploys: apps[%d]: duplicate target %s", i, app.Target))
			continue
		}
		apps[app.Target] = app
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Service{repo: opts.Repository, apps: apps, commits: opts.Commits, migrations: opts.Migrations, logger: logger}, nil
}

func (a App) validate() error {
	errs := []error{a.Target.Validate()}
	if strings.TrimSpace(a.Repository) == "" {
		errs = append(errs, errors.New("repository must not be empty"))
	}
	if strings.TrimSpace(a.Branch) == "" {
		errs = append(errs, errors.New("branch must not be empty"))
	}
	if strings.TrimSpace(a.ImageRepository) == "" {
		errs = append(errs, errors.New("image repository must not be empty"))
	}
	return errors.Join(errs...)
}

func (s *Service) Deploy(ctx context.Context, req Request) (Result, error) {
	reason, err := cleanReason(req.Reason)
	if err != nil {
		return Result{}, err
	}
	req.Reason = reason
	req.CommitSHA = strings.ToLower(strings.TrimSpace(req.CommitSHA))
	if err := validateCaller(req.Actor, req.Role, core.ValidateCommitSHA(req.CommitSHA)); err != nil {
		return Result{}, err
	}
	app, ok := s.apps[req.Target]
	if !ok {
		return Result{}, fmt.Errorf("%w %s", ErrUnknownApp, req.Target)
	}
	commit := core.Commit{Repository: app.Repository, SHA: req.CommitSHA}
	onBranch, err := s.commits.OnBranch(ctx, app.Repository, app.Branch, req.CommitSHA)
	switch {
	case errors.Is(err, ErrCommitNotFound):
		return Result{}, err
	case err != nil:
		return Result{}, fmt.Errorf("%w: check that %s is on %s of %s: %w", ErrUnavailable, req.CommitSHA, app.Branch, app.Repository, err)
	case !onBranch:
		return Result{}, fmt.Errorf("%w: %s is not reachable from %s of %s", ErrCommitNotOnBranch, req.CommitSHA, app.Branch, app.Repository)
	}
	migration, migrationReason, err := s.migrations.MigrationFlag(ctx, req.Target, commit)
	if err != nil {
		return Result{}, fmt.Errorf("%w: detect migrations of %s: %w", ErrUnavailable, req.CommitSHA, err)
	}
	if !migration {
		migrationReason = ""
	}

	var result Result
	err = s.repo.InTx(ctx, func(tx Repository) error {
		if err := tx.LockTarget(ctx, req.Target); err != nil {
			return err
		}
		block, blocked, err := tx.Blocked(ctx, req.Target, commit)
		if err != nil {
			return err
		}
		if blocked {
			return &core.BlockedError{Block: block}
		}
		d, created, err := tx.EnqueueDeployment(ctx, core.Deployment{
			Target:          req.Target,
			Kind:            core.KindDeploy,
			Source:          core.SourceAPI,
			RequestedBy:     req.Actor.String(),
			Commit:          commit,
			Image:           core.ImageRef{Repository: app.ImageRepository, Tag: req.CommitSHA},
			Migration:       migration,
			MigrationReason: core.CleanMigrationReason(migrationReason),
		}, priority(req.Urgent))
		if err != nil {
			return err
		}
		result = Result{Deployment: d, Created: created}
		detail := map[string]any{"commit": req.CommitSHA, "urgent": req.Urgent, "created": created}
		if req.Reason != "" {
			detail["reason"] = req.Reason
		}
		if d.Migration {
			detail["migration"] = true
		}
		entry, err := auditEntry(core.AuditDeployRequest, d, req.Actor, req.Role, detail)
		if err != nil {
			return err
		}
		return tx.RecordAudit(ctx, entry)
	})
	if err != nil {
		return Result{}, fmt.Errorf("deploy %s to %s: %w", req.CommitSHA, req.Target, err)
	}
	s.logger.InfoContext(ctx, "deploy requested",
		slog.String("app", req.Target.String()),
		slog.Int64("deployment_id", int64(result.Deployment.ID)),
		slog.String("commit", req.CommitSHA),
		slog.Bool("urgent", req.Urgent),
		slog.Bool("created", result.Created),
		slog.String("actor", req.Actor.String()),
	)
	return result, nil
}

func (s *Service) Cancel(ctx context.Context, req CancelRequest) (core.Deployment, error) {
	reason, err := cleanReason(req.Reason)
	if err != nil {
		return core.Deployment{}, err
	}
	req.Reason = reason
	var idErr error
	if req.ID <= 0 {
		idErr = fmt.Errorf("deployment id %d must be positive", req.ID)
	}
	if err := validateCaller(req.Actor, req.Role, idErr); err != nil {
		return core.Deployment{}, err
	}
	var cancelled, from core.Deployment
	for range MaxAttempts {
		cancelled, from, err = s.cancelOnce(ctx, req)
		if !errors.Is(err, core.ErrStateConflict) {
			break
		}
	}
	if err != nil {
		return core.Deployment{}, fmt.Errorf("cancel deployment %d: %w", req.ID, err)
	}
	s.logger.InfoContext(ctx, "deployment cancelled",
		slog.String("app", cancelled.Target.String()),
		slog.Int64("deployment_id", int64(cancelled.ID)),
		slog.String("from_state", string(from.State)),
		slog.String("actor", req.Actor.String()),
	)
	return cancelled, nil
}

func (s *Service) cancelOnce(ctx context.Context, req CancelRequest) (core.Deployment, core.Deployment, error) {
	var cancelled, current core.Deployment
	err := s.repo.InTx(ctx, func(tx Repository) error {
		var err error
		current, err = tx.GetDeployment(ctx, req.ID)
		if err != nil {
			return err
		}
		if current.State.Terminal() {
			return fmt.Errorf("%w: deployment %d is already %s", ErrNotCancellable, current.ID, current.State)
		}
		if err := tx.LockTarget(ctx, current.Target); err != nil {
			return err
		}
		message := "cancelled by " + req.Actor.String()
		if req.Reason != "" {
			message += ": " + req.Reason
		}
		cancelled, err = tx.TransitionDeployment(ctx, store.Transition{
			ID:      current.ID,
			From:    current.State,
			To:      core.StateCancelled,
			Actor:   req.Actor.String(),
			Message: message,
		})
		if err != nil {
			return err
		}
		detail := map[string]any{"from_state": current.State}
		if req.Reason != "" {
			detail["reason"] = req.Reason
		}
		if cancelled.PinCommitSHA != "" {
			detail["pin_commit_sha"] = cancelled.PinCommitSHA
		}
		entry, err := auditEntry(core.AuditDeployCancel, cancelled, req.Actor, req.Role, detail)
		if err != nil {
			return err
		}
		return tx.RecordAudit(ctx, entry)
	})
	return cancelled, current, err
}

func validateCaller(actor core.Actor, role core.Role, more ...error) error {
	errs := append([]error{actor.Validate()}, more...)
	if role != "" && !role.Valid() {
		errs = append(errs, fmt.Errorf("%w %q", core.ErrUnknownRole, role))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return nil
}

func cleanReason(reason string) (string, error) {
	if !utf8.ValidString(reason) {
		return "", fmt.Errorf("%w: reason must be valid UTF-8", ErrInvalidRequest)
	}
	cleaned := strings.Join(strings.FieldsFunc(reason, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
	if n := utf8.RuneCountInString(cleaned); n > MaxReasonLength {
		return "", fmt.Errorf("%w: reason must be at most %d characters, got %d", ErrInvalidRequest, MaxReasonLength, n)
	}
	return cleaned, nil
}

func priority(urgent bool) core.Priority {
	if urgent {
		return core.PriorityUrgent
	}
	return core.PriorityNormal
}

func auditEntry(action core.AuditAction, d core.Deployment, actor core.Actor, role core.Role, detail map[string]any) (core.AuditEntry, error) {
	data, err := json.Marshal(detail)
	if err != nil {
		return core.AuditEntry{}, fmt.Errorf("encode %s audit detail: %w", action, err)
	}
	target := d.Target
	id := d.ID
	return core.AuditEntry{Actor: actor, Role: role, Action: action, Target: &target, DeploymentID: &id, Detail: data}, nil
}
