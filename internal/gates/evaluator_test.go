package gates_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

func TestNewEvaluatorValidatesOptions(t *testing.T) {
	t.Parallel()

	h := newHarness(t, gates.Plan{})
	tests := []struct {
		name   string
		mutate func(*gates.Options)
	}{
		{name: "no store", mutate: func(o *gates.Options) { o.Store = nil }},
		{name: "no logger", mutate: func(o *gates.Options) { o.Logger = nil }},
		{name: "negative timeout", mutate: func(o *gates.Options) { o.EvaluationTimeout = -time.Second }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := gates.Options{Store: h.store, Logger: discardLogger()}
			tt.mutate(&opts)
			if _, err := gates.NewEvaluator(opts); err == nil {
				t.Error("NewEvaluator accepted invalid options")
			}
		})
	}

	eval, err := gates.NewEvaluator(gates.Options{Store: h.store, Logger: discardLogger(), Plans: map[string]gates.Plan{"web": {After: []gates.Binding{bind("health", core.GatePhaseAfter, verdicts(core.GatePassed))}}}})
	if err != nil {
		t.Fatalf("NewEvaluator with defaults: %v", err)
	}
	out, err := eval.Evaluate(t.Context(), deployment(core.KindDeploy), core.GatePhaseAfter)
	if err != nil || out.Status != core.GatePassed {
		t.Errorf("Evaluate with default metrics and clock = %+v, %v", out, err)
	}
}

func TestEvaluateAggregatesPhase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		statuses []core.GateStatus
		want     core.GateStatus
	}{
		{name: "no gates", want: core.GatePassed},
		{name: "one passed", statuses: []core.GateStatus{core.GatePassed}, want: core.GatePassed},
		{name: "passed and skipped", statuses: []core.GateStatus{core.GatePassed, core.GateSkipped}, want: core.GatePassed},
		{name: "one pending", statuses: []core.GateStatus{core.GatePassed, core.GatePending}, want: core.GatePending},
		{name: "failed wins", statuses: []core.GateStatus{core.GatePending, core.GateFailed, core.GatePassed}, want: core.GateFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var plan gates.Plan
			var wantResults []string
			for i, s := range tt.statuses {
				name := "gate" + string(rune('a'+i))
				plan.After = append(plan.After, bind(name, core.GatePhaseAfter, verdicts(s)))
				wantResults = append(wantResults, name+"="+string(s))
			}
			h := newHarness(t, plan)
			out := mustEvaluate(t, h, deployment(core.KindDeploy), core.GatePhaseAfter)
			if out.Phase != core.GatePhaseAfter || out.Status != tt.want {
				t.Errorf("outcome = %s %s, want after %s", out.Phase, out.Status, tt.want)
			}
			if got := outcomeStatuses(out); !slices.Equal(got, wantResults) {
				t.Errorf("results = %v, want %v", got, wantResults)
			}
			for _, r := range out.Results {
				if r.DeploymentID != 7 || r.Phase != core.GatePhaseAfter || r.Type != r.Name {
					t.Errorf("result identity = %+v", r)
				}
			}
		})
	}
}

func TestEvaluateRunsOnlyRequestedPhase(t *testing.T) {
	t.Parallel()

	checks := verdicts(core.GatePassed)
	health := verdicts(core.GatePending)
	h := newHarness(t, gates.Plan{
		Before: []gates.Binding{bind("checks", core.GatePhaseBefore, checks)},
		After:  []gates.Binding{bind("health", core.GatePhaseAfter, health)},
	})
	d := deployment(core.KindDeploy)

	before := mustEvaluate(t, h, d, core.GatePhaseBefore)
	if before.Status != core.GatePassed || checks.calls() != 1 || health.calls() != 0 {
		t.Errorf("before: status %s, checks %d calls, health %d calls", before.Status, checks.calls(), health.calls())
	}
	if req := checks.lastRequest(); req.Phase != core.GatePhaseBefore || req.Deployment.ID != d.ID {
		t.Errorf("request = %+v", req)
	}

	after := mustEvaluate(t, h, d, core.GatePhaseAfter)
	if after.Status != core.GatePending || checks.calls() != 1 || health.calls() != 1 {
		t.Errorf("after: status %s, checks %d calls, health %d calls", after.Status, checks.calls(), health.calls())
	}
	if got := outcomeStatuses(after); !slices.Equal(got, []string{"health=pending"}) {
		t.Errorf("after results = %v", got)
	}
}

func TestEvaluateStickyTerminalResults(t *testing.T) {
	t.Parallel()

	passed := verdicts(core.GatePassed, core.GateFailed)
	failed := verdicts(core.GateFailed, core.GatePassed)
	pending := verdicts(core.GatePending, core.GatePassed)
	h := newHarness(t, gates.Plan{After: []gates.Binding{
		bind("passed", core.GatePhaseAfter, passed),
		bind("failed", core.GatePhaseAfter, failed),
		bind("pending", core.GatePhaseAfter, pending),
	}})
	d := deployment(core.KindDeploy)

	first := mustEvaluate(t, h, d, core.GatePhaseAfter)
	if got := outcomeStatuses(first); !slices.Equal(got, []string{"passed=passed", "failed=failed", "pending=pending"}) {
		t.Fatalf("first results = %v", got)
	}
	second := mustEvaluate(t, h, d, core.GatePhaseAfter)
	if got := outcomeStatuses(second); !slices.Equal(got, []string{"passed=passed", "failed=failed", "pending=passed"}) {
		t.Errorf("second results = %v", got)
	}
	if passed.calls() != 1 || failed.calls() != 1 || pending.calls() != 2 {
		t.Errorf("calls passed=%d failed=%d pending=%d, want 1 1 2", passed.calls(), failed.calls(), pending.calls())
	}
	if second.Status != core.GateFailed {
		t.Errorf("status = %s, want failed", second.Status)
	}

	third := mustEvaluate(t, h, d, core.GatePhaseAfter)
	if pending.calls() != 2 || third.Status != core.GateFailed {
		t.Errorf("third pass re-ran a resolved gate or changed the verdict: calls %d, status %s", pending.calls(), third.Status)
	}
}

func TestReevaluateRerunsEveryGate(t *testing.T) {
	t.Parallel()

	passed := verdicts(core.GatePassed, core.GatePending)
	failed := verdicts(core.GateFailed, core.GatePassed)
	h := newHarness(t, gates.Plan{After: []gates.Binding{
		bind("passed", core.GatePhaseAfter, passed),
		bind("failed", core.GatePhaseAfter, failed),
	}})
	d := deployment(core.KindDeploy)

	mustEvaluate(t, h, d, core.GatePhaseAfter)
	firstPassed, _ := h.store.get(d.ID, "passed")
	h.clock.Advance(time.Minute)

	out, err := h.eval.Reevaluate(t.Context(), d, core.GatePhaseAfter)
	if err != nil {
		t.Fatalf("Reevaluate: %v", err)
	}
	if passed.calls() != 2 || failed.calls() != 2 {
		t.Errorf("calls passed=%d failed=%d, want 2 2", passed.calls(), failed.calls())
	}
	if got := outcomeStatuses(out); !slices.Equal(got, []string{"passed=pending", "failed=passed"}) {
		t.Errorf("results = %v", got)
	}
	if out.Status != core.GatePending {
		t.Errorf("status = %s, want pending", out.Status)
	}
	reset := out.Results[0]
	if !reset.StartedAt.After(firstPassed.StartedAt) || reset.FinishedAt != nil {
		t.Errorf("terminal to pending kept StartedAt %v (was %v) or FinishedAt %v", reset.StartedAt, firstPassed.StartedAt, reset.FinishedAt)
	}
}

func TestEvaluateDoesNotShortCircuit(t *testing.T) {
	t.Parallel()

	first := verdicts(core.GateFailed)
	second := verdicts(core.GatePending)
	third := verdicts(core.GatePassed)
	h := newHarness(t, gates.Plan{Before: []gates.Binding{
		bind("first", core.GatePhaseBefore, first),
		bind("second", core.GatePhaseBefore, second),
		bind("third", core.GatePhaseBefore, third),
	}})

	out := mustEvaluate(t, h, deployment(core.KindDeploy), core.GatePhaseBefore)
	if out.Status != core.GateFailed {
		t.Errorf("status = %s, want failed", out.Status)
	}
	if first.calls() != 1 || second.calls() != 1 || third.calls() != 1 {
		t.Errorf("calls = %d %d %d, want every gate evaluated once", first.calls(), second.calls(), third.calls())
	}
	if n := h.store.recordCount(); n != 3 {
		t.Errorf("records = %d, want 3", n)
	}
}

func TestEvaluatePassesPreviousResult(t *testing.T) {
	t.Parallel()

	soak := &scriptedGate{verdicts: []gates.Evaluation{
		{Status: core.GatePending, Message: "soaking", Evidence: json.RawMessage(`{"since":"12:00"}`)},
		{Status: core.GatePassed, Message: "soaked"},
	}}
	h := newHarness(t, gates.Plan{After: []gates.Binding{bind("soak", core.GatePhaseAfter, soak)}})
	d := deployment(core.KindDeploy)

	mustEvaluate(t, h, d, core.GatePhaseAfter)
	if prev := soak.lastRequest().Previous; prev != nil {
		t.Errorf("first request Previous = %+v, want nil", prev)
	}
	mustEvaluate(t, h, d, core.GatePhaseAfter)
	prev := soak.lastRequest().Previous
	if prev == nil {
		t.Fatal("second request has no Previous")
	}
	if prev.Status != core.GatePending || prev.Message != "soaking" || string(prev.Evidence) != `{"since":"12:00"}` {
		t.Errorf("Previous = %+v", prev)
	}
}

func TestEvaluateTimesOutPendingGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		timeout time.Duration
		advance time.Duration
		want    core.GateStatus
	}{
		{name: "before the timeout", timeout: 10 * time.Minute, advance: 9 * time.Minute, want: core.GatePending},
		{name: "at the timeout", timeout: 10 * time.Minute, advance: 10 * time.Minute, want: core.GateFailed},
		{name: "past the timeout", timeout: 10 * time.Minute, advance: time.Hour, want: core.GateFailed},
		{name: "no timeout", timeout: 0, advance: 100 * time.Hour, want: core.GatePending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gate := &scriptedGate{verdicts: []gates.Evaluation{{Status: core.GatePending, Message: "progressing", Evidence: json.RawMessage(`{"health":"Progressing"}`)}}}
			b := bind("health", core.GatePhaseAfter, gate)
			b.Timeout = tt.timeout
			h := newHarness(t, gates.Plan{After: []gates.Binding{b}})
			d := deployment(core.KindDeploy)

			mustEvaluate(t, h, d, core.GatePhaseAfter)
			h.clock.Advance(tt.advance)
			out := mustEvaluate(t, h, d, core.GatePhaseAfter)
			if out.Status != tt.want {
				t.Fatalf("status = %s, want %s", out.Status, tt.want)
			}
			r := out.Results[0]
			if tt.want == core.GateFailed {
				if r.Message != "timed out after 10m0s" {
					t.Errorf("message = %q", r.Message)
				}
				if string(r.Evidence) != `{"health":"Progressing"}` {
					t.Errorf("evidence = %s, want the gate's last evidence", r.Evidence)
				}
				obs := h.metrics.all()
				if len(obs) != 1 || obs[0].result != metrics.GateResultFailed || obs[0].duration != tt.advance {
					t.Errorf("metrics = %+v, want one failed observation of %s", obs, tt.advance)
				}
				mustEvaluate(t, h, d, core.GatePhaseAfter)
				if gate.calls() != 2 {
					t.Errorf("timed-out gate evaluated %d times, want 2", gate.calls())
				}
			} else if len(h.metrics.all()) != 0 {
				t.Errorf("metrics recorded for a pending gate: %+v", h.metrics.all())
			}
		})
	}
}

func TestEvaluateGateErrorStaysPending(t *testing.T) {
	t.Parallel()

	gate := &scriptedGate{
		verdicts: []gates.Evaluation{
			{Status: core.GatePending, Message: "waiting", Evidence: json.RawMessage(`{"seen":1}`)},
			{Status: core.GatePassed},
		},
		errs: []error{nil, errors.New("prometheus unavailable"), errors.New("prometheus unavailable")},
	}
	b := bind("query", core.GatePhaseAfter, gate)
	b.Kind = metrics.GateKindMetrics
	b.Timeout = 5 * time.Minute
	h := newHarness(t, gates.Plan{After: []gates.Binding{b}})
	gate.before = func() { h.clock.Advance(2 * time.Second) }
	d := deployment(core.KindDeploy)

	mustEvaluate(t, h, d, core.GatePhaseAfter)
	out := mustEvaluate(t, h, d, core.GatePhaseAfter)
	r := out.Results[0]
	if out.Status != core.GatePending || r.Status != core.GatePending {
		t.Fatalf("status = %s, want pending", out.Status)
	}
	if r.Message != "evaluation error: prometheus unavailable" {
		t.Errorf("message = %q", r.Message)
	}
	if string(r.Evidence) != `{"seen":1}` {
		t.Errorf("evidence = %s, want the previous evidence", r.Evidence)
	}
	obs := h.metrics.all()
	want := observation{app: "web", gate: metrics.GateKindMetrics, phase: metrics.PhaseDeploy, result: metrics.GateResultError, duration: 2 * time.Second}
	if len(obs) != 1 || obs[0] != want {
		t.Errorf("metrics = %+v, want [%+v]", obs, want)
	}
	if !strings.Contains(h.logs.String(), "gate evaluation error") {
		t.Errorf("evaluation error not logged: %s", h.logs.String())
	}

	h.clock.Advance(5 * time.Minute)
	out = mustEvaluate(t, h, d, core.GatePhaseAfter)
	if out.Status != core.GateFailed || out.Results[0].Message != "timed out after 5m0s" {
		t.Errorf("after the timeout = %s %q, want failed by timeout", out.Status, out.Results[0].Message)
	}
	if string(out.Results[0].Evidence) != `{"seen":1}` {
		t.Errorf("timed-out evidence = %s", out.Results[0].Evidence)
	}
}

func TestEvaluateRejectsInvalidEvaluations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		evaluation gates.Evaluation
		message    string
	}{
		{name: "empty status", evaluation: gates.Evaluation{}, message: `evaluation error: gate returned unknown status ""`},
		{name: "unknown status", evaluation: gates.Evaluation{Status: "running"}, message: `evaluation error: gate returned unknown status "running"`},
		{name: "invalid JSON", evaluation: gates.Evaluation{Status: core.GatePassed, Evidence: json.RawMessage(`{"a":`)}, message: "evaluation error: gate evidence is not valid JSON"},
		{name: "oversized evidence", evaluation: gates.Evaluation{Status: core.GatePassed, Evidence: json.RawMessage(`"` + strings.Repeat("x", core.MaxGateEvidenceBytes) + `"`)}, message: "evaluation error: gate evidence is 65538 bytes, over the 65536 byte limit"},
		{name: "NUL in evidence", evaluation: gates.Evaluation{Status: core.GatePassed, Evidence: json.RawMessage(`{"a":"\u0000"}`)}, message: `evaluation error: gate evidence contains \u0000, which Postgres cannot store`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gate := &scriptedGate{verdicts: []gates.Evaluation{tt.evaluation}}
			h := newHarness(t, gates.Plan{Before: []gates.Binding{bind("checks", core.GatePhaseBefore, gate)}})
			out := mustEvaluate(t, h, deployment(core.KindDeploy), core.GatePhaseBefore)
			if out.Status != core.GatePending {
				t.Errorf("status = %s, want pending", out.Status)
			}
			if got := out.Results[0].Message; got != tt.message {
				t.Errorf("message = %q, want %q", got, tt.message)
			}
			if string(out.Results[0].Evidence) != "{}" {
				t.Errorf("evidence = %s, want {}", out.Results[0].Evidence)
			}
			obs := h.metrics.all()
			if len(obs) != 1 || obs[0].result != metrics.GateResultError {
				t.Errorf("metrics = %+v, want one error", obs)
			}
		})
	}
}

func TestEvaluateCleansMessages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message string
		want    string
	}{
		{name: "short", message: "ok", want: "ok"},
		{name: "exact limit", message: strings.Repeat("a", core.MaxGateMessageBytes), want: strings.Repeat("a", core.MaxGateMessageBytes)},
		{name: "over limit", message: strings.Repeat("a", core.MaxGateMessageBytes+10), want: strings.Repeat("a", core.MaxGateMessageBytes)},
		{name: "multibyte at limit", message: strings.Repeat("a", core.MaxGateMessageBytes-1) + "é", want: strings.Repeat("a", core.MaxGateMessageBytes-1)},
		{name: "NUL removed", message: "a\x00b", want: "ab"},
		{name: "invalid UTF-8 replaced", message: "a\xffb", want: "a�b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gate := &scriptedGate{verdicts: []gates.Evaluation{{Status: core.GatePassed, Message: tt.message}}}
			h := newHarness(t, gates.Plan{Before: []gates.Binding{bind("checks", core.GatePhaseBefore, gate)}})
			out := mustEvaluate(t, h, deployment(core.KindDeploy), core.GatePhaseBefore)
			got := out.Results[0].Message
			if got != tt.want {
				t.Errorf("message = %q (%d bytes), want %q (%d bytes)", got, len(got), tt.want, len(tt.want))
			}
			if !utf8.ValidString(got) || len(got) > core.MaxGateMessageBytes {
				t.Errorf("message is not valid UTF-8 within the limit: %d bytes", len(got))
			}
		})
	}
}

func TestEvaluateErrorMessageIsTruncated(t *testing.T) {
	t.Parallel()

	gate := &scriptedGate{errs: []error{errors.New(strings.Repeat("x", 4*core.MaxGateMessageBytes))}}
	h := newHarness(t, gates.Plan{Before: []gates.Binding{bind("checks", core.GatePhaseBefore, gate)}})
	out := mustEvaluate(t, h, deployment(core.KindDeploy), core.GatePhaseBefore)
	msg := out.Results[0].Message
	if len(msg) != core.MaxGateMessageBytes || !strings.HasPrefix(msg, "evaluation error: xxx") {
		t.Errorf("message has %d bytes and starts %q", len(msg), msg[:min(len(msg), 24)])
	}
}

func TestEvaluateCallTimeout(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	tests := []struct {
		name string
		gate gates.Gate
	}{
		{name: "gate honours its context", gate: gates.GateFunc(func(ctx context.Context, _ gates.Request) (gates.Evaluation, error) {
			<-ctx.Done()
			return gates.Evaluation{}, ctx.Err()
		})},
		{name: "gate ignores its context", gate: gates.GateFunc(func(context.Context, gates.Request) (gates.Evaluation, error) {
			<-release
			return gates.Evaluation{Status: core.GatePassed}, nil
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, gates.Plan{After: []gates.Binding{bind("health", core.GatePhaseAfter, tt.gate)}}, func(o *gates.Options) {
				o.EvaluationTimeout = 20 * time.Millisecond
			})
			out := mustEvaluate(t, h, deployment(core.KindDeploy), core.GatePhaseAfter)
			if out.Status != core.GatePending || out.Results[0].Message != "evaluation error: no verdict within 20ms" {
				t.Errorf("outcome = %s %q", out.Status, out.Results[0].Message)
			}
		})
	}
}

func TestEvaluateSkipsGateWithAbandonedCall(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	var calls atomic.Int32
	gate := gates.GateFunc(func(context.Context, gates.Request) (gates.Evaluation, error) {
		if calls.Add(1) == 1 {
			<-release
		}
		return gates.Evaluation{Status: core.GatePassed, Message: "ok"}, nil
	})
	h := newHarness(t, gates.Plan{After: []gates.Binding{bind("health", core.GatePhaseAfter, gate)}}, func(o *gates.Options) {
		o.EvaluationTimeout = 250 * time.Millisecond
	})
	d := deployment(core.KindDeploy)
	other := deployment(core.KindDeploy)
	other.ID = 8

	out := mustEvaluate(t, h, d, core.GatePhaseAfter)
	if out.Results[0].Message != "evaluation error: no verdict within 250ms" {
		t.Fatalf("first message = %q", out.Results[0].Message)
	}
	if !strings.Contains(h.logs.String(), "gate call abandoned while still running") {
		t.Errorf("abandoned call not logged: %s", h.logs.String())
	}

	out = mustEvaluate(t, h, d, core.GatePhaseAfter)
	if out.Status != core.GatePending || out.Results[0].Message != "evaluation error: previous call has not returned yet" {
		t.Errorf("while abandoned = %s %q", out.Status, out.Results[0].Message)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("gate called %d times while its abandoned call was running, want 1", n)
	}

	out = mustEvaluate(t, h, other, core.GatePhaseAfter)
	if out.Status != core.GatePassed {
		t.Errorf("other deployment = %s %q, want passed", out.Status, out.Results[0].Message)
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		out = mustEvaluate(t, h, d, core.GatePhaseAfter)
		if out.Status == core.GatePassed || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if out.Status != core.GatePassed {
		t.Errorf("after the abandoned call returned = %s %q, want passed", out.Status, out.Results[0].Message)
	}
}

func TestEvaluateMetricDurationIncludesCall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		statuses []core.GateStatus
		wait     time.Duration
		want     time.Duration
	}{
		{name: "verdict on first evaluation", statuses: []core.GateStatus{core.GatePassed}, want: 3 * time.Second},
		{name: "failed on first evaluation", statuses: []core.GateStatus{core.GateFailed}, want: 3 * time.Second},
		{name: "verdict after pending", statuses: []core.GateStatus{core.GatePending, core.GatePassed}, wait: time.Minute, want: time.Minute + 6*time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gate := verdicts(tt.statuses...)
			h := newHarness(t, gates.Plan{After: []gates.Binding{bind("health", core.GatePhaseAfter, gate)}})
			gate.before = func() { h.clock.Advance(3 * time.Second) }
			d := deployment(core.KindDeploy)
			for range tt.statuses {
				mustEvaluate(t, h, d, core.GatePhaseAfter)
				h.clock.Advance(tt.wait)
			}
			obs := h.metrics.all()
			if len(obs) != 1 || obs[0].duration != tt.want {
				t.Errorf("metrics = %+v, want one observation of %s", obs, tt.want)
			}
		})
	}
}

func TestEvaluateRecoversGatePanic(t *testing.T) {
	t.Parallel()

	gate := gates.GateFunc(func(context.Context, gates.Request) (gates.Evaluation, error) {
		panic("boom")
	})
	h := newHarness(t, gates.Plan{After: []gates.Binding{bind("health", core.GatePhaseAfter, gate)}})
	out := mustEvaluate(t, h, deployment(core.KindDeploy), core.GatePhaseAfter)
	if out.Status != core.GatePending || out.Results[0].Message != "evaluation error: gate panicked: boom" {
		t.Errorf("outcome = %s %q", out.Status, out.Results[0].Message)
	}
}

func TestEvaluateRollbackSkipsGates(t *testing.T) {
	t.Parallel()

	ci := verdicts(core.GatePassed)
	health := verdicts(core.GatePassed)
	soak := verdicts(core.GatePassed)
	ciBinding := bind("checks", core.GatePhaseBefore, ci)
	ciBinding.SkipOnRollback = true
	ciBinding.Kind = metrics.GateKindCI
	soakBinding := bind("soak", core.GatePhaseAfter, soak)
	soakBinding.SkipOnRollback = true
	soakBinding.Kind = metrics.GateKindSoak
	h := newHarness(t, gates.Plan{
		Before: []gates.Binding{ciBinding},
		After:  []gates.Binding{bind("health", core.GatePhaseAfter, health), soakBinding},
	})
	rollback := deployment(core.KindRollback)

	before := mustEvaluate(t, h, rollback, core.GatePhaseBefore)
	after := mustEvaluate(t, h, rollback, core.GatePhaseAfter)
	if before.Status != core.GatePassed || after.Status != core.GatePassed {
		t.Errorf("statuses = %s %s, want passed passed", before.Status, after.Status)
	}
	if ci.calls() != 0 || soak.calls() != 0 || health.calls() != 1 {
		t.Errorf("calls ci=%d soak=%d health=%d, want 0 0 1", ci.calls(), soak.calls(), health.calls())
	}
	for _, r := range []core.GateResult{before.Results[0], after.Results[1]} {
		if r.Status != core.GateSkipped || r.Message != "skipped for rollback" || r.FinishedAt == nil {
			t.Errorf("skipped result = %+v", r)
		}
	}
	want := []observation{{app: "web", gate: metrics.GateKindArgoCD, phase: metrics.PhaseRollback, result: metrics.GateResultPassed}}
	if got := h.metrics.all(); !slices.Equal(got, want) {
		t.Errorf("metrics = %+v, want %+v", got, want)
	}

	deploy := deployment(core.KindDeploy)
	deploy.ID = 8
	mustEvaluate(t, h, deploy, core.GatePhaseBefore)
	if ci.calls() != 1 {
		t.Errorf("deploy did not evaluate the skip-on-rollback gate")
	}
}

func TestEvaluateMetricsOncePerResolution(t *testing.T) {
	t.Parallel()

	gate := verdicts(core.GatePending, core.GatePending, core.GatePassed)
	b := bind("health", core.GatePhaseAfter, gate)
	h := newHarness(t, gates.Plan{After: []gates.Binding{b}})
	d := deployment(core.KindDeploy)

	for range 2 {
		mustEvaluate(t, h, d, core.GatePhaseAfter)
		h.clock.Advance(30 * time.Second)
		if obs := h.metrics.all(); len(obs) != 0 {
			t.Fatalf("metrics while pending = %+v", obs)
		}
	}
	for range 2 {
		mustEvaluate(t, h, d, core.GatePhaseAfter)
	}
	if _, err := h.eval.Reevaluate(t.Context(), d, core.GatePhaseAfter); err != nil {
		t.Fatalf("Reevaluate: %v", err)
	}
	want := []observation{{app: "web", gate: metrics.GateKindArgoCD, phase: metrics.PhaseDeploy, result: metrics.GateResultPassed, duration: time.Minute}}
	if got := h.metrics.all(); !slices.Equal(got, want) {
		t.Errorf("metrics = %+v, want %+v", got, want)
	}
}

func TestEvaluateNotifiesObserversOnStatusChange(t *testing.T) {
	t.Parallel()

	failing := &recordingObserver{err: errors.New("github unavailable")}
	gate := verdicts(core.GatePending, core.GatePending, core.GatePassed, core.GatePassed)
	h := newHarness(t, gates.Plan{After: []gates.Binding{bind("health", core.GatePhaseAfter, gate)}}, func(o *gates.Options) {
		o.Observers = append(o.Observers, failing)
	})
	d := deployment(core.KindDeploy)

	var last core.GateOutcome
	for range 3 {
		last = mustEvaluate(t, h, d, core.GatePhaseAfter)
	}
	if _, err := h.eval.Reevaluate(t.Context(), d, core.GatePhaseAfter); err != nil {
		t.Fatalf("Reevaluate: %v", err)
	}
	if last.Status != core.GatePassed {
		t.Errorf("status = %s, want passed despite the failing observer", last.Status)
	}
	want := []string{"health=pending", "health=passed"}
	for _, o := range []*recordingObserver{h.observer, failing} {
		if got := o.statuses(); !slices.Equal(got, want) {
			t.Errorf("observer saw %v, want %v", got, want)
		}
	}
	logs := h.logs.String()
	if strings.Count(logs, "gate observer failed") != 2 || !strings.Contains(logs, "github unavailable") || !strings.Contains(logs, "gate=health") {
		t.Errorf("observer failures not logged as expected: %s", logs)
	}
}

func TestEvaluateRejectsBadRequests(t *testing.T) {
	t.Parallel()

	h := newHarness(t, gates.Plan{})
	tests := []struct {
		name  string
		d     core.Deployment
		phase core.GatePhase
		want  error
	}{
		{name: "unknown phase", d: deployment(core.KindDeploy), phase: "during", want: gates.ErrInvalidPhase},
		{name: "empty phase", d: deployment(core.KindDeploy), phase: "", want: gates.ErrInvalidPhase},
		{name: "unknown app", d: func() core.Deployment {
			d := deployment(core.KindDeploy)
			d.Target.App = "api"
			return d
		}(), phase: core.GatePhaseBefore, want: gates.ErrNoPlan},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, fn := range []func(context.Context, core.Deployment, core.GatePhase) (core.GateOutcome, error){h.eval.Evaluate, h.eval.Reevaluate} {
				if _, err := fn(t.Context(), tt.d, tt.phase); !errors.Is(err, tt.want) {
					t.Errorf("err = %v, want %v", err, tt.want)
				}
			}
		})
	}
}

func TestEvaluateStoreErrors(t *testing.T) {
	t.Parallel()

	errDB := errors.New("connection reset")
	tests := []struct {
		name   string
		mutate func(*fakeStore)
	}{
		{name: "list fails", mutate: func(s *fakeStore) { s.listErr = errDB }},
		{name: "record fails", mutate: func(s *fakeStore) { s.recordErr = errDB }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			second := verdicts(core.GatePassed)
			h := newHarness(t, gates.Plan{After: []gates.Binding{
				bind("health", core.GatePhaseAfter, verdicts(core.GatePassed)),
				bind("soak", core.GatePhaseAfter, second),
			}})
			tt.mutate(h.store)
			_, err := h.eval.Evaluate(t.Context(), deployment(core.KindDeploy), core.GatePhaseAfter)
			if !errors.Is(err, errDB) {
				t.Fatalf("err = %v, want %v", err, errDB)
			}
			if second.calls() != 0 {
				t.Errorf("evaluation continued after a store error")
			}
			if obs := h.metrics.all(); len(obs) != 0 {
				t.Errorf("metrics after store error = %+v", obs)
			}
		})
	}
}

func TestEvaluateCancelledContext(t *testing.T) {
	t.Parallel()

	t.Run("before the pass", func(t *testing.T) {
		t.Parallel()
		gate := verdicts(core.GatePassed)
		h := newHarness(t, gates.Plan{After: []gates.Binding{bind("health", core.GatePhaseAfter, gate)}})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := h.eval.Evaluate(ctx, deployment(core.KindDeploy), core.GatePhaseAfter); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if gate.calls() != 0 || h.store.recordCount() != 0 {
			t.Errorf("calls %d, records %d, want none", gate.calls(), h.store.recordCount())
		}
	})

	t.Run("during a gate", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		gate := gates.GateFunc(func(gctx context.Context, _ gates.Request) (gates.Evaluation, error) {
			cancel()
			<-gctx.Done()
			return gates.Evaluation{}, gctx.Err()
		})
		next := verdicts(core.GatePassed)
		h := newHarness(t, gates.Plan{After: []gates.Binding{
			bind("health", core.GatePhaseAfter, gate),
			bind("soak", core.GatePhaseAfter, next),
		}})
		if _, err := h.eval.Evaluate(ctx, deployment(core.KindDeploy), core.GatePhaseAfter); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if next.calls() != 0 || h.store.recordCount() != 0 {
			t.Errorf("calls %d, records %d, want none", next.calls(), h.store.recordCount())
		}
		if obs := h.metrics.all(); len(obs) != 0 {
			t.Errorf("metrics after cancellation = %+v", obs)
		}
	})
}
