package intake

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

type LiveChecks struct {
	Source CheckSource
	Now    func() time.Time
}

func (l LiveChecks) Checks(ctx context.Context, repository, sha string, required []string) (core.ChecksEvaluation, error) {
	if l.Source == nil {
		return core.ChecksEvaluation{}, errors.New("live checks: no check source")
	}
	now := l.Now
	if now == nil {
		now = time.Now
	}
	var names []string
	if len(required) > 0 {
		names = uniqueNames(required)
	}
	readAt := now()
	snapshot, err := l.Source.Checks(ctx, repository, sha, names)
	if err != nil {
		return core.ChecksEvaluation{}, fmt.Errorf("read checks for %s@%s: %w", repository, sha, err)
	}
	state, checks := Evaluate(required, snapshot)
	return core.ChecksEvaluation{State: state, Checks: checks, ReadAt: readAt}, nil
}
