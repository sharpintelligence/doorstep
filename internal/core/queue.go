package core

import (
	"errors"
	"strconv"
	"time"
)

type Priority int

const (
	PriorityNormal   Priority = 0
	PriorityUrgent   Priority = 100
	PriorityRollback Priority = 200
)

var ErrLeaseLost = errors.New("queue lease lost")

func (p Priority) String() string {
	switch p {
	case PriorityNormal:
		return "normal"
	case PriorityUrgent:
		return "urgent"
	case PriorityRollback:
		return "rollback"
	default:
		return strconv.Itoa(int(p))
	}
}

type QueueEntry struct {
	Position       int
	Deployment     Deployment
	Priority       Priority
	EnqueuedAt     time.Time
	AvailableAt    time.Time
	LeaseExpiresAt *time.Time
	Attempts       int
}

func (e QueueEntry) InFlight() bool {
	return e.Deployment.State.Active() || e.LeaseExpiresAt != nil
}
