package web

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

type findingData struct {
	Signal   string `json:"signal"`
	Node     string `json:"node"`
	Pods     int    `json:"pods"`
	Reason   string `json:"reason"`
	ExitCode *int   `json:"exit_code"`
	Detail   string `json:"detail"`
}

type diagnosisData struct {
	Verdict       string        `json:"verdict"`
	Summary       string        `json:"summary"`
	Attempt       int           `json:"attempt"`
	Findings      []findingData `json:"findings"`
	Nodes         []string      `json:"nodes"`
	Evidence      []string      `json:"evidence"`
	PodsInspected *bool         `json:"pods_inspected"`
}

type retryData struct {
	Attempt     int    `json:"attempt"`
	NextAttempt int    `json:"next_attempt"`
	MaxAttempts int    `json:"max_attempts"`
	DelayMS     int64  `json:"delay_ms"`
	Reason      string `json:"reason"`
	Summary     string `json:"summary"`
}

type attemptEventData struct {
	FailureClass     string         `json:"failure_class"`
	SyncReason       string         `json:"sync_reason"`
	Retry            *retryData     `json:"retry"`
	Attempts         int            `json:"attempts"`
	Decision         string         `json:"decision"`
	RollbackEligible bool           `json:"rollback_eligible"`
	Diagnosis        *diagnosisData `json:"diagnosis"`
}

type findingView findingData

type attemptView struct {
	Number           int
	MaxAttempts      int
	OccurredAt       time.Time
	Verdict          string
	Summary          string
	SyncReason       string
	Decision         string
	NextAttempt      int
	Delay            time.Duration
	Final            bool
	RollbackEligible bool
	Findings         []findingView
	Nodes            []string
	Evidence         []string
	PodsNotInspected bool
	HasDiagnosis     bool
	Message          string
}

func isRetryEvent(e core.Event) bool {
	return e.Type == core.EventTransition && e.From == core.StateSyncing && e.To == core.StateSyncing
}

func isFailedEvent(e core.Event) bool {
	return e.Type == core.EventTransition && e.To == core.StateFailed
}

func decodeAttemptData(raw json.RawMessage) (attemptEventData, bool) {
	var data attemptEventData
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return data, false
	}
	if err := json.Unmarshal(trimmed, &data); err != nil {
		return attemptEventData{}, false
	}
	return data, true
}

func attemptsFrom(events []core.Event) []attemptView {
	var attempts []attemptView
	maxAttempts := 0
	for _, e := range events {
		switch {
		case isRetryEvent(e):
			attempt := attemptView{Number: len(attempts) + 1, OccurredAt: e.OccurredAt, Message: e.Message}
			if data, ok := decodeAttemptData(e.Data); ok {
				attempt = retryAttempt(attempt, data)
				maxAttempts = max(maxAttempts, attempt.MaxAttempts)
			}
			attempts = append(attempts, attempt)
		case isFailedEvent(e):
			data, ok := decodeAttemptData(e.Data)
			if !ok || data.Diagnosis == nil {
				continue
			}
			attempts = append(attempts, finalAttempt(e, data, maxAttempts, len(attempts)+1))
		}
	}
	slices.SortStableFunc(attempts, func(a, b attemptView) int { return a.Number - b.Number })
	return attempts
}

func retryAttempt(attempt attemptView, data attemptEventData) attemptView {
	if data.Diagnosis != nil && data.Diagnosis.Attempt > 0 {
		attempt.Number = data.Diagnosis.Attempt
	}
	if data.Retry != nil {
		if data.Retry.Attempt > 0 {
			attempt.Number = data.Retry.Attempt
		}
		attempt.MaxAttempts = data.Retry.MaxAttempts
		attempt.NextAttempt = data.Retry.NextAttempt
		attempt.Delay = time.Duration(data.Retry.DelayMS) * time.Millisecond
		attempt.Summary = data.Retry.Summary
	}
	if data.FailureClass != "" {
		attempt.Verdict = data.FailureClass
	}
	attempt.SyncReason = data.SyncReason
	return withDiagnosis(attempt, data.Diagnosis)
}

func finalAttempt(e core.Event, data attemptEventData, maxAttempts, ordinal int) attemptView {
	attempt := attemptView{
		Number:           ordinal,
		MaxAttempts:      maxAttempts,
		OccurredAt:       e.OccurredAt,
		SyncReason:       data.SyncReason,
		Decision:         data.Decision,
		Final:            true,
		RollbackEligible: data.RollbackEligible,
		Message:          e.Message,
	}
	if data.Attempts > 0 {
		attempt.Number = data.Attempts
	}
	return withDiagnosis(attempt, data.Diagnosis)
}

func withDiagnosis(attempt attemptView, d *diagnosisData) attemptView {
	if d == nil {
		return attempt
	}
	attempt.HasDiagnosis = true
	if d.Verdict != "" {
		attempt.Verdict = d.Verdict
	}
	if d.Summary != "" {
		attempt.Summary = d.Summary
	}
	attempt.Nodes = d.Nodes
	attempt.Evidence = d.Evidence
	attempt.PodsNotInspected = d.PodsInspected != nil && !*d.PodsInspected
	for _, f := range d.Findings {
		attempt.Findings = append(attempt.Findings, findingView(f))
	}
	return attempt
}

func verdictClass(verdict string) string {
	switch strings.ToLower(verdict) {
	case "version":
		return "badge bad"
	case "configuration":
		return "badge warn"
	case "transient":
		return "badge active"
	default:
		return "badge muted"
	}
}
