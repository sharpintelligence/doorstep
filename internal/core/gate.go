package core

import (
	"encoding/json"
	"slices"
	"time"
)

type GateStatus string

const (
	GatePending GateStatus = "pending"
	GatePassed  GateStatus = "passed"
	GateFailed  GateStatus = "failed"
	GateSkipped GateStatus = "skipped"
)

func GateStatuses() []GateStatus {
	return []GateStatus{GatePending, GatePassed, GateFailed, GateSkipped}
}

func (s GateStatus) Valid() bool {
	return slices.Contains(GateStatuses(), s)
}

type GateResult struct {
	Name       string
	Status     GateStatus
	Message    string
	Evidence   json.RawMessage
	StartedAt  time.Time
	FinishedAt *time.Time
}
