package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/engine"
)

type eventData struct {
	Stage            core.State             `json:"stage"`
	FailureClass     core.FailureClass      `json:"failure_class"`
	SyncReason       core.SyncFailureReason `json:"sync_reason"`
	Attempts         int                    `json:"attempts"`
	Decision         engine.DecisionReason  `json:"decision"`
	RollbackEligible bool                   `json:"rollback_eligible"`
	Diagnosis        json.RawMessage        `json:"diagnosis"`
	Retry            *struct {
		Attempt     int                   `json:"attempt"`
		NextAttempt int                   `json:"next_attempt"`
		MaxAttempts int                   `json:"max_attempts"`
		DelayMS     int64                 `json:"delay_ms"`
		Reason      engine.DecisionReason `json:"reason"`
		Summary     string                `json:"summary"`
	} `json:"retry"`
}

func decodeEventData(t *testing.T, tr engine.Transition) eventData {
	t.Helper()
	var data eventData
	if err := json.Unmarshal(tr.Data, &data); err != nil {
		t.Fatalf("decode event data %s: %v", tr.Data, err)
	}
	return data
}

func lastEventData(t *testing.T, h *harness) eventData {
	t.Helper()
	recorded := h.store.recorded()
	if len(recorded) == 0 {
		t.Fatal("no transitions recorded")
	}
	return decodeEventData(t, recorded[len(recorded)-1])
}

func degradedSync(context.Context, string) (core.SyncStatus, error) {
	return core.SyncStatus{Failure: &core.SyncFailure{Reason: core.SyncReasonDegraded, Class: core.FailureVersion, Message: "pod pending"}},
		fmt.Errorf("argocd: %w after 10m", core.ErrSyncTimeout)
}

const diagnosis = `{"verdict":"transient","findings":[{"signal":"image_pull","node":"pi-3"}]}`

func retryUpTo(delay time.Duration) func(engine.Failure) engine.Decision {
	const maxAttempts = 3
	return func(f engine.Failure) engine.Decision {
		if f.Attempt >= maxAttempts {
			return engine.Decision{Action: engine.FailureActionFail, Reason: engine.DecisionRetriesExhausted, Class: core.FailureTransient, Summary: "transient: image pull failing on node pi-3", MaxAttempts: maxAttempts, Diagnosis: json.RawMessage(diagnosis)}
		}
		return engine.Decision{
			Action:      engine.FailureActionRetry,
			Reason:      engine.DecisionTransient,
			Class:       core.FailureTransient,
			Summary:     "transient: image pull failing on node pi-3",
			Delay:       delay,
			MaxAttempts: maxAttempts,
			Diagnosis:   json.RawMessage(diagnosis),
		}
	}
}

func TestHandleSyncFailureIsRetried(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.sync.waitFn = degradedSync
	h.policy.decide = retryUpTo(time.Minute)
	d := h.store.add(in(core.StateSyncing, deploy()))

	result, err := h.run(t, t.Context(), d.ID)
	if err != nil || result != (engine.Result{RequeueAfter: time.Minute}) {
		t.Fatalf("Handle = %+v, %v, want a release for the backoff", result, err)
	}
	recorded := h.store.recorded()
	if len(recorded) != 1 || recorded[0].From != core.StateSyncing || recorded[0].To != core.StateSyncing || recorded[0].FailureReason != "" {
		t.Fatalf("transitions = %+v, want one syncing -> syncing", recorded)
	}
	if want := "attempt 1 failed; retrying (attempt 2 of 3) in 1m"; recorded[0].Message != want {
		t.Errorf("message = %q, want %q", recorded[0].Message, want)
	}
	data := decodeEventData(t, recorded[0])
	if data.Stage != core.StateSyncing || data.FailureClass != core.FailureTransient || data.SyncReason != core.SyncReasonDegraded {
		t.Errorf("event data = %+v", data)
	}
	if r := data.Retry; r == nil || r.Attempt != 1 || r.NextAttempt != 2 || r.MaxAttempts != 3 || r.DelayMS != 60000 || r.Reason != engine.DecisionTransient || r.Summary != "transient: image pull failing on node pi-3" {
		t.Errorf("retry data = %+v", data.Retry)
	}
	if string(data.Diagnosis) != diagnosis {
		t.Errorf("diagnosis = %s, want %s", data.Diagnosis, diagnosis)
	}
	if got := h.store.deployment(d.ID); got.State != core.StateSyncing || got.FailureReason != "" {
		t.Errorf("deployment = %s %q, want still syncing", got.State, got.FailureReason)
	}
	notes := h.notifier.all()
	if len(notes) != 1 || notes[0].Kind != core.NotificationDeployRetrying {
		t.Fatalf("notifications = %v, want one deploy.retrying", h.notifier.kinds())
	}
	if want := "attempt 1 of 3 failed (transient: image pull failing on node pi-3); retrying (attempt 2 of 3) in 1m"; notes[0].Message != want {
		t.Errorf("notification message = %q, want %q", notes[0].Message, want)
	}
	if got := h.metrics.retries(); !slices.Equal(got, []core.FailureClass{core.FailureTransient}) {
		t.Errorf("retry metrics = %v", got)
	}
	if len(h.metrics.outcomes()) != 0 {
		t.Errorf("finished metrics = %v, want none while retrying", h.metrics.outcomes())
	}
	if f := h.policy.decided(); len(f) != 1 || f[0].Attempt != 1 || f[0].Sync == nil {
		t.Errorf("policy saw %+v", f)
	}
}

func TestHandleRetryWaitsOutTheBackoff(t *testing.T) {
	t.Parallel()

	h := newHarness()
	var failing atomic.Bool
	failing.Store(true)
	h.sync.waitFn = func(ctx context.Context, revision string) (core.SyncStatus, error) {
		if failing.Load() {
			return degradedSync(ctx, revision)
		}
		return syncedStatus(), nil
	}
	h.policy.decide = retryUpTo(time.Minute)
	d := h.store.add(in(core.StateSyncing, deploy()))
	h.clock.Advance(syncTimeout)

	if result, err := h.run(t, t.Context(), d.ID); err != nil || result.RequeueAfter != time.Minute {
		t.Fatalf("first claim = %+v, %v", result, err)
	}
	failing.Store(false)

	steps := []struct {
		advance time.Duration
		want    time.Duration
	}{
		{advance: 0, want: time.Minute},
		{advance: 40 * time.Second, want: 20 * time.Second},
	}
	for _, step := range steps {
		h.clock.Advance(step.advance)
		result, err := h.run(t, t.Context(), d.ID)
		if err != nil || result.RequeueAfter != step.want {
			t.Fatalf("early claim after %s = %+v, %v, want a release for %s", step.advance, result, err, step.want)
		}
	}
	if waits := h.sync.waitCalls(); len(waits) != 1 {
		t.Fatalf("WaitSynced calls = %d, want only the first attempt's", len(waits))
	}

	h.clock.Advance(25 * time.Second)
	if _, err := h.run(t, t.Context(), d.ID); err != nil {
		t.Fatalf("claim after the backoff: %v", err)
	}
	waits := h.sync.waitCalls()
	if len(waits) != 2 {
		t.Fatalf("WaitSynced calls = %d, want 2", len(waits))
	}
	if want := syncTimeout - 5*time.Second; waits[1].timeout < want-5*time.Second || waits[1].timeout > want {
		t.Errorf("second attempt waits %s, want a fresh window of about %s", waits[1].timeout, want)
	}
	if got := h.store.deployment(d.ID); got.State != core.StateSucceeded {
		t.Fatalf("state = %s, want succeeded", got.State)
	}
	notes := h.notifier.all()
	if kinds := h.notifier.kinds(); !slices.Equal(kinds, []core.NotificationKind{core.NotificationDeployRetrying, core.NotificationDeploySucceeded}) {
		t.Fatalf("notifications = %v", kinds)
	}
	if want := "succeeded on attempt 2 of 3"; notes[1].Message != want {
		t.Errorf("success message = %q, want %q", notes[1].Message, want)
	}
}

func TestHandleRetriesUntilTheBudgetIsSpent(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.sync.waitFn = degradedSync
	h.policy.decide = retryUpTo(0)
	d := h.store.add(in(core.StateSyncing, deploy()))

	if result, err := h.run(t, t.Context(), d.ID); err != nil || result != (engine.Result{}) {
		t.Fatalf("Handle = %+v, %v", result, err)
	}
	if got := h.store.states(); !slices.Equal(got, []core.State{core.StateSyncing, core.StateSyncing, core.StateFailed}) {
		t.Fatalf("transitions = %v", got)
	}
	var attempts []int
	for _, f := range h.policy.decided() {
		attempts = append(attempts, f.Attempt)
	}
	if !slices.Equal(attempts, []int{1, 2, 3}) {
		t.Errorf("policy saw attempts %v, want 1, 2, 3", attempts)
	}
	recorded := h.store.recorded()
	if want := "attempt 2 failed; retrying (attempt 3 of 3) now"; recorded[1].Message != want {
		t.Errorf("second retry message = %q, want %q", recorded[1].Message, want)
	}
	last := recorded[2]
	if last.Message != "failed while syncing after 3 attempts" {
		t.Errorf("failed message = %q", last.Message)
	}
	data := decodeEventData(t, last)
	if data.Attempts != 3 || data.Decision != engine.DecisionRetriesExhausted || !data.RollbackEligible || data.Retry != nil || string(data.Diagnosis) != diagnosis {
		t.Errorf("failure data = %+v", data)
	}
	notes := h.notifier.all()
	if kinds := h.notifier.kinds(); !slices.Equal(kinds, []core.NotificationKind{core.NotificationDeployRetrying, core.NotificationDeployFailed}) {
		t.Fatalf("notifications = %v, want one retrying and the final failure", kinds)
	}
	if want := "still failing after 3 attempts: transient: image pull failing on node pi-3"; notes[1].Message != want {
		t.Errorf("failure message = %q, want %q", notes[1].Message, want)
	}
	if got := len(h.metrics.retries()); got != 2 {
		t.Errorf("retry metrics = %d, want 2", got)
	}
	if got := h.metrics.outcomes(); !slices.Equal(got, []engine.Outcome{engine.OutcomeFailed}) {
		t.Errorf("finished metrics = %v", got)
	}
}

func TestHandleFinalDecisions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		kind         core.Kind
		decision     engine.Decision
		wantMessage  string
		wantEligible bool
		wantClass    core.FailureClass
	}{
		{
			name:         "version caused",
			decision:     engine.Decision{Action: engine.FailureActionFail, Reason: engine.DecisionVersionCaused, Class: core.FailureVersion, Summary: "version: crash loop (exit code 1) on nodes pi-1, pi-2"},
			wantMessage:  "version: crash loop (exit code 1) on nodes pi-1, pi-2; not retried",
			wantEligible: true,
			wantClass:    core.FailureVersion,
		},
		{
			name:        "newer release queued",
			decision:    engine.Decision{Action: engine.FailureActionFail, Reason: engine.DecisionNewerReleaseQueued, Class: core.FailureTransient, Summary: "transient: node lost"},
			wantMessage: "not retried because a newer deployment is waiting: transient: node lost",
			wantClass:   core.FailureTransient,
		},
		{
			name:        "retries disabled",
			decision:    engine.Decision{Action: engine.FailureActionFail, Reason: engine.DecisionRetriesDisabled},
			wantMessage: "not retried because retries are disabled",
			wantClass:   core.FailureVersion,
		},
		{
			name:        "configuration",
			decision:    engine.Decision{Action: engine.FailureActionFail, Reason: engine.DecisionNotRetryable, Class: core.FailureConfiguration, Summary: "configuration: not_found"},
			wantMessage: "configuration: not_found",
			wantClass:   core.FailureConfiguration,
		},
		{
			name:        "rollbacks are never eligible",
			kind:        core.KindRollback,
			decision:    engine.Decision{Action: engine.FailureActionFail, Reason: engine.DecisionVersionCaused, Class: core.FailureVersion},
			wantClass:   core.FailureVersion,
			wantMessage: "",
		},
		{
			name:        "diagnosis that is not an object is dropped",
			decision:    engine.Decision{Action: engine.FailureActionFail, Reason: engine.DecisionNotRetryable, Diagnosis: json.RawMessage(`["x"]`)},
			wantClass:   core.FailureVersion,
			wantMessage: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			h.sync.waitFn = degradedSync
			h.policy.decide = func(engine.Failure) engine.Decision { return tt.decision }
			d := deploy()
			if tt.kind == core.KindRollback {
				bad := h.store.add(in(core.StateFailed, deploy()))
				d.Kind = core.KindRollback
				d.RollbackOfID = &bad.ID
			}
			d = h.store.add(in(core.StateSyncing, d))

			if _, err := h.run(t, t.Context(), d.ID); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			data := lastEventData(t, h)
			if data.RollbackEligible != tt.wantEligible || data.FailureClass != tt.wantClass || data.Attempts != 1 {
				t.Errorf("failure data = %+v, want eligible %t class %s", data, tt.wantEligible, tt.wantClass)
			}
			if len(data.Diagnosis) != 0 {
				t.Errorf("diagnosis = %s, want none", data.Diagnosis)
			}
			notes := h.notifier.all()
			if len(notes) != 1 || notes[0].Kind != core.NotificationDeployFailed || notes[0].Message != tt.wantMessage {
				t.Errorf("notifications = %+v, want deploy.failed with %q", notes, tt.wantMessage)
			}
		})
	}
}

func TestHandleRetryNeedsAnArgoCDFailure(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.policy.action = engine.FailureActionRetry
	d := h.store.add(in(core.StateSyncing, deploy()))
	h.apps = nil

	if _, err := h.run(t, t.Context(), d.ID); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := h.store.states(); !slices.Equal(got, []core.State{core.StateFailed}) {
		t.Errorf("transitions = %v, want failed without a retry", got)
	}
}

func TestHandleRetryLosesTheRace(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.sync.waitFn = degradedSync
	var id core.DeploymentID
	h.policy.decide = func(f engine.Failure) engine.Decision {
		h.store.setState(id, core.StateCancelled)
		return retryUpTo(time.Minute)(f)
	}
	d := h.store.add(in(core.StateSyncing, deploy()))
	id = d.ID

	result, err := h.run(t, t.Context(), d.ID)
	if err != nil || result != (engine.Result{}) {
		t.Fatalf("Handle = %+v, %v, want a quiet stop", result, err)
	}
	if len(h.store.recorded()) != 0 || len(h.notifier.kinds()) != 0 || len(h.metrics.retries()) != 0 {
		t.Errorf("lost race recorded %v, announced %v, counted %v", h.store.states(), h.notifier.kinds(), h.metrics.retries())
	}
}

func TestHandleRetryStateErrors(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.store.failRetry = errors.New("connection reset")
	d := h.store.add(in(core.StateSyncing, deploy()))

	if _, err := h.run(t, t.Context(), d.ID); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("Handle error = %v, want the store error", err)
	}
	if len(h.store.recorded()) != 0 || len(h.sync.waitCalls()) != 0 {
		t.Errorf("acted without the retry state: %v, %d waits", h.store.states(), len(h.sync.waitCalls()))
	}
}

func TestHandleRollbackIsRetried(t *testing.T) {
	t.Parallel()

	h := newHarness()
	var calls atomic.Int32
	h.sync.waitFn = func(ctx context.Context, revision string) (core.SyncStatus, error) {
		if calls.Add(1) == 1 {
			return degradedSync(ctx, revision)
		}
		return syncedStatus(), nil
	}
	h.policy.decide = retryUpTo(0)
	bad := h.store.add(in(core.StateFailed, deploy()))
	rb := deploy()
	rb.Kind = core.KindRollback
	rb.RollbackOfID = &bad.ID
	rb.Digest = digestOld
	d := h.store.add(in(core.StateSyncing, rb))

	if _, err := h.run(t, t.Context(), d.ID); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := h.store.deployment(d.ID); got.State != core.StateSucceeded {
		t.Fatalf("state = %s, want succeeded", got.State)
	}
	notes := h.notifier.all()
	if kinds := h.notifier.kinds(); !slices.Equal(kinds, []core.NotificationKind{core.NotificationDeployRetrying, core.NotificationDeployRolledBack}) {
		t.Fatalf("notifications = %v", kinds)
	}
	if notes[1].Message != "succeeded on attempt 2 of 3" {
		t.Errorf("rolled back message = %q", notes[1].Message)
	}
}

func TestHandleRetryWithoutPinCommit(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.sync.health = []core.Health{{Status: core.HealthDegraded, Message: "pod pending"}, {Status: core.HealthHealthy}}
	h.policy.decide = retryUpTo(time.Minute)
	d := in(core.StateSyncing, deploy())
	d.PinCommitSHA = ""
	d = h.store.add(d)
	h.clock.Advance(syncTimeout)

	if result, err := h.run(t, t.Context(), d.ID); err != nil || result.RequeueAfter != time.Minute {
		t.Fatalf("first claim = %+v, %v, want a release for the backoff", result, err)
	}
	if got := h.store.states(); !slices.Equal(got, []core.State{core.StateSyncing}) {
		t.Fatalf("transitions = %v", got)
	}
	if data := lastEventData(t, h); data.SyncReason != core.SyncReasonDegraded || data.Retry == nil {
		t.Errorf("retry data = %+v", data)
	}
	h.clock.Advance(time.Minute)
	if _, err := h.run(t, t.Context(), d.ID); err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if got := h.store.deployment(d.ID); got.State != core.StateSucceeded {
		t.Fatalf("state = %s, want succeeded", got.State)
	}
}

func TestHandleNewWindowAfterRetry(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.sync.health = []core.Health{{Status: core.HealthDegraded}, {Status: core.HealthProgressing}}
	h.policy.decide = retryUpTo(0)
	d := in(core.StateSyncing, deploy())
	d.PinCommitSHA = ""
	d = h.store.add(d)
	h.clock.Advance(syncTimeout)

	result, err := h.run(t, t.Context(), d.ID)
	if err != nil || result != (engine.Result{RequeueAfter: engine.DefaultVerifyPollInterval}) {
		t.Fatalf("Handle = %+v, %v, want a poll in the new window", result, err)
	}
	if got := h.store.states(); !slices.Equal(got, []core.State{core.StateSyncing}) {
		t.Errorf("transitions = %v, want one retry and no failure", got)
	}
}
