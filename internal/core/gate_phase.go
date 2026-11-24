package core

import "slices"

type GatePhase string

const (
	GatePhaseBefore GatePhase = "before"
	GatePhaseAfter  GatePhase = "after"
)

func GatePhases() []GatePhase {
	return []GatePhase{GatePhaseBefore, GatePhaseAfter}
}

func (p GatePhase) Valid() bool {
	return slices.Contains(GatePhases(), p)
}

type GateOutcome struct {
	Phase   GatePhase
	Status  GateStatus
	Results []GateResult
}

func AggregateGateStatus(results []GateResult) GateStatus {
	status := GatePassed
	for _, r := range results {
		if r.Status == GateFailed {
			return GateFailed
		}
		if r.Status != GatePassed && r.Status != GateSkipped {
			status = GatePending
		}
	}
	return status
}
