package gates_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
)

func trackedCommit(target core.Target, state core.ChecksState, checks ...core.Check) core.TrackedCommit {
	d := deployment(core.KindDeploy)
	evaluated := time.Date(2026, 11, 23, 11, 0, 0, 0, time.UTC)
	return core.TrackedCommit{
		ID: 1, Target: target, Commit: d.Commit, Branch: "main", Source: core.SourceWebhook,
		ChecksState: state, Checks: checks, EvaluatedAt: &evaluated, UpdatedAt: evaluated,
	}
}

func TestChecksGate(t *testing.T) {
	t.Parallel()

	prod := core.Target{App: "web", Environment: "prod"}
	staging := core.Target{App: "web", Environment: "staging"}
	green := core.Check{Name: "test", Kind: core.CheckKindRun, State: core.ChecksSuccess, Detail: "success"}
	running := core.Check{Name: "lint", Kind: core.CheckKindRun, State: core.ChecksPending, Detail: "in_progress"}
	red := core.Check{Name: "lint", Kind: core.CheckKindRun, State: core.ChecksFailure, Detail: "failure"}
	readAt := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		tracked     []core.TrackedCommit
		noLive      bool
		live        core.ChecksEvaluation
		wantStatus  core.GateStatus
		wantMessage string
		wantSource  string
		wantLive    bool
	}{
		{
			name:        "tracked success passes without GitHub",
			tracked:     []core.TrackedCommit{trackedCommit(prod, core.ChecksSuccess, green)},
			wantStatus:  core.GatePassed,
			wantMessage: "1 check passed",
			wantSource:  "tracked",
		},
		{
			name:        "tracked success with no checks",
			tracked:     []core.TrackedCommit{trackedCommit(prod, core.ChecksSuccess)},
			wantStatus:  core.GatePassed,
			wantMessage: "checks passed",
			wantSource:  "tracked",
		},
		{
			name:        "tracked for another environment reads live",
			tracked:     []core.TrackedCommit{trackedCommit(staging, core.ChecksSuccess, green)},
			live:        core.ChecksEvaluation{State: core.ChecksPending, Checks: []core.Check{green, running}, ReadAt: readAt},
			wantStatus:  core.GatePending,
			wantMessage: "waiting for checks: lint",
			wantSource:  "live",
			wantLive:    true,
		},
		{
			name:        "tracked pending reads live success",
			tracked:     []core.TrackedCommit{trackedCommit(prod, core.ChecksPending, running)},
			live:        core.ChecksEvaluation{State: core.ChecksSuccess, Checks: []core.Check{green}, ReadAt: readAt},
			wantStatus:  core.GatePassed,
			wantMessage: "1 check passed",
			wantSource:  "live",
			wantLive:    true,
		},
		{
			name:        "untracked commit reads live failure",
			live:        core.ChecksEvaluation{State: core.ChecksFailure, Checks: []core.Check{green, red}, ReadAt: readAt},
			wantStatus:  core.GateFailed,
			wantMessage: "checks failed: lint",
			wantSource:  "live",
			wantLive:    true,
		},
		{
			name:        "live pending with nothing reported",
			live:        core.ChecksEvaluation{State: core.ChecksPending, ReadAt: readAt},
			wantStatus:  core.GatePending,
			wantMessage: "waiting for checks",
			wantSource:  "live",
			wantLive:    true,
		},
		{
			name:        "tracked pending without live",
			tracked:     []core.TrackedCommit{trackedCommit(prod, core.ChecksPending, green, running)},
			noLive:      true,
			wantStatus:  core.GatePending,
			wantMessage: "waiting for checks: lint",
			wantSource:  "tracked",
		},
		{
			name:        "tracked failure without live",
			tracked:     []core.TrackedCommit{trackedCommit(prod, core.ChecksFailure, red)},
			noLive:      true,
			wantStatus:  core.GateFailed,
			wantMessage: "checks failed: lint",
			wantSource:  "tracked",
		},
		{
			name:        "tracked failure without failing names",
			tracked:     []core.TrackedCommit{trackedCommit(prod, core.ChecksFailure)},
			noLive:      true,
			wantStatus:  core.GateFailed,
			wantMessage: "checks failed",
			wantSource:  "tracked",
		},
		{
			name:        "untracked without live waits",
			tracked:     []core.TrackedCommit{trackedCommit(staging, core.ChecksSuccess)},
			noLive:      true,
			wantStatus:  core.GatePending,
			wantMessage: "commit is not tracked by doorstep; waiting for its checks",
			wantSource:  "tracked",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newBuiltinFakes()
			f.tracked.commits = tt.tracked
			f.live.eval = tt.live
			deps := f.deps()
			if tt.noLive {
				deps.Live = nil
			}
			g := builtinGate(t, deps, webApp(), config.Gate{Type: config.GateChecks})
			d := deployment(core.KindDeploy)
			ev := evaluateGate(t, g, d, nil)
			if ev.Status != tt.wantStatus || ev.Message != tt.wantMessage {
				t.Errorf("Evaluate = %s %q, want %s %q", ev.Status, ev.Message, tt.wantStatus, tt.wantMessage)
			}
			evidence := evidenceMap(t, ev)
			if evidence["source"] != tt.wantSource {
				t.Errorf("evidence source = %v, want %s (%s)", evidence["source"], tt.wantSource, ev.Evidence)
			}
			if _, ok := evidence["checks"].([]any); !ok {
				t.Errorf("evidence checks is not a list: %s", ev.Evidence)
			}
			if got := f.live.callCount(); (got > 0) != tt.wantLive {
				t.Errorf("live calls = %d, want live %v", got, tt.wantLive)
			}
			if tt.wantLive && !slices.Equal(f.live.required, []string{"test", "lint"}) {
				t.Errorf("live required = %v, want the app's required checks", f.live.required)
			}
			if want := []string{d.Commit.Repository + "@" + d.Commit.SHA}; !slices.Equal(f.tracked.calls, want) {
				t.Errorf("tracked calls = %v, want %v", f.tracked.calls, want)
			}
		})
	}
}

func TestChecksGatePrefersLatestTrackedRow(t *testing.T) {
	t.Parallel()

	f := newBuiltinFakes()
	prod := core.Target{App: "web", Environment: "prod"}
	older := trackedCommit(prod, core.ChecksFailure)
	newer := trackedCommit(prod, core.ChecksSuccess)
	newer.UpdatedAt = older.UpdatedAt.Add(time.Minute)
	f.tracked.commits = []core.TrackedCommit{newer, older}
	g := builtinGate(t, f.deps(), webApp(), config.Gate{Type: config.GateChecks})
	if ev := evaluateGate(t, g, deployment(core.KindDeploy), nil); ev.Status != core.GatePassed {
		t.Errorf("Evaluate = %s %q, want passed from the newer row", ev.Status, ev.Message)
	}
}

func TestChecksGateErrors(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	tests := []struct {
		name   string
		mutate func(*builtinFakes)
		want   error
	}{
		{name: "tracked read fails", mutate: func(f *builtinFakes) { f.tracked.err = boom }, want: boom},
		{name: "live read fails", mutate: func(f *builtinFakes) { f.live.err = boom }, want: boom},
		{name: "live returns unknown state", mutate: func(f *builtinFakes) { f.live.eval = core.ChecksEvaluation{State: "weird"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newBuiltinFakes()
			tt.mutate(f)
			g := builtinGate(t, f.deps(), webApp(), config.Gate{Type: config.GateChecks})
			_, err := g.Evaluate(t.Context(), gates.Request{Deployment: deployment(core.KindDeploy), Phase: core.GatePhaseBefore})
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Errorf("Evaluate error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestChecksGateEvidenceStaysBounded(t *testing.T) {
	t.Parallel()

	f := newBuiltinFakes()
	checks := make([]core.Check, 0, 2000)
	for i := range 2000 {
		checks = append(checks, core.Check{Name: fmt.Sprintf("check-%04d-%s", i, strings.Repeat("x", 40)), State: core.ChecksPending, Detail: "queued\x00"})
	}
	f.live.eval = core.ChecksEvaluation{State: core.ChecksPending, Checks: checks, ReadAt: f.clock.Now()}
	g := builtinGate(t, f.deps(), webApp(), config.Gate{Type: config.GateChecks})
	ev := evaluateGate(t, g, deployment(core.KindDeploy), nil)
	if len(ev.Evidence) > core.MaxGateEvidenceBytes {
		t.Fatalf("evidence is %d bytes", len(ev.Evidence))
	}
	evidence := evidenceMap(t, ev)
	kept := len(evidence["checks"].([]any))
	truncated, _ := evidence["truncated"].(float64)
	if kept == 0 || kept+int(truncated) != len(checks) {
		t.Errorf("kept %d and truncated %v of %d checks", kept, truncated, len(checks))
	}
	if !strings.HasSuffix(ev.Message, "and 1995 more") {
		t.Errorf("message = %q, want the first names and a count", ev.Message)
	}
}
