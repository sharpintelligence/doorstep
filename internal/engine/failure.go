package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const maxFailureReason = 500

type FailureAction string

const (
	FailureActionFail  FailureAction = "fail"
	FailureActionRetry FailureAction = "retry"
)

type DecisionReason string

const (
	DecisionTransient          DecisionReason = "transient"
	DecisionVersionCaused      DecisionReason = "version_caused"
	DecisionRetriesExhausted   DecisionReason = "retries_exhausted"
	DecisionNotRetryable       DecisionReason = "not_retryable"
	DecisionRetriesDisabled    DecisionReason = "retries_disabled"
	DecisionNewerReleaseQueued DecisionReason = "newer_release_queued"
)

type Decision struct {
	Action      FailureAction
	Reason      DecisionReason
	Class       core.FailureClass
	Summary     string
	Delay       time.Duration
	MaxAttempts int
	Diagnosis   json.RawMessage
}

type Failure struct {
	Deployment core.Deployment
	Stage      core.State
	Class      core.FailureClass
	Reason     string
	Sync       *core.SyncFailure
	Gates      *core.GateOutcome
	KeepPin    bool
	Attempt    int
}

func (f Failure) Retryable() bool {
	return f.Stage == core.StateSyncing && f.Sync != nil
}

type FailurePolicy interface {
	Decide(ctx context.Context, f Failure) (Decision, error)
}

type FailPolicy struct{}

func (FailPolicy) Decide(_ context.Context, f Failure) (Decision, error) {
	return Decision{Action: FailureActionFail, Reason: DecisionNotRetryable, Class: f.Class}, nil
}

type failureData struct {
	Stage            core.State             `json:"stage"`
	FailureClass     core.FailureClass      `json:"failure_class"`
	SyncReason       core.SyncFailureReason `json:"sync_reason,omitempty"`
	Attempts         int                    `json:"attempts"`
	Decision         DecisionReason         `json:"decision"`
	RollbackEligible bool                   `json:"rollback_eligible"`
	Diagnosis        json.RawMessage        `json:"diagnosis,omitempty"`
	AutoRollback     *autoRollbackData      `json:"auto_rollback,omitempty"`
}

type retryData struct {
	Stage        core.State             `json:"stage"`
	FailureClass core.FailureClass      `json:"failure_class"`
	SyncReason   core.SyncFailureReason `json:"sync_reason,omitempty"`
	Retry        retryInfo              `json:"retry"`
	Diagnosis    json.RawMessage        `json:"diagnosis,omitempty"`
}

type retryInfo struct {
	Attempt     int            `json:"attempt"`
	NextAttempt int            `json:"next_attempt"`
	MaxAttempts int            `json:"max_attempts"`
	DelayMS     int64          `json:"delay_ms"`
	Reason      DecisionReason `json:"reason"`
	Summary     string         `json:"summary,omitempty"`
}

func (e *Engine) fail(ctx context.Context, d core.Deployment, f Failure) (core.Deployment, Result, error) {
	f.Deployment = d
	f.Stage = d.State
	f.Reason = cleanReason(f.Reason)
	state, err := e.store.RetryState(ctx, d.ID)
	if err != nil {
		return d, Result{}, fmt.Errorf("read the retry state of deployment %d: %w", d.ID, err)
	}
	f.Attempt = state.Retries + 1
	decision := e.decide(ctx, f)
	if decision.Action == FailureActionRetry {
		return e.retryAttempt(ctx, d, f, decision)
	}
	return e.failFinal(ctx, d, f, decision)
}

func (e *Engine) decide(ctx context.Context, f Failure) Decision {
	logger := e.logFor(f.Deployment)
	fallback := Decision{Action: FailureActionFail, Reason: DecisionNotRetryable, Class: f.Class}
	decision, err := e.policy.Decide(ctx, f)
	switch {
	case err != nil:
		logger.ErrorContext(ctx, "failure policy failed; failing the deployment", slog.String("error", err.Error()))
		return fallback
	case decision.Action == FailureActionRetry && !f.Retryable():
		logger.ErrorContext(ctx, "failure policy asked to retry a failure that cannot be retried; failing the deployment", slog.String("stage", string(f.Stage)))
		return fallback
	case decision.Action != FailureActionFail && decision.Action != FailureActionRetry:
		logger.ErrorContext(ctx, "failure policy returned an unknown action; failing the deployment", slog.String("action", string(decision.Action)))
		return fallback
	}
	if decision.Class == "" {
		decision.Class = f.Class
	}
	if decision.Reason == "" {
		decision.Reason = DecisionNotRetryable
	}
	decision.Summary = cleanReason(decision.Summary)
	if decision.Summary == unknownFailure {
		decision.Summary = ""
	}
	if !jsonObject(decision.Diagnosis) {
		if len(decision.Diagnosis) > 0 {
			logger.WarnContext(ctx, "failure policy returned a diagnosis that is not a JSON object; dropping it")
		}
		decision.Diagnosis = nil
	}
	return decision
}

func (e *Engine) retryAttempt(ctx context.Context, d core.Deployment, f Failure, decision Decision) (core.Deployment, Result, error) {
	next := f.Attempt + 1
	maxAttempts := max(decision.MaxAttempts, next)
	delay := max(decision.Delay, 0)
	data := retryData{
		Stage:        f.Stage,
		FailureClass: decision.Class,
		SyncReason:   f.Sync.Reason,
		Retry: retryInfo{
			Attempt:     f.Attempt,
			NextAttempt: next,
			MaxAttempts: maxAttempts,
			DelayMS:     delay.Milliseconds(),
			Reason:      decision.Reason,
			Summary:     decision.Summary,
		},
		Diagnosis: decision.Diagnosis,
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return d, Result{}, fmt.Errorf("encode retry of deployment %d: %w", d.ID, err)
	}
	plan := fmt.Sprintf("retrying (attempt %d of %d) %s", next, maxAttempts, after(delay))
	retries := f.Attempt - 1
	retried, err := e.store.Transition(ctx, Transition{
		ID:            d.ID,
		From:          core.StateSyncing,
		To:            core.StateSyncing,
		Actor:         Actor,
		Message:       fmt.Sprintf("attempt %d failed; %s", f.Attempt, plan),
		Data:          encoded,
		ExpectRetries: &retries,
	})
	if err != nil {
		return d, Result{}, err
	}
	e.logFor(retried).WarnContext(ctx, "deployment attempt failed; retrying",
		slog.Int("attempt", f.Attempt),
		slog.Int("max_attempts", maxAttempts),
		slog.Duration("retry_in", delay),
		slog.String("failure_class", string(decision.Class)),
		slog.String("decision", string(decision.Reason)),
		slog.String("summary", decision.Summary),
		slog.String("reason", f.Reason),
	)
	e.metrics.DeploymentRetried(retried.Target.App, decision.Class)
	if f.Attempt == 1 {
		message := fmt.Sprintf("attempt 1 of %d failed", maxAttempts)
		if decision.Summary != "" {
			message += " (" + decision.Summary + ")"
		}
		e.notify(ctx, core.NotificationDeployRetrying, retried, message+"; "+plan)
	}
	if delay > 0 {
		return retried, Result{RequeueAfter: delay}, nil
	}
	return retried, Result{}, nil
}

func (e *Engine) failFinal(ctx context.Context, d core.Deployment, f Failure, decision Decision) (core.Deployment, Result, error) {
	if rollbackEligible(f, decision) && e.rollbacks != nil && e.rollbacks.Enabled(d.Target) {
		return e.failAndRollBack(ctx, d, f, decision)
	}
	t, err := failureTransition(f, decision, nil)
	if err != nil {
		return d, Result{}, err
	}
	failed, err := e.store.Transition(ctx, t)
	if err != nil {
		return d, Result{}, err
	}
	e.announceFailure(ctx, failed, f, decision, "")
	return failed, Result{}, nil
}

func failureTransition(f Failure, decision Decision, plan *RollbackPlan) (Transition, error) {
	d := f.Deployment
	data := failureData{
		Stage:            f.Stage,
		FailureClass:     decision.Class,
		Attempts:         f.Attempt,
		Decision:         decision.Reason,
		RollbackEligible: rollbackEligible(f, decision),
		Diagnosis:        decision.Diagnosis,
		AutoRollback:     autoRollbackFor(plan),
	}
	if f.Sync != nil {
		data.SyncReason = f.Sync.Reason
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return Transition{}, fmt.Errorf("encode failure of deployment %d: %w", d.ID, err)
	}
	message := "failed while " + string(f.Stage)
	if f.Attempt > 1 {
		message += " after " + strconv.Itoa(f.Attempt) + " attempts"
	}
	reason := f.Reason
	if plan != nil {
		message += "; " + rollbackMessage(*plan)
		if plan.Outcome == RollbackWithheldMigrations {
			reason = withheldReason(f.Reason)
		}
	}
	return Transition{
		ID:            d.ID,
		From:          d.State,
		To:            core.StateFailed,
		Actor:         Actor,
		Message:       message,
		Data:          encoded,
		FailureReason: reason,
	}, nil
}

func (e *Engine) announceFailure(ctx context.Context, failed core.Deployment, f Failure, decision Decision, rollback string) {
	e.logFor(f.Deployment).WarnContext(ctx, "deployment failed",
		slog.String("stage", string(f.Stage)),
		slog.String("failure_class", string(decision.Class)),
		slog.String("decision", string(decision.Reason)),
		slog.Int("attempts", f.Attempt),
		slog.Bool("rollback_eligible", rollbackEligible(f, decision)),
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
	e.notify(ctx, core.NotificationDeployFailed, failed, joinSentences(finalMessage(f, decision), rollback))
}

func rollbackEligible(f Failure, decision Decision) bool {
	d := f.Deployment
	if d.Kind != core.KindDeploy || (d.PinCommitSHA == "" && d.Digest == "") {
		return false
	}
	if f.Stage != core.StateSyncing && f.Stage != core.StateVerifying {
		return false
	}
	if decision.Reason == DecisionVersionCaused || decision.Reason == DecisionRetriesExhausted {
		return true
	}
	return f.Stage == core.StateVerifying && decision.Class == core.FailureVersion
}

func finalMessage(f Failure, decision Decision) string {
	summary := decision.Summary
	switch decision.Reason {
	case DecisionRetriesExhausted:
		return joinMessage(fmt.Sprintf("still failing after %d attempts", f.Attempt), summary)
	case DecisionVersionCaused:
		if summary == "" {
			return ""
		}
		return summary + "; not retried"
	case DecisionNewerReleaseQueued:
		return joinMessage("not retried because a newer deployment is waiting", summary)
	case DecisionRetriesDisabled:
		return joinMessage("not retried because retries are disabled", summary)
	case DecisionTransient, DecisionNotRetryable:
		return summary
	}
	return summary
}

func joinSentences(first, second string) string {
	switch {
	case first == "":
		return second
	case second == "":
		return first
	}
	return first + "; " + second
}

func joinMessage(lead, detail string) string {
	if detail == "" {
		return lead
	}
	return lead + ": " + detail
}

func after(delay time.Duration) string {
	if delay <= 0 {
		return "now"
	}
	return "in " + shortDuration(delay)
}

func shortDuration(d time.Duration) string {
	s := d.Round(time.Millisecond).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func jsonObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}

const unknownFailure = "unknown failure"

func cleanReason(reason string) string {
	reason = strings.ToValidUTF8(reason, "�")
	reason = strings.Join(strings.FieldsFunc(reason, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
	if reason == "" {
		reason = unknownFailure
	}
	return truncate(reason, maxFailureReason)
}

func truncate(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	return string(runes[:limit-1]) + "…"
}
