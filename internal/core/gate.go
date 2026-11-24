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

func (s GateStatus) Terminal() bool {
	return s == GatePassed || s == GateFailed || s == GateSkipped
}

type GateResult struct {
	DeploymentID DeploymentID
	Name         string
	Type         string
	Phase        GatePhase
	Status       GateStatus
	Message      string
	Evidence     json.RawMessage
	StartedAt    time.Time
	FinishedAt   *time.Time
	UpdatedAt    time.Time
}
