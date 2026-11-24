package gates

import (
	"context"
	"fmt"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
)

const maxLockEvidence = 50

type lockGate struct {
	freezes FreezeReader
}

type lockEvidence struct {
	Locks     []freezeEvidence `json:"locks"`
	Truncated int              `json:"truncated,omitempty"`
}

type freezeEvidence struct {
	Scope     string `json:"scope"`
	Owner     string `json:"owner"`
	Reason    string `json:"reason,omitempty"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

func (b Builtins) newLock(config.App, config.Gate) (Gate, error) {
	return lockGate{freezes: b.Freezes}, nil
}

func (g lockGate) Evaluate(ctx context.Context, req Request) (Evaluation, error) {
	locks, err := g.freezes.Blocking(ctx, req.Deployment.Target)
	if err != nil {
		return Evaluation{}, fmt.Errorf("read freezes of %s: %w", req.Deployment.Target, err)
	}
	ev := lockEvidence{Locks: make([]freezeEvidence, 0, min(len(locks), maxLockEvidence))}
	for i, l := range locks {
		if i == maxLockEvidence {
			ev.Truncated = len(locks) - maxLockEvidence
			break
		}
		f := freezeEvidence{Scope: l.Scope(), Owner: clean(l.Owner), Reason: clean(l.Reason), CreatedAt: timestamp(l.CreatedAt)}
		if l.ExpiresAt != nil {
			f.ExpiresAt = timestamp(*l.ExpiresAt)
		}
		ev.Locks = append(ev.Locks, f)
	}
	evidence, err := marshalEvidence(ev)
	if err != nil {
		return Evaluation{}, err
	}
	if len(locks) == 0 {
		return Evaluation{Status: core.GatePassed, Message: "no freeze", Evidence: evidence}, nil
	}
	return Evaluation{Status: core.GatePending, Message: freezeMessage(locks), Evidence: evidence}, nil
}

func freezeMessage(locks []core.Lock) string {
	first := locks[0]
	message := "frozen by " + first.Owner
	if first.Global {
		message = "globally " + message
	}
	if first.Reason != "" {
		message += ": " + first.Reason
	}
	if more := len(locks) - 1; more > 0 {
		message += fmt.Sprintf(" (and %d more %s)", more, plural(more, "freeze", "freezes"))
	}
	return message
}
