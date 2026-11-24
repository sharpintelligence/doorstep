package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const maxFailureReason = 500

type FailureAction string

const FailureActionFail FailureAction = "fail"

type Failure struct {
	Deployment core.Deployment
	Stage      core.State
	Class      core.FailureClass
	Reason     string
	Sync       *core.SyncFailure
	Gates      *core.GateOutcome
	KeepPin    bool
}

type FailurePolicy interface {
	Decide(ctx context.Context, f Failure) (FailureAction, error)
}

type FailPolicy struct{}

func (FailPolicy) Decide(context.Context, Failure) (FailureAction, error) {
	return FailureActionFail, nil
}

type failureData struct {
	Stage        core.State             `json:"stage"`
	FailureClass core.FailureClass      `json:"failure_class"`
	SyncReason   core.SyncFailureReason `json:"sync_reason,omitempty"`
}

func (e *Engine) fail(ctx context.Context, d core.Deployment, f Failure) (core.Deployment, Result, error) {
	f.Deployment = d
	f.Stage = d.State
	f.Reason = cleanReason(f.Reason)
	logger := e.logFor(d)
	action, err := e.policy.Decide(ctx, f)
	switch {
	case err != nil:
		logger.ErrorContext(ctx, "failure policy failed; failing the deployment", slog.String("error", err.Error()))
	case action != FailureActionFail:
		logger.ErrorContext(ctx, "failure policy returned an unknown action; failing the deployment", slog.String("action", string(action)))
	}

	data := failureData{Stage: f.Stage, FailureClass: f.Class}
	if f.Sync != nil {
		data.SyncReason = f.Sync.Reason
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return d, Result{}, fmt.Errorf("encode failure of deployment %d: %w", d.ID, err)
	}
	failed, err := e.store.Transition(ctx, Transition{
		ID:            d.ID,
		From:          d.State,
		To:            core.StateFailed,
		Actor:         Actor,
		Message:       "failed while " + string(f.Stage),
		Data:          encoded,
		FailureReason: f.Reason,
	})
	if err != nil {
		return d, Result{}, err
	}
	logger.WarnContext(ctx, "deployment failed",
		slog.String("stage", string(f.Stage)),
		slog.String("failure_class", string(f.Class)),
		slog.String("reason", f.Reason),
	)
	if f.Stage == core.StatePinning && !f.KeepPin {
		e.abandon(ctx, failed)
	}
	outcome := OutcomeFailed
	if failed.Kind == core.KindRollback {
		outcome = OutcomeRollbackFailed
	}
	e.metrics.DeploymentFinished(failed.Target.App, outcome, runDuration(failed))
	e.notify(ctx, core.NotificationDeployFailed, failed, "")
	return failed, Result{}, nil
}

func cleanReason(reason string) string {
	reason = strings.ToValidUTF8(reason, "�")
	reason = strings.Join(strings.FieldsFunc(reason, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
	if reason == "" {
		reason = "unknown failure"
	}
	if utf8.RuneCountInString(reason) <= maxFailureReason {
		return reason
	}
	runes := []rune(reason)
	return string(runes[:maxFailureReason-1]) + "…"
}
