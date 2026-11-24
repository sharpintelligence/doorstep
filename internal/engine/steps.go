package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const noPinCommitMessage = "pin already current; no pin commit to wait for"

func (e *Engine) admit(ctx context.Context, d core.Deployment) (core.Deployment, Result, error) {
	if _, ok := e.syncTimeout(d.Target); !ok {
		return e.fail(ctx, d, Failure{Class: core.FailureConfiguration, Reason: "app " + d.Target.App + " is not configured"})
	}
	outcome, err := e.gates.Evaluate(ctx, d, core.GatePhaseBefore)
	if err != nil {
		return d, Result{}, fmt.Errorf("evaluate before gates: %w", err)
	}
	switch outcome.Status {
	case core.GatePassed, core.GateSkipped:
	case core.GateFailed:
		return e.fail(ctx, d, Failure{Class: core.FailureUnknown, Reason: "before gates failed: " + gateSummary(outcome), Gates: &outcome})
	case core.GatePending:
		e.logFor(d).DebugContext(ctx, "before gates pending", slog.Duration("retry_in", e.beforePoll))
		return d, Result{RequeueAfter: e.beforePoll}, nil
	default:
		return d, Result{}, fmt.Errorf("before gates returned unknown status %q", outcome.Status)
	}
	started, err := e.transition(ctx, d, core.StateResolving, "before gates passed", core.Lineage{})
	if err != nil {
		return d, Result{}, err
	}
	e.logFor(started).InfoContext(ctx, "deployment started")
	e.metrics.DeploymentStarted(started.Target.App)
	e.notify(ctx, core.NotificationDeployStarted, started, "")
	return started, Result{}, nil
}

func (e *Engine) resolve(ctx context.Context, d core.Deployment) (core.Deployment, Result, error) {
	digest := d.Digest
	if d.Kind == core.KindRollback {
		if digest == "" {
			return e.fail(ctx, d, Failure{Class: core.FailureConfiguration, Reason: "rollback has no digest to pin"})
		}
	} else {
		resolved, err := e.resolver.Resolve(ctx, d.Image)
		switch {
		case err == nil:
			digest = resolved
		case errors.Is(err, ErrPermanent):
			return e.fail(ctx, d, Failure{Class: core.FailureConfiguration, Reason: "resolve " + d.Image.String() + ": " + err.Error()})
		case errors.Is(err, ErrImageNotReady):
			return e.awaitImage(ctx, d, err)
		default:
			return e.retry(ctx, d, e.resolveTimeout, err, "resolve "+d.Image.String())
		}
		block, found, err := e.blocks.Blocked(ctx, d.Target, d.Commit, digest)
		if err != nil {
			return e.retry(ctx, d, e.resolveTimeout, err, "check blocks")
		}
		if found {
			return e.fail(ctx, d, Failure{Class: core.FailureVersion, Reason: (&core.BlockedError{Block: block}).Error()})
		}
	}
	pinning, err := e.transition(ctx, d, core.StatePinning, "resolved "+d.Image.String()+" to "+string(digest), core.Lineage{Digest: digest})
	if err != nil {
		return d, Result{}, err
	}
	return pinning, Result{}, nil
}

func (e *Engine) awaitImage(ctx context.Context, d core.Deployment, cause error) (core.Deployment, Result, error) {
	elapsed, err := e.elapsed(ctx, d)
	if err != nil {
		return d, Result{}, errors.Join(cause, err)
	}
	if elapsed >= e.resolveTimeout {
		return e.fail(ctx, d, Failure{
			Class:  core.FailureTransient,
			Reason: fmt.Sprintf("image %s was not available within %s: %v", d.Image, e.resolveTimeout, cause),
		})
	}
	e.logFor(d).InfoContext(ctx, "image not ready; waiting",
		slog.String("image", d.Image.String()),
		slog.String("reason", cause.Error()),
		slog.Duration("retry_in", e.resolveRetry),
	)
	return d, Result{RequeueAfter: e.resolveRetry}, nil
}

func (e *Engine) pin(ctx context.Context, d core.Deployment) (core.Deployment, Result, error) {
	if d.Digest == "" {
		return e.fail(ctx, d, Failure{Class: core.FailureConfiguration, Reason: "no digest to pin"})
	}
	res, err := e.writePin(ctx, d)
	if err != nil {
		if errors.Is(err, ErrPermanent) {
			return e.fail(ctx, d, Failure{Class: core.FailureConfiguration, Reason: "pin " + string(d.Digest) + ": " + err.Error()})
		}
		return e.retry(ctx, d, e.pinTimeout, err, "pin "+string(d.Digest))
	}
	message := "pinned in " + res.Commit.Repository + "@" + res.Commit.SHA
	if res.Commit.SHA == "" {
		message = noPinCommitMessage
	}
	syncing, err := e.transition(ctx, d, core.StateSyncing, message, core.Lineage{
		PreviousDigest:   res.Previous,
		GitOpsRepository: res.Commit.Repository,
		PinCommitSHA:     res.Commit.SHA,
	})
	if err != nil {
		return d, Result{}, err
	}
	return syncing, Result{}, nil
}

func (e *Engine) writePin(ctx context.Context, d core.Deployment) (PinResult, error) {
	trailers, err := e.blocks.PendingTrailers(ctx, d.Target)
	if err != nil {
		return PinResult{}, fmt.Errorf("read pending block trailers: %w", err)
	}
	res, err := e.pins.Pin(ctx, PinRequest{Deployment: d, Digest: d.Digest, Blocks: trailers.Blocks, Unblocks: trailers.Unblocks})
	if err != nil {
		return PinResult{}, err
	}
	if res.Changed && !trailers.IsZero() {
		if err := e.blocks.MarkMirrored(ctx, d.Target, trailers, res.Commit); err != nil {
			e.logFor(d).WarnContext(ctx, "mark block trailers mirrored", slog.String("commit", res.Commit.SHA), slog.String("error", err.Error()))
		}
	}
	return res, nil
}

func (e *Engine) syncStep(ctx context.Context, d core.Deployment) (core.Deployment, Result, error) {
	timeout, ok := e.syncTimeout(d.Target)
	if !ok {
		return e.fail(ctx, d, Failure{Class: core.FailureConfiguration, Reason: "app " + d.Target.App + " is not configured"})
	}
	elapsed, err := e.elapsed(ctx, d)
	if err != nil {
		return d, Result{}, err
	}
	if d.PinCommitSHA == "" {
		return e.awaitHealthy(ctx, d, elapsed, timeout)
	}
	waitCtx, cancel := context.WithTimeoutCause(ctx, max(timeout-elapsed, e.syncGrace), errSyncDeadline)
	status, err := e.sync.WaitSynced(waitCtx, d.Target, d.PinCommitSHA)
	ownDeadline := errors.Is(context.Cause(waitCtx), errSyncDeadline)
	cancel()
	switch {
	case err == nil:
		verifying, err := e.transition(ctx, d, core.StateVerifying, "synced and healthy", core.Lineage{ArgoCDRevision: status.Revision})
		if err != nil {
			return d, Result{}, err
		}
		return verifying, Result{}, nil
	case ctx.Err() != nil:
		return d, Result{}, ctx.Err()
	case ownDeadline || isSyncFailure(err):
		return e.failSync(ctx, d, status, err, timeout)
	default:
		return d, Result{}, fmt.Errorf("wait for sync: %w", err)
	}
}

func (e *Engine) awaitHealthy(ctx context.Context, d core.Deployment, elapsed, timeout time.Duration) (core.Deployment, Result, error) {
	health, err := e.sync.Health(ctx, d.Target)
	switch {
	case ctx.Err() != nil:
		return d, Result{}, ctx.Err()
	case err == nil && health.Status == core.HealthHealthy:
		verifying, err := e.transition(ctx, d, core.StateVerifying, noPinCommitMessage, core.Lineage{})
		if err != nil {
			return d, Result{}, err
		}
		return verifying, Result{}, nil
	case errors.Is(err, core.ErrSyncTargetNotFound), errors.Is(err, core.ErrSyncForbidden):
		return e.failSync(ctx, d, core.SyncStatus{}, err, timeout)
	}
	if elapsed >= timeout {
		failure := healthFailure(health, err)
		return e.failSync(ctx, d, core.SyncStatus{Health: health, Failure: &failure}, core.ErrSyncTimeout, timeout)
	}
	attrs := []any{slog.String("health", string(health.Status)), slog.Duration("retry_in", e.verifyPoll)}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	e.logFor(d).DebugContext(ctx, "waiting for a healthy application", attrs...)
	return d, Result{RequeueAfter: e.verifyPoll}, nil
}

func healthFailure(health core.Health, err error) core.SyncFailure {
	if err != nil {
		return core.SyncFailure{Reason: core.SyncReasonUnavailable, Class: core.SyncReasonUnavailable.Class(true), Message: err.Error()}
	}
	reason := core.SyncReasonProgressing
	switch health.Status {
	case core.HealthDegraded:
		reason = core.SyncReasonDegraded
	case core.HealthSuspended:
		reason = core.SyncReasonSuspended
	case core.HealthMissing:
		reason = core.SyncReasonMissing
	case core.HealthHealthy, core.HealthProgressing, core.HealthUnknown:
	}
	message := health.Message
	if message == "" {
		message = "application is " + string(health.Status)
	}
	return core.SyncFailure{Reason: reason, Class: reason.Class(true), Message: message}
}

func isSyncFailure(err error) bool {
	for _, target := range []error{core.ErrSyncFailed, core.ErrSyncTimeout, core.ErrSyncTargetNotFound, core.ErrSyncForbidden} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func (e *Engine) failSync(ctx context.Context, d core.Deployment, status core.SyncStatus, err error, timeout time.Duration) (core.Deployment, Result, error) {
	failure := core.SyncFailure{Reason: core.SyncReasonProgressing, Class: core.FailureUnknown, Message: "application did not settle"}
	if status.Failure != nil {
		failure = *status.Failure
	}
	var summary string
	switch {
	case errors.Is(err, core.ErrSyncFailed):
		summary = "Argo CD sync failed"
	case errors.Is(err, core.ErrSyncTargetNotFound):
		summary = "Argo CD Application not found"
		failure = configurationFailure(failure, core.SyncReasonNotFound, err)
	case errors.Is(err, core.ErrSyncForbidden):
		summary = "reading the Argo CD Application is forbidden"
		failure = configurationFailure(failure, core.SyncReasonForbidden, err)
	default:
		summary = fmt.Sprintf("Argo CD did not sync and become healthy within %s", timeout)
	}
	return e.fail(ctx, d, Failure{
		Class:  failure.Class,
		Reason: fmt.Sprintf("%s (%s): %s", summary, failure.Reason, failure.Message),
		Sync:   &failure,
	})
}

func configurationFailure(f core.SyncFailure, reason core.SyncFailureReason, err error) core.SyncFailure {
	if f.Reason == reason {
		return f
	}
	return core.SyncFailure{Reason: reason, Class: reason.Class(true), Message: err.Error()}
}

func (e *Engine) verify(ctx context.Context, d core.Deployment) (core.Deployment, Result, error) {
	outcome, err := e.gates.Evaluate(ctx, d, core.GatePhaseAfter)
	if err != nil {
		return d, Result{}, fmt.Errorf("evaluate after gates: %w", err)
	}
	switch outcome.Status {
	case core.GatePassed, core.GateSkipped:
	case core.GateFailed:
		return e.fail(ctx, d, Failure{Class: core.FailureVersion, Reason: "after gates failed: " + gateSummary(outcome), Gates: &outcome})
	case core.GatePending:
		e.logFor(d).DebugContext(ctx, "after gates pending", slog.Duration("retry_in", e.verifyPoll))
		return d, Result{RequeueAfter: e.verifyPoll}, nil
	default:
		return d, Result{}, fmt.Errorf("after gates returned unknown status %q", outcome.Status)
	}
	if d.Kind == core.KindRollback {
		current, err := e.pins.Current(ctx, d.Target)
		switch {
		case errors.Is(err, ErrPermanent):
			return e.fail(ctx, d, Failure{Class: core.FailureConfiguration, Reason: "read the current pin: " + err.Error()})
		case err != nil:
			return d, Result{}, fmt.Errorf("read the current pin: %w", err)
		case current != d.Digest:
			return e.repin(ctx, d, current)
		}
	}
	return e.succeed(ctx, d)
}

func (e *Engine) repin(ctx context.Context, d core.Deployment, current core.Digest) (core.Deployment, Result, error) {
	e.logFor(d).WarnContext(ctx, "rollback pin was replaced; pinning again",
		slog.String("current", string(current)),
		slog.String("digest", string(d.Digest)),
	)
	res, err := e.writePin(ctx, d)
	switch {
	case errors.Is(err, ErrPermanent):
		return e.fail(ctx, d, Failure{Class: core.FailureConfiguration, Reason: "pin " + string(d.Digest) + " again: " + err.Error()})
	case err != nil:
		return d, Result{}, fmt.Errorf("pin %s again: %w", d.Digest, err)
	case res.Commit.SHA == "":
		return d, Result{RequeueAfter: e.verifyPoll}, nil
	}
	d, err = e.store.SetLineage(ctx, d.ID, core.Lineage{PinCommitSHA: res.Commit.SHA, GitOpsRepository: res.Commit.Repository}, Actor)
	if err != nil {
		return d, Result{}, err
	}
	status, err := e.sync.WaitSynced(ctx, d.Target, res.Commit.SHA)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return d, Result{}, ctx.Err()
	case isSyncFailure(err):
		timeout, _ := e.syncTimeout(d.Target)
		return e.failSync(ctx, d, status, err, timeout)
	default:
		return d, Result{}, fmt.Errorf("wait for sync of the new pin: %w", err)
	}
	if status.Revision != "" {
		if d, err = e.store.SetLineage(ctx, d.ID, core.Lineage{ArgoCDRevision: status.Revision}, Actor); err != nil {
			return d, Result{}, err
		}
	}
	return d, Result{RequeueAfter: e.verifyPoll}, nil
}

func (e *Engine) succeed(ctx context.Context, d core.Deployment) (core.Deployment, Result, error) {
	succeeded, err := e.transition(ctx, d, core.StateSucceeded, "after gates passed", core.Lineage{})
	if err != nil {
		return d, Result{}, err
	}
	e.logFor(succeeded).InfoContext(ctx, "deployment succeeded", slog.String("digest", string(succeeded.Digest)))
	if succeeded.Kind == core.KindRollback {
		e.metrics.DeploymentFinished(succeeded.Target.App, OutcomeRolledBack, runDuration(succeeded))
		e.notify(ctx, core.NotificationDeployRolledBack, succeeded, "")
	} else {
		e.metrics.DeploymentFinished(succeeded.Target.App, OutcomeSucceeded, runDuration(succeeded))
		e.notify(ctx, core.NotificationDeploySucceeded, succeeded, "")
	}
	return succeeded, Result{}, nil
}

func (e *Engine) retry(ctx context.Context, d core.Deployment, limit time.Duration, cause error, action string) (core.Deployment, Result, error) {
	if ctx.Err() != nil || errors.Is(cause, core.ErrStateConflict) {
		return d, Result{}, cause
	}
	elapsed, err := e.elapsed(ctx, d)
	if err != nil {
		return d, Result{}, errors.Join(cause, err)
	}
	if elapsed < limit {
		return d, Result{}, fmt.Errorf("%s: %w", action, cause)
	}
	return e.fail(ctx, d, Failure{
		Class:  core.FailureTransient,
		Reason: fmt.Sprintf("%s: gave up after %s: %v", action, limit, cause),
	})
}

func gateSummary(outcome core.GateOutcome) string {
	var parts []string
	for _, r := range outcome.Results {
		if r.Status != core.GateFailed {
			continue
		}
		part := r.Name
		if r.Message != "" {
			part += ": " + r.Message
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "no gate reported why"
	}
	return strings.Join(parts, "; ")
}
