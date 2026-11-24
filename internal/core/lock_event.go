package core

import (
	"slices"
	"time"
)

type LockAction string

const (
	LockActionLocked   LockAction = "locked"
	LockActionUnlocked LockAction = "unlocked"
	LockActionExpired  LockAction = "expired"
	LockActionReminded LockAction = "reminded"
)

func LockActions() []LockAction {
	return []LockAction{LockActionLocked, LockActionUnlocked, LockActionExpired, LockActionReminded}
}

func (a LockAction) Valid() bool {
	return slices.Contains(LockActions(), a)
}

type LockEvent struct {
	Action     LockAction
	Lock       Lock
	Actor      string
	OccurredAt time.Time
}
