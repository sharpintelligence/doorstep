package metrics

const unknownLabel = "unknown"

type Outcome string

const (
	OutcomeSucceeded      Outcome = "succeeded"
	OutcomeFailed         Outcome = "failed"
	OutcomeRolledBack     Outcome = "rolled_back"
	OutcomeRollbackFailed Outcome = "rollback_failed"
	OutcomeCancelled      Outcome = "cancelled"
	OutcomeSuperseded     Outcome = "superseded"
)

func (o Outcome) label() string {
	switch o {
	case OutcomeSucceeded, OutcomeFailed, OutcomeRolledBack, OutcomeRollbackFailed, OutcomeCancelled, OutcomeSuperseded:
		return string(o)
	default:
		return unknownLabel
	}
}

type RollbackReason string

const (
	RollbackReasonGateFailed RollbackReason = "gate_failed"
	RollbackReasonSyncFailed RollbackReason = "sync_failed"
	RollbackReasonManual     RollbackReason = "manual"
)

func (r RollbackReason) label() string {
	switch r {
	case RollbackReasonGateFailed, RollbackReasonSyncFailed, RollbackReasonManual:
		return string(r)
	default:
		return unknownLabel
	}
}

type Phase string

const (
	PhaseDeploy   Phase = "deploy"
	PhaseRollback Phase = "rollback"
)

func (p Phase) label() string {
	switch p {
	case PhaseDeploy, PhaseRollback:
		return string(p)
	default:
		return unknownLabel
	}
}

type GateResult string

const (
	GateResultPassed GateResult = "passed"
	GateResultFailed GateResult = "failed"
	GateResultError  GateResult = "error"
)

func (r GateResult) label() string {
	switch r {
	case GateResultPassed, GateResultFailed, GateResultError:
		return string(r)
	default:
		return unknownLabel
	}
}
