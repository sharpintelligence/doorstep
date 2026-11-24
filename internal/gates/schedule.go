package gates

import (
	"context"
	"fmt"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
)

const maxNextOpenSteps = 100

type scheduleGate struct {
	windows          []config.ScheduledWindow
	applyToRollbacks bool
	hasAllow         bool
	now              func() time.Time
}

type scheduleEvidence struct {
	EvaluatedAt string           `json:"evaluated_at"`
	Active      []activeEvidence `json:"active"`
	NextAllow   string           `json:"next_allow,omitempty"`
}

type activeEvidence struct {
	Effect   config.WindowEffect `json:"effect"`
	Name     string              `json:"name,omitempty"`
	Cron     string              `json:"cron"`
	Timezone string              `json:"timezone"`
	Start    string              `json:"start"`
	End      string              `json:"end"`
}

type activeWindow struct {
	window config.ScheduledWindow
	start  time.Time
	end    time.Time
}

func (b Builtins) newSchedule(_ config.App, gate config.Gate) (Gate, error) {
	parsed, err := gate.ScheduleWindows()
	if err != nil {
		return nil, err
	}
	if len(parsed) == 0 {
		return nil, fmt.Errorf("schedule gate %s has no windows", gate.Name)
	}
	g := scheduleGate{windows: parsed, applyToRollbacks: gate.ApplyToRollbacks, now: b.Now}
	for _, w := range parsed {
		if w.Effect == config.WindowAllow {
			g.hasAllow = true
		}
	}
	return g, nil
}

func (g scheduleGate) Evaluate(ctx context.Context, req Request) (Evaluation, error) {
	if err := ctx.Err(); err != nil {
		return Evaluation{}, err
	}
	if req.Deployment.Kind == core.KindRollback && !g.applyToRollbacks {
		return Evaluation{Status: core.GateSkipped, Message: skippedForRollback}, nil
	}
	now := g.now()
	var deny, allow *activeWindow
	ev := scheduleEvidence{EvaluatedAt: timestamp(now), Active: []activeEvidence{}}
	for _, w := range g.windows {
		start, end, ok := w.Window.Active(now)
		if !ok {
			continue
		}
		active := activeWindow{window: w, start: start, end: end}
		ev.Active = append(ev.Active, activeEvidence{
			Effect: w.Effect, Name: w.Name, Cron: w.Cron, Timezone: w.Timezone,
			Start: timestamp(start), End: timestamp(end),
		})
		switch w.Effect {
		case config.WindowDeny:
			if deny == nil || end.After(deny.end) {
				deny = &active
			}
		case config.WindowAllow:
			if allow == nil || end.After(allow.end) {
				allow = &active
			}
		}
	}

	var status core.GateStatus
	var message string
	switch {
	case deny != nil:
		status = core.GatePending
		message = "deploys are frozen until " + timestamp(deny.end) + label(deny.window)
	case g.hasAllow && allow == nil:
		status = core.GatePending
		message = "outside deploy windows"
		if next := g.nextOpen(now); !next.IsZero() {
			ev.NextAllow = timestamp(next)
			message += "; next opens " + ev.NextAllow
		}
	case allow != nil:
		status = core.GatePassed
		message = "inside deploy window until " + timestamp(allow.end) + label(allow.window)
	default:
		status = core.GatePassed
		message = "no deny window is active"
	}
	evidence, err := marshalEvidence(ev)
	if err != nil {
		return Evaluation{}, err
	}
	return Evaluation{Status: status, Message: message, Evidence: evidence}, nil
}

func (g scheduleGate) nextOpen(now time.Time) time.Time {
	candidate := g.nextAllowFiring(now)
	for range maxNextOpenSteps {
		if candidate.IsZero() {
			return candidate
		}
		denyEnd, denied := g.latestDenyEnd(candidate)
		if !denied {
			return candidate
		}
		if g.allowActive(denyEnd) {
			candidate = denyEnd
		} else {
			candidate = g.nextAllowFiring(denyEnd)
		}
	}
	return time.Time{}
}

func (g scheduleGate) nextAllowFiring(after time.Time) time.Time {
	var next time.Time
	for _, w := range g.windows {
		if w.Effect != config.WindowAllow {
			continue
		}
		if t := w.Window.Next(after); !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	return next
}

func (g scheduleGate) latestDenyEnd(at time.Time) (time.Time, bool) {
	var latest time.Time
	denied := false
	for _, w := range g.windows {
		if w.Effect != config.WindowDeny {
			continue
		}
		if _, end, ok := w.Window.Active(at); ok && (!denied || end.After(latest)) {
			latest, denied = end, true
		}
	}
	return latest, denied
}

func (g scheduleGate) allowActive(at time.Time) bool {
	for _, w := range g.windows {
		if w.Effect != config.WindowAllow {
			continue
		}
		if _, _, ok := w.Window.Active(at); ok {
			return true
		}
	}
	return false
}

func label(w config.ScheduledWindow) string {
	if w.Name != "" {
		return " (" + w.Name + ")"
	}
	return " (" + w.Cron + ")"
}
