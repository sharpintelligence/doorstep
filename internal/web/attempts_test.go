package web

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

func retryEvent(data string) core.Event {
	return core.Event{Type: core.EventTransition, From: core.StateSyncing, To: core.StateSyncing, OccurredAt: testNow, Message: "retrying", Data: json.RawMessage(data)}
}

func failedEvent(from core.State, data string) core.Event {
	return core.Event{Type: core.EventTransition, From: from, To: core.StateFailed, OccurredAt: testNow, Message: "failed", Data: json.RawMessage(data)}
}

func TestAttemptsFrom(t *testing.T) {
	t.Parallel()

	exit := 137
	tests := []struct {
		name   string
		events []core.Event
		want   []attemptView
	}{
		{name: "no events"},
		{
			name: "unrelated transitions",
			events: []core.Event{
				{Type: core.EventTransition, To: core.StateQueued},
				{Type: core.EventTransition, From: core.StateSyncing, To: core.StateVerifying},
				{Type: core.EventGate, From: core.StateSyncing, To: core.StateSyncing, Data: json.RawMessage(`{"retry":{"attempt":1}}`)},
				failedEvent(core.StateResolving, `{"stage":"resolving"}`),
			},
		},
		{
			name: "one retry",
			events: []core.Event{retryEvent(`{
				"sync_reason":"degraded",
				"retry":{"attempt":1,"next_attempt":2,"max_attempts":3,"delay_ms":60000,"reason":"transient","summary":"image pull failing"},
				"diagnosis":{"verdict":"transient","summary":"image pull failing on pi-3","attempt":1,
					"findings":[{"signal":"image_pull","node":"pi-3","pods":2,"reason":"ImagePullBackOff","exit_code":137,"detail":"d"}],
					"nodes":["pi-3"],"evidence":["e1"],"pods_inspected":true,"unknown_field":1}}`)},
			want: []attemptView{{
				Number: 1, MaxAttempts: 3, OccurredAt: testNow, Verdict: "transient", Summary: "image pull failing on pi-3", SyncReason: "degraded",
				NextAttempt: 2, Delay: time.Minute, HasDiagnosis: true, Message: "retrying",
				Findings: []findingView{{Signal: "image_pull", Node: "pi-3", Pods: 2, Reason: "ImagePullBackOff", ExitCode: &exit, Detail: "d"}},
				Nodes:    []string{"pi-3"}, Evidence: []string{"e1"},
			}},
		},
		{
			name: "retries then a final failure",
			events: []core.Event{
				retryEvent(`{"retry":{"attempt":1,"next_attempt":2,"max_attempts":3,"delay_ms":1000,"reason":"unknown"}}`),
				retryEvent(`{"retry":{"attempt":2,"next_attempt":3,"max_attempts":3,"delay_ms":2000,"reason":"unknown"}}`),
				failedEvent(core.StateSyncing, `{"attempts":3,"decision":"retries_exhausted","rollback_eligible":true,"diagnosis":{"verdict":"version","summary":"crash loop","pods_inspected":false}}`),
			},
			want: []attemptView{
				{Number: 1, MaxAttempts: 3, OccurredAt: testNow, Verdict: "unknown", NextAttempt: 2, Delay: time.Second, Message: "retrying"},
				{Number: 2, MaxAttempts: 3, OccurredAt: testNow, Verdict: "unknown", NextAttempt: 3, Delay: 2 * time.Second, Message: "retrying"},
				{Number: 3, MaxAttempts: 3, OccurredAt: testNow, Verdict: "version", Summary: "crash loop", Decision: "retries_exhausted", Final: true, RollbackEligible: true, HasDiagnosis: true, PodsNotInspected: true, Message: "failed"},
			},
		},
		{
			name:   "failure without retries but with a diagnosis",
			events: []core.Event{failedEvent(core.StateSyncing, `{"decision":"version_caused","diagnosis":{"verdict":"version","summary":"s"}}`)},
			want: []attemptView{
				{Number: 1, OccurredAt: testNow, Verdict: "version", Summary: "s", Decision: "version_caused", Final: true, HasDiagnosis: true, Message: "failed"},
			},
		},
		{
			name:   "failure without a diagnosis is not an attempt",
			events: []core.Event{failedEvent(core.StateSyncing, `{"attempts":2,"decision":"not_retryable"}`)},
		},
		{
			name:   "verifying failure after retried syncs",
			events: []core.Event{failedEvent(core.StateVerifying, `{"stage":"verifying","attempts":2,"decision":"version_caused","diagnosis":{"verdict":"version"}}`)},
			want: []attemptView{
				{Number: 2, OccurredAt: testNow, Verdict: "version", Decision: "version_caused", Final: true, HasDiagnosis: true, Message: "failed"},
			},
		},
		{
			name:   "number falls back to the diagnosis and then to the ordinal",
			events: []core.Event{retryEvent(`{"diagnosis":{"attempt":4,"verdict":"unknown"}}`), retryEvent(`{}`)},
			want: []attemptView{
				{Number: 2, OccurredAt: testNow, Message: "retrying"},
				{Number: 4, OccurredAt: testNow, Verdict: "unknown", HasDiagnosis: true, Message: "retrying"},
			},
		},
		{
			name:   "sorted by attempt number",
			events: []core.Event{retryEvent(`{"retry":{"attempt":2}}`), retryEvent(`{"retry":{"attempt":1}}`)},
			want: []attemptView{
				{Number: 1, OccurredAt: testNow, Message: "retrying"},
				{Number: 2, OccurredAt: testNow, Message: "retrying"},
			},
		},
		{
			name:   "malformed data keeps the message",
			events: []core.Event{retryEvent(`{"retry":`), retryEvent(`[1,2]`), retryEvent(``), retryEvent(`null`), retryEvent(`{"retry":{"attempt":"x"}}`)},
			want: []attemptView{
				{Number: 1, OccurredAt: testNow, Message: "retrying"},
				{Number: 2, OccurredAt: testNow, Message: "retrying"},
				{Number: 3, OccurredAt: testNow, Message: "retrying"},
				{Number: 4, OccurredAt: testNow, Message: "retrying"},
				{Number: 5, OccurredAt: testNow, Message: "retrying"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := attemptsFrom(tt.events)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("attemptsFrom =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestVerdictClass(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"version":       "badge bad",
		"configuration": "badge warn",
		"transient":     "badge active",
		"Transient":     "badge active",
		"unknown":       "badge muted",
		"":              "badge muted",
		"<script>":      "badge muted",
	}
	for verdict, want := range tests {
		if got := verdictClass(verdict); got != want {
			t.Errorf("verdictClass(%q) = %q, want %q", verdict, got, want)
		}
	}
}

func TestAttemptsSectionHasNoDisclosures(t *testing.T) {
	t.Parallel()

	html := body(t, get(t, "/deployments/10", viewer))
	start, end := strings.Index(html, "<h2>Attempts</h2>"), strings.Index(html, "<h2>Events</h2>")
	if start < 0 || end < start {
		t.Fatalf("no attempts section:\n%s", html)
	}
	if strings.Contains(html[start:end], "<details") {
		t.Errorf("attempts section contains a disclosure, which would pause the live refresh:\n%s", html[start:end])
	}
}
