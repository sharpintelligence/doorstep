package core

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

type ChecksState string

const (
	ChecksPending ChecksState = "pending"
	ChecksSuccess ChecksState = "success"
	ChecksFailure ChecksState = "failure"
)

func ChecksStates() []ChecksState {
	return []ChecksState{ChecksPending, ChecksSuccess, ChecksFailure}
}

func (s ChecksState) Valid() bool {
	return slices.Contains(ChecksStates(), s)
}

type CheckKind string

const (
	CheckKindRun    CheckKind = "check_run"
	CheckKindStatus CheckKind = "status"
	CheckKindSuite  CheckKind = "check_suite"
)

func CheckKinds() []CheckKind {
	return []CheckKind{CheckKindRun, CheckKindStatus, CheckKindSuite}
}

func (k CheckKind) Valid() bool {
	return slices.Contains(CheckKinds(), k)
}

type Check struct {
	Name   string      `json:"name"`
	Kind   CheckKind   `json:"kind,omitempty"`
	State  ChecksState `json:"state"`
	Detail string      `json:"detail,omitempty"`
}

type TrackedCommitID int64

type TrackedCommit struct {
	ID             TrackedCommitID
	Target         Target
	Commit         Commit
	Branch         string
	Source         Source
	PushedBy       string
	ChecksState    ChecksState
	Checks         []Check
	DeploymentID   *DeploymentID
	SupersededByID *TrackedCommitID
	CreatedAt      time.Time
	UpdatedAt      time.Time
	EvaluatedAt    *time.Time
	EnqueuedAt     *time.Time
}

func (c TrackedCommit) Validate() error {
	errs := []error{c.Target.Validate(), c.Commit.Validate()}
	if strings.TrimSpace(c.Branch) == "" {
		errs = append(errs, errors.New("tracked commit branch must not be empty"))
	}
	if !c.Source.Valid() {
		errs = append(errs, fmt.Errorf("unknown tracked commit source %q", c.Source))
	}
	if !c.ChecksState.Valid() {
		errs = append(errs, fmt.Errorf("unknown checks state %q", c.ChecksState))
	}
	return errors.Join(errs...)
}

func (c TrackedCommit) Open() bool {
	return c.EnqueuedAt == nil && c.SupersededByID == nil
}

type ChecksEvaluation struct {
	State  ChecksState
	Checks []Check
	ReadAt time.Time
}

func (e ChecksEvaluation) Validate() error {
	var errs []error
	if !e.State.Valid() {
		errs = append(errs, fmt.Errorf("unknown checks state %q", e.State))
	}
	if e.ReadAt.IsZero() {
		errs = append(errs, errors.New("checks evaluation needs the time the checks were read"))
	}
	return errors.Join(errs...)
}

type TrackOutcome string

const (
	TrackCreated  TrackOutcome = "created"
	TrackReopened TrackOutcome = "reopened"
	TrackExisting TrackOutcome = "existing"
	TrackStale    TrackOutcome = "stale"
)

type FollowedBranch struct {
	Target     Target
	Repository string
	Branch     string
}
