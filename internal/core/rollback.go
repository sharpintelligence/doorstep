package core

import (
	"errors"
	"strconv"
)

var (
	ErrNoRollbackTarget   = errors.New("no deployment to roll back to")
	ErrRollbackInProgress = errors.New("a rollback is already in progress")
	ErrInvalidRollback    = errors.New("invalid rollback")
)

type RollbackInProgressError struct {
	Deployment Deployment
}

func (e *RollbackInProgressError) Error() string {
	return "rollback " + strconv.FormatInt(int64(e.Deployment.ID), 10) + " of " + e.Deployment.Target.String() +
		" is already " + string(e.Deployment.State)
}

func (e *RollbackInProgressError) Is(target error) bool {
	return target == ErrRollbackInProgress
}
