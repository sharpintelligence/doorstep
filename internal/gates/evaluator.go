package gates

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

const (
	DefaultEvaluationTimeout = 30 * time.Second

	skippedForRollback = "skipped for rollback"
	evaluationErrorTag = "evaluation error: "
)

var (
	ErrNoPlan       = errors.New("no gate plan for app")
	ErrInvalidPhase = errors.New("invalid gate phase")
)

type ResultStore interface {
	RecordGateResult(ctx context.Context, r core.GateResult) (core.GateResult, bool, error)
	ListGateResults(ctx context.Context, id core.DeploymentID) ([]core.GateResult, error)
}

type Metrics interface {
	GateEvaluated(app string, gate metrics.GateKind, phase metrics.Phase, result metrics.GateResult, duration time.Duration)
}

type Options struct {
	Store             ResultStore
	Plans             map[string]Plan
	Metrics           Metrics
	Observers         []Observer
	Logger            *slog.Logger
	Now               func() time.Time
	EvaluationTimeout time.Duration
}

type Evaluator struct {
	store             ResultStore
	plans             map[string]Plan
	metrics           Metrics
	observers         []Observer
	logger            *slog.Logger
	now               func() time.Time
	evaluationTimeout time.Duration

	mu        sync.Mutex
	abandoned map[callKey]int
}

type callKey struct {
	deployment core.DeploymentID
	gate       string
}

type callState struct {
	returned  bool
	abandoned bool
}

func NewEvaluator(opts Options) (*Evaluator, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("gate evaluator: a result store is required")
	case opts.Logger == nil:
		return nil, errors.New("gate evaluator: a logger is required")
	case opts.EvaluationTimeout < 0:
		return nil, fmt.Errorf("gate evaluator: evaluation timeout %s is negative", opts.EvaluationTimeout)
	}
	e := &Evaluator{
		store:             opts.Store,
		plans:             make(map[string]Plan, len(opts.Plans)),
		metrics:           opts.Metrics,
		observers:         append([]Observer(nil), opts.Observers...),
		logger:            opts.Logger,
		now:               opts.Now,
		evaluationTimeout: opts.EvaluationTimeout,
		abandoned:         map[callKey]int{},
	}
	for app, plan := range opts.Plans {
		e.plans[app] = plan
	}
	if e.metrics == nil {
		e.metrics = discardMetrics{}
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.evaluationTimeout == 0 {
		e.evaluationTimeout = DefaultEvaluationTimeout
	}
	return e, nil
}

func (e *Evaluator) Evaluate(ctx context.Context, d core.Deployment, phase core.GatePhase) (core.GateOutcome, error) {
	return e.evaluate(ctx, d, phase, false)
}

func (e *Evaluator) Reevaluate(ctx context.Context, d core.Deployment, phase core.GatePhase) (core.GateOutcome, error) {
	return e.evaluate(ctx, d, phase, true)
}

func (e *Evaluator) evaluate(ctx context.Context, d core.Deployment, phase core.GatePhase, rerun bool) (core.GateOutcome, error) {
	if !phase.Valid() {
		return core.GateOutcome{}, fmt.Errorf("%w %q", ErrInvalidPhase, phase)
	}
	plan, ok := e.plans[d.Target.App]
	if !ok {
		return core.GateOutcome{}, fmt.Errorf("%w %s", ErrNoPlan, d.Target.App)
	}
	bindings := plan.Phase(phase)
	results := make([]core.GateResult, 0, len(bindings))
	if len(bindings) > 0 {
		recorded, err := e.store.ListGateResults(ctx, d.ID)
		if err != nil {
			return core.GateOutcome{}, fmt.Errorf("evaluate %s gates of deployment %d: %w", phase, d.ID, err)
		}
		previous := make(map[string]core.GateResult, len(recorded))
		for _, r := range recorded {
			previous[r.Name] = r
		}
		for _, b := range bindings {
			if err := ctx.Err(); err != nil {
				return core.GateOutcome{}, err
			}
			prev, found := previous[b.Name]
			if found && prev.Status.Terminal() && !rerun {
				results = append(results, prev)
				continue
			}
			var prevPtr *core.GateResult
			if found {
				prevPtr = &prev
			}
			result, err := e.evaluateGate(ctx, d, b, prevPtr)
			if err != nil {
				return core.GateOutcome{}, err
			}
			results = append(results, result)
		}
	}
	return core.GateOutcome{Phase: phase, Status: core.AggregateGateStatus(results), Results: results}, nil
}

func (e *Evaluator) evaluateGate(ctx context.Context, d core.Deployment, b Binding, previous *core.GateResult) (core.GateResult, error) {
	candidate := core.GateResult{DeploymentID: d.ID, Name: b.Name, Type: string(b.Type), Phase: b.Phase}
	var (
		evalErr      error
		callDuration time.Duration
	)
	if d.Kind == core.KindRollback && b.SkipOnRollback {
		candidate.Status = core.GateSkipped
		candidate.Message = skippedForRollback
	} else {
		var ev Evaluation
		ev, callDuration, evalErr = e.call(ctx, b, Request{Deployment: d, Phase: b.Phase, Previous: previous})
		if err := ctx.Err(); err != nil {
			return core.GateResult{}, err
		}
		if evalErr == nil {
			evalErr = validateEvaluation(ev)
		}
		if evalErr != nil {
			candidate.Status = core.GatePending
			candidate.Message = evaluationErrorTag + evalErr.Error()
			if previous != nil {
				candidate.Evidence = previous.Evidence
			}
		} else {
			candidate.Status = ev.Status
			candidate.Message = ev.Message
			candidate.Evidence = ev.Evidence
		}
	}
	candidate.Message = core.CleanGateMessage(candidate.Message)

	stored, err := e.record(ctx, d, b, candidate, callDuration)
	if err != nil {
		return core.GateResult{}, err
	}
	if evalErr != nil {
		e.logger.WarnContext(ctx, "gate evaluation error",
			slog.Int64("deployment", int64(d.ID)), slog.String("app", d.Target.App),
			slog.String("gate", b.Name), slog.Any("error", evalErr))
		e.metrics.GateEvaluated(d.Target.App, b.Kind, metricsPhase(d), metrics.GateResultError, callDuration)
	}
	if stored.Status == core.GatePending && b.Timeout > 0 && stored.UpdatedAt.Sub(stored.StartedAt) >= b.Timeout {
		timedOut := candidate
		timedOut.Status = core.GateFailed
		timedOut.Message = fmt.Sprintf("timed out after %s", b.Timeout)
		timedOut.Evidence = stored.Evidence
		return e.record(ctx, d, b, timedOut, callDuration)
	}
	return stored, nil
}

func (e *Evaluator) record(ctx context.Context, d core.Deployment, b Binding, candidate core.GateResult, callDuration time.Duration) (core.GateResult, error) {
	stored, changed, err := e.store.RecordGateResult(ctx, candidate)
	if err != nil {
		return core.GateResult{}, fmt.Errorf("record gate %s of deployment %d: %w", b.Name, d.ID, err)
	}
	if !changed {
		return stored, nil
	}
	if outcome, ok := metricsResult(stored.Status); ok && stored.FinishedAt != nil {
		e.metrics.GateEvaluated(d.Target.App, b.Kind, metricsPhase(d), outcome, stored.FinishedAt.Sub(stored.StartedAt)+callDuration)
	}
	for _, o := range e.observers {
		if err := o.GateRecorded(ctx, d, stored); err != nil {
			e.logger.WarnContext(ctx, "gate observer failed",
				slog.Int64("deployment", int64(d.ID)), slog.String("app", d.Target.App),
				slog.String("gate", b.Name), slog.String("status", string(stored.Status)),
				slog.Any("error", err))
		}
	}
	return stored, nil
}

type callResult struct {
	evaluation Evaluation
	err        error
}

func (e *Evaluator) call(ctx context.Context, b Binding, req Request) (Evaluation, time.Duration, error) {
	key := callKey{deployment: req.Deployment.ID, gate: b.Name}
	e.mu.Lock()
	stuck := e.abandoned[key] > 0
	e.mu.Unlock()
	if stuck {
		return Evaluation{}, 0, errors.New("previous call has not returned yet")
	}

	callCtx, cancel := context.WithTimeout(ctx, e.evaluationTimeout)
	defer cancel()

	started := e.now()
	state := &callState{}
	done := make(chan callResult, 1)
	go func() {
		var res callResult
		defer func() {
			if r := recover(); r != nil {
				res = callResult{err: fmt.Errorf("gate panicked: %v", r)}
			}
			done <- res
			e.callReturned(key, state)
		}()
		ev, err := b.Gate.Evaluate(callCtx, req)
		res = callResult{evaluation: ev, err: err}
	}()

	var res callResult
	select {
	case res = <-done:
	case <-callCtx.Done():
		res = callResult{err: callCtx.Err()}
		e.abandonCall(ctx, key, state)
	}
	elapsed := e.now().Sub(started)
	if errors.Is(res.err, context.DeadlineExceeded) && ctx.Err() == nil {
		res.err = fmt.Errorf("no verdict within %s", e.evaluationTimeout)
	}
	return res.evaluation, elapsed, res.err
}

func (e *Evaluator) abandonCall(ctx context.Context, key callKey, state *callState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if state.returned {
		return
	}
	state.abandoned = true
	e.abandoned[key]++
	e.logger.WarnContext(ctx, "gate call abandoned while still running",
		slog.Int64("deployment", int64(key.deployment)), slog.String("gate", key.gate))
}

func (e *Evaluator) callReturned(key callKey, state *callState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	state.returned = true
	if !state.abandoned {
		return
	}
	if e.abandoned[key]--; e.abandoned[key] <= 0 {
		delete(e.abandoned, key)
	}
}

func validateEvaluation(ev Evaluation) error {
	if !ev.Status.Valid() {
		return fmt.Errorf("gate returned unknown status %q", ev.Status)
	}
	return core.ValidateGateEvidence(ev.Evidence)
}

func metricsPhase(d core.Deployment) metrics.Phase {
	if d.Kind == core.KindRollback {
		return metrics.PhaseRollback
	}
	return metrics.PhaseDeploy
}

func metricsResult(status core.GateStatus) (metrics.GateResult, bool) {
	switch status {
	case core.GatePassed:
		return metrics.GateResultPassed, true
	case core.GateFailed:
		return metrics.GateResultFailed, true
	case core.GatePending, core.GateSkipped:
		return "", false
	default:
		return "", false
	}
}

type discardMetrics struct{}

func (discardMetrics) GateEvaluated(string, metrics.GateKind, metrics.Phase, metrics.GateResult, time.Duration) {
}
