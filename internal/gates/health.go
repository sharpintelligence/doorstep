package gates

import (
	"context"
	"fmt"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
)

type healthGate struct {
	ref    applicationRef
	reader ApplicationReader
	now    func() time.Time
	grace  time.Duration
}

type healthEvidence struct {
	appEvidence
	PinCommit string `json:"pin_commit,omitempty"`
}

func (b Builtins) newHealth(app config.App, _ config.Gate) (Gate, error) {
	return healthGate{ref: applicationOf(app), reader: b.Applications, now: b.Now, grace: b.DegradedGrace}, nil
}

func (g healthGate) Evaluate(ctx context.Context, req Request) (Evaluation, error) {
	previous := pendingState[healthEvidence](req.Previous)
	o, err := observe(ctx, g.reader, g.ref)
	if err != nil {
		return Evaluation{}, err
	}
	now := g.now()
	ev := healthEvidence{appEvidence: o.evidence(g.ref), PinCommit: clean(req.Deployment.PinCommitSHA)}
	status := core.GatePending
	message := o.message
	switch o.condition {
	case appReady:
		status = core.GatePassed
	case appDegraded:
		verdict := judgeDegraded(previous.DegradedSince, now, g.grace)
		ev.DegradedSince = timestamp(verdict.since)
		if verdict.failed {
			status = core.GateFailed
			message = withDetail(fmt.Sprintf("%s for %s", o.failure, g.grace), o.app.HealthMessage)
		}
	case appWaiting:
	}
	evidence, err := marshalEvidence(ev)
	if err != nil {
		return Evaluation{}, err
	}
	return Evaluation{Status: status, Message: message, Evidence: evidence}, nil
}
