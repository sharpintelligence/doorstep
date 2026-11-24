package retry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/engine"
)

const DefaultLookupTimeout = 15 * time.Second

type Settings struct {
	Enabled     bool
	MaxAttempts int
	Backoff     time.Duration
	MaxBackoff  time.Duration
}

func (s Settings) Delay(attempt int) time.Duration {
	delay := max(s.Backoff, 0)
	for range max(attempt-1, 0) {
		if delay >= s.MaxBackoff {
			break
		}
		delay *= 2
	}
	return min(delay, max(s.MaxBackoff, 0))
}

type Diagnostics interface {
	Pods(ctx context.Context, d core.Deployment) ([]core.PodStatus, error)
}

type History interface {
	Diagnoses(ctx context.Context, id core.DeploymentID) (earlier, recent []json.RawMessage, err error)
}

type Queue interface {
	Waiting(ctx context.Context, t core.Target, except core.DeploymentID) (bool, error)
}

type Options struct {
	Settings      func(core.Target) (Settings, bool)
	Diagnostics   Diagnostics
	History       History
	Queue         Queue
	Logger        *slog.Logger
	LookupTimeout time.Duration
}

type Policy struct {
	settings    func(core.Target) (Settings, bool)
	diagnostics Diagnostics
	history     History
	queue       Queue
	logger      *slog.Logger
	timeout     time.Duration
}

var _ engine.FailurePolicy = (*Policy)(nil)

func New(opts Options) (*Policy, error) {
	if opts.Settings == nil {
		return nil, errors.New("retry: the settings of each app are required")
	}
	p := &Policy{
		settings:    opts.Settings,
		diagnostics: opts.Diagnostics,
		history:     opts.History,
		queue:       opts.Queue,
		logger:      opts.Logger,
		timeout:     opts.LookupTimeout,
	}
	if p.logger == nil {
		p.logger = slog.New(slog.DiscardHandler)
	}
	if p.timeout <= 0 {
		p.timeout = DefaultLookupTimeout
	}
	return p, nil
}

func (p *Policy) Decide(ctx context.Context, f engine.Failure) (engine.Decision, error) {
	if !f.Retryable() {
		return engine.Decision{Action: engine.FailureActionFail, Reason: engine.DecisionNotRetryable, Class: f.Class}, nil
	}
	settings, ok := p.settings(f.Deployment.Target)
	if !ok {
		settings = Settings{MaxAttempts: 1}
	}
	logger := p.logger.With(
		slog.Int64("deployment_id", int64(f.Deployment.ID)),
		slog.String("target", f.Deployment.Target.String()),
		slog.Int("attempt", f.Attempt),
	)
	in := Input{Failure: f}
	in.Pods, in.PodsInspected = p.pods(ctx, logger, f.Deployment)
	in.Earlier, in.Recent = p.diagnoses(ctx, logger, f.Deployment.ID)
	diagnosis := Classify(in)
	encoded, err := json.Marshal(diagnosis)
	if err != nil {
		return engine.Decision{}, err
	}
	decision := engine.Decision{
		Action:      engine.FailureActionFail,
		Class:       diagnosis.Verdict,
		Summary:     diagnosis.Summary,
		MaxAttempts: settings.MaxAttempts,
		Diagnosis:   encoded,
	}
	switch {
	case diagnosis.Verdict == core.FailureVersion:
		decision.Reason = engine.DecisionVersionCaused
	case diagnosis.Verdict == core.FailureConfiguration:
		decision.Reason = engine.DecisionNotRetryable
	case !settings.Enabled:
		decision.Reason = engine.DecisionRetriesDisabled
	case f.Attempt >= settings.MaxAttempts:
		decision.Reason = engine.DecisionRetriesExhausted
	case p.newerWaiting(ctx, logger, f.Deployment):
		decision.Reason = engine.DecisionNewerReleaseQueued
	default:
		decision.Action = engine.FailureActionRetry
		decision.Reason = engine.DecisionTransient
		decision.Delay = settings.Delay(f.Attempt)
	}
	return decision, nil
}

func (p *Policy) pods(ctx context.Context, logger *slog.Logger, d core.Deployment) ([]core.PodStatus, bool) {
	if p.diagnostics == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	pods, err := p.diagnostics.Pods(ctx, d)
	if err != nil {
		logger.WarnContext(ctx, "pod diagnostics unavailable; classifying from the Argo CD Application only", slog.String("error", err.Error()))
		return nil, false
	}
	return pods, true
}

func (p *Policy) diagnoses(ctx context.Context, logger *slog.Logger, id core.DeploymentID) ([]Diagnosis, []Diagnosis) {
	if p.history == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	earlier, recent, err := p.history.Diagnoses(ctx, id)
	if err != nil {
		logger.WarnContext(ctx, "read earlier diagnoses; classifying without them", slog.String("error", err.Error()))
		return nil, nil
	}
	return decodeAll(earlier), decodeAll(recent)
}

func decodeAll(raw []json.RawMessage) []Diagnosis {
	var out []Diagnosis
	for _, r := range raw {
		var d Diagnosis
		if err := json.Unmarshal(r, &d); err == nil {
			out = append(out, d)
		}
	}
	return out
}

func (p *Policy) newerWaiting(ctx context.Context, logger *slog.Logger, d core.Deployment) bool {
	if p.queue == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	waiting, err := p.queue.Waiting(ctx, d.Target, d.ID)
	if err != nil {
		logger.WarnContext(ctx, "read the queue; retrying without knowing whether a newer deployment waits", slog.String("error", err.Error()))
		return false
	}
	return waiting
}
