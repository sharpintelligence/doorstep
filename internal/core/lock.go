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

const GlobalScope = "global"

type Lock struct {
	Target       Target
	Global       bool
	Kind         LockKind
	Owner        string
	Reason       string
	DeploymentID *DeploymentID
	CreatedAt    time.Time
	ExpiresAt    *time.Time
	RemindedAt   *time.Time
}

func (l Lock) Scope() string {
	if l.Global {
		return GlobalScope
	}
	return l.Target.App
}
