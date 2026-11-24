package gates

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	checksSourceTracked = "tracked"
	checksSourceLive    = "live"
	maxNamedChecks      = 5
	checksEvidenceLimit = core.MaxGateEvidenceBytes - 1024
)

type checksGate struct {
	repository string
	required   []string
	tracked    TrackedChecks
	live       LiveChecks
}

type checksEvidence struct {
	Source    string           `json:"source"`
	State     core.ChecksState `json:"state,omitempty"`
	ReadAt    string           `json:"read_at,omitempty"`
	Checks    []core.Check     `json:"checks"`
	Truncated int              `json:"truncated,omitempty"`
}

func (b Builtins) newChecks(app config.App, _ config.Gate) (Gate, error) {
	return checksGate{
		repository: string(app.Source.Repo),
		required:   slices.Clone(app.Source.RequiredChecks),
		tracked:    b.Tracked,
		live:       b.Live,
	}, nil
}

func (g checksGate) Evaluate(ctx context.Context, req Request) (Evaluation, error) {
	d := req.Deployment
	repository := cmp.Or(d.Commit.Repository, g.repository)
	commits, err := g.tracked.TrackedCommits(ctx, repository, d.Commit.SHA)
	if err != nil {
		return Evaluation{}, fmt.Errorf("read tracked commit %s@%s: %w", repository, d.Commit.SHA, err)
	}
	tracked, found := trackedFor(commits, d.Target)
	if found && tracked.ChecksState == core.ChecksSuccess {
		return checksVerdict(checksEvidence{Source: checksSourceTracked, State: tracked.ChecksState, ReadAt: readAt(tracked), Checks: tracked.Checks})
	}
	if g.live != nil {
		eval, err := g.live.Checks(ctx, repository, d.Commit.SHA, g.required)
		if err != nil {
			return Evaluation{}, fmt.Errorf("read checks of %s@%s: %w", repository, d.Commit.SHA, err)
		}
		if !eval.State.Valid() {
			return Evaluation{}, fmt.Errorf("read checks of %s@%s: unknown state %q", repository, d.Commit.SHA, eval.State)
		}
		ev := checksEvidence{Source: checksSourceLive, State: eval.State, Checks: eval.Checks}
		if !eval.ReadAt.IsZero() {
			ev.ReadAt = timestamp(eval.ReadAt)
		}
		return checksVerdict(ev)
	}
	if !found {
		evidence, err := marshalEvidence(checksEvidence{Source: checksSourceTracked, Checks: []core.Check{}})
		if err != nil {
			return Evaluation{}, err
		}
		return Evaluation{Status: core.GatePending, Message: "commit is not tracked by doorstep; waiting for its checks", Evidence: evidence}, nil
	}
	return checksVerdict(checksEvidence{Source: checksSourceTracked, State: tracked.ChecksState, ReadAt: readAt(tracked), Checks: tracked.Checks})
}

func trackedFor(commits []core.TrackedCommit, target core.Target) (core.TrackedCommit, bool) {
	var best core.TrackedCommit
	found := false
	for _, c := range commits {
		if c.Target != target {
			continue
		}
		if !found || c.UpdatedAt.After(best.UpdatedAt) {
			best, found = c, true
		}
	}
	return best, found
}

func readAt(c core.TrackedCommit) string {
	if c.EvaluatedAt == nil {
		return ""
	}
	return timestamp(*c.EvaluatedAt)
}

func checksVerdict(ev checksEvidence) (Evaluation, error) {
	status, message := core.GatePending, "waiting for checks"
	switch ev.State {
	case core.ChecksSuccess:
		status, message = core.GatePassed, "checks passed"
		if n := len(ev.Checks); n > 0 {
			message = fmt.Sprintf("%d %s passed", n, plural(n, "check", "checks"))
		}
	case core.ChecksFailure:
		status, message = core.GateFailed, "checks failed"
		if names := checkNames(ev.Checks, core.ChecksFailure); names != "" {
			message += ": " + names
		}
	case core.ChecksPending:
		if names := checkNames(ev.Checks, core.ChecksPending); names != "" {
			message += ": " + names
		}
	}
	evidence, err := checksEvidenceJSON(ev)
	if err != nil {
		return Evaluation{}, err
	}
	return Evaluation{Status: status, Message: message, Evidence: evidence}, nil
}

func checkNames(checks []core.Check, state core.ChecksState) string {
	var names []string
	for _, c := range checks {
		if c.State == state {
			names = append(names, c.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	if len(names) > maxNamedChecks {
		return strings.Join(names[:maxNamedChecks], ", ") + fmt.Sprintf(" and %d more", len(names)-maxNamedChecks)
	}
	return strings.Join(names, ", ")
}

func checksEvidenceJSON(ev checksEvidence) (json.RawMessage, error) {
	checks := make([]core.Check, len(ev.Checks))
	for i, c := range ev.Checks {
		checks[i] = core.Check{Name: clean(c.Name), Kind: c.Kind, State: c.State, Detail: clean(c.Detail)}
	}
	ev.Checks = checks
	total := len(ev.Checks)
	for {
		data, err := marshalEvidence(ev)
		if err != nil {
			return nil, err
		}
		if len(data) <= checksEvidenceLimit || len(ev.Checks) == 0 {
			return data, nil
		}
		ev.Checks = ev.Checks[:len(ev.Checks)/2]
		ev.Truncated = total - len(ev.Checks)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
