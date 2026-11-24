package core

import (
	"encoding/json"
	"slices"
	"time"
)

type EventType string

const (
	EventTransition EventType = "transition"
	EventGate       EventType = "gate"
	EventLock       EventType = "lock"
	EventNote       EventType = "note"
)

func EventTypes() []EventType {
	return []EventType{EventTransition, EventGate, EventLock, EventNote}
}

func (t EventType) Valid() bool {
	return slices.Contains(EventTypes(), t)
}

type Event struct {
	ID           int64
	DeploymentID DeploymentID
	Target       Target
	Type         EventType
	From         State
	To           State
	Actor        string
	Message      string
	Data         json.RawMessage
	OccurredAt   time.Time
}
