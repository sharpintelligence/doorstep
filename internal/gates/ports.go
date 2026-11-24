package gates

import (
	"context"
	"errors"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	SyncSynced    = "Synced"
	SyncOutOfSync = "OutOfSync"
	SyncUnknown   = "Unknown"

	HealthHealthy     = "Healthy"
	HealthProgressing = "Progressing"
	HealthDegraded    = "Degraded"
	HealthSuspended   = "Suspended"
	HealthMissing     = "Missing"
	HealthUnknown     = "Unknown"

	OperationRunning     = "Running"
	OperationTerminating = "Terminating"
	OperationFailed      = "Failed"
	OperationError       = "Error"
	OperationSucceeded   = "Succeeded"
)

var ErrApplicationNotFound = errors.New("argo cd application not found")

type TrackedChecks interface {
	TrackedCommits(ctx context.Context, repository, sha string) ([]core.TrackedCommit, error)
}

type LiveChecks interface {
	Checks(ctx context.Context, repository, sha string, required []string) (core.ChecksEvaluation, error)
}

type FreezeReader interface {
	Blocking(ctx context.Context, t core.Target) ([]core.Lock, error)
}

type ExternalReports interface {
	GetGateReport(ctx context.Context, id core.DeploymentID, gate string) (core.GateReport, error)
}

type ApplicationReader interface {
	Application(ctx context.Context, namespace, name string) (Application, error)
}

type Application struct {
	SyncStatus     string
	Revision       string
	Health         string
	HealthMessage  string
	OperationPhase string
}
