package gates

import (
	"context"
	"fmt"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
)

type soakGate struct {
	ref      applicationRef
	reader   ApplicationReader
	now      func() time.Time
	grace    time.Duration
	duration time.Duration
}

type soakEvidence struct {
	appEvidence
	HealthySince string `json:"healthy_since,omitempty"`
	Soak         string `json:"soak"`
	Until        string `json:"until,omitempty"`
}

func (b Builtins) newSoak(app config.App, gate config.Gate) (Gate, error) {
	if gate.Duration <= 0 {
		return nil, fmt.Errorf("soak gate %s needs a positive duration", gate.Name)
	}
	return soakGate{ref: applicationOf(app), reader: b.Applications, now: b.Now, grace: b.DegradedGrace, duration: gate.Duration.Std()}, nil
}

func (g soakGate) Evaluate(ctx context.Context, req Request) (Evaluation, error) {
	previous := pendingState[soakEvidence](req.Previous)
	o, err := observe(ctx, g.reader, g.ref)
	if err != nil {
		return Evaluation{}, err
	}
	now := g.now()
	ev := soakEvidence{appEvidence: o.evidence(g.ref), Soak: g.duration.String()}
	healthySince, soaking := parseTimestamp(previous.HealthySince)
	if soaking && (healthySince.After(now) || o.condition == appDegraded) {
		soaking = false
	}
	if !soaking && o.condition == appReady {
		healthySince, soaking = now, true
	}
	if soaking {
		ev.HealthySince = timestamp(healthySince)
		ev.Until = timestamp(healthySince.Add(g.duration))
	}

	status := core.GatePending
	message := o.message
	switch o.condition {
	case appReady:
		if now.Sub(healthySince) >= g.duration {
			status = core.GatePassed
			message = fmt.Sprintf("healthy for the %s soak", g.duration)
		} else {
			message = "soaking until " + ev.Until
		}
	case appDegraded:
		verdict := judgeDegraded(previous.DegradedSince, now, g.grace)
		ev.DegradedSince = timestamp(verdict.since)
		if verdict.failed {
			status = core.GateFailed
			message = withDetail(fmt.Sprintf("%s for %s while soaking", o.failure, g.grace), o.app.HealthMessage)
		}
	case appWaiting:
		ev.DegradedSince = carriedDegradedSince(previous.DegradedSince, now)
	}
	evidence, err := marshalEvidence(ev)
	if err != nil {
		return Evaluation{}, err
	}
	return Evaluation{Status: status, Message: message, Evidence: evidence}, nil
}
