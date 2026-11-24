package core

import (
	"slices"
	"time"
)

type LockKind string

const (
	LockDeployment LockKind = "deployment"
	LockFreeze     LockKind = "freeze"
)

func LockKinds() []LockKind {
	return []LockKind{LockDeployment, LockFreeze}
}

func (k LockKind) Valid() bool {
	return slices.Contains(LockKinds(), k)
}

type Lock struct {
	Target       Target
	Kind         LockKind
	Owner        string
	Reason       string
	DeploymentID *DeploymentID
	CreatedAt    time.Time
	ExpiresAt    *time.Time
}
