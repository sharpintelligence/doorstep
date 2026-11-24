package core

import (
	"errors"
	"time"
)

var (
	ErrSyncTimeout        = errors.New("sync timed out")
	ErrSyncFailed         = errors.New("sync failed")
	ErrSyncTargetNotFound = errors.New("sync target not found")
	ErrSyncUnavailable    = errors.New("sync status unavailable")
)

type HealthStatus string

const (
	HealthHealthy     HealthStatus = "healthy"
	HealthProgressing HealthStatus = "progressing"
	HealthDegraded    HealthStatus = "degraded"
	HealthSuspended   HealthStatus = "suspended"
	HealthMissing     HealthStatus = "missing"
	HealthUnknown     HealthStatus = "unknown"
)

func (h HealthStatus) Valid() bool {
	switch h {
	case HealthHealthy, HealthProgressing, HealthDegraded, HealthSuspended, HealthMissing, HealthUnknown:
		return true
	default:
		return false
	}
}

type SyncState string

const (
	SyncSynced    SyncState = "synced"
	SyncOutOfSync SyncState = "out_of_sync"
	SyncUnknown   SyncState = "unknown"
)

func (s SyncState) Valid() bool {
	switch s {
	case SyncSynced, SyncOutOfSync, SyncUnknown:
		return true
	default:
		return false
	}
}

const (
	OperationRunning     = "Running"
	OperationTerminating = "Terminating"
	OperationSucceeded   = "Succeeded"
	OperationFailed      = "Failed"
	OperationError       = "Error"
)

type ResourceStatus struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
	Health    HealthStatus
	Sync      string
	Message   string
}

type Health struct {
	Status     HealthStatus
	Message    string
	Resources  []ResourceStatus
	ObservedAt time.Time
}

type SyncStatus struct {
	State            SyncState
	Revision         string
	Revisions        []string
	Reached          bool
	OperationPhase   string
	OperationMessage string
	Health           Health
	Failure          *SyncFailure
}

func (s SyncStatus) OperationInProgress() bool {
	return s.OperationPhase == OperationRunning || s.OperationPhase == OperationTerminating
}

func (s SyncStatus) Ready() bool {
	return s.Reached && s.State == SyncSynced && s.Health.Status == HealthHealthy && !s.OperationInProgress()
}

type SyncFailureReason string

const (
	SyncReasonNotObserved SyncFailureReason = "not_observed"
	SyncReasonOutOfSync   SyncFailureReason = "out_of_sync"
	SyncReasonRunning     SyncFailureReason = "sync_running"
	SyncReasonError       SyncFailureReason = "sync_error"
	SyncReasonFailed      SyncFailureReason = "sync_failed"
	SyncReasonDegraded    SyncFailureReason = "degraded"
	SyncReasonProgressing SyncFailureReason = "progressing"
	SyncReasonSuspended   SyncFailureReason = "suspended"
	SyncReasonMissing     SyncFailureReason = "missing"
	SyncReasonNotFound    SyncFailureReason = "not_found"
	SyncReasonForbidden   SyncFailureReason = "forbidden"
	SyncReasonUnavailable SyncFailureReason = "unavailable"
)

type FailureClass string

const (
	FailureTransient     FailureClass = "transient"
	FailureVersion       FailureClass = "version"
	FailureConfiguration FailureClass = "configuration"
	FailureUnknown       FailureClass = "unknown"
)

func (r SyncFailureReason) Class(automatedSync bool) FailureClass {
	switch r {
	case SyncReasonNotObserved, SyncReasonError, SyncReasonUnavailable:
		return FailureTransient
	case SyncReasonFailed, SyncReasonDegraded:
		return FailureVersion
	case SyncReasonNotFound, SyncReasonForbidden:
		return FailureConfiguration
	case SyncReasonOutOfSync:
		if automatedSync {
			return FailureUnknown
		}
		return FailureConfiguration
	case SyncReasonRunning, SyncReasonProgressing, SyncReasonSuspended, SyncReasonMissing:
		return FailureUnknown
	default:
		return FailureUnknown
	}
}

type SyncFailure struct {
	Reason    SyncFailureReason
	Class     FailureClass
	Message   string
	Resources []ResourceStatus
}
