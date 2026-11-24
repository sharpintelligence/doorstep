package core

import (
	"errors"
	"fmt"
	"slices"
)

type State string

const (
	StateQueued     State = "queued"
	StateResolving  State = "resolving"
	StatePinning    State = "pinning"
	StateSyncing    State = "syncing"
	StateVerifying  State = "verifying"
	StateSucceeded  State = "succeeded"
	StateFailed     State = "failed"
	StateCancelled  State = "cancelled"
	StateSuperseded State = "superseded"
)

var (
	ErrUnknownState      = errors.New("unknown deployment state")
	ErrInvalidTransition = errors.New("invalid state transition")
)

func States() []State {
	return []State{
		StateQueued,
		StateResolving,
		StatePinning,
		StateSyncing,
		StateVerifying,
		StateSucceeded,
		StateFailed,
		StateCancelled,
		StateSuperseded,
	}
}

func ParseState(s string) (State, error) {
	state := State(s)
	if !state.Valid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownState, s)
	}
	return state, nil
}

func (s State) Valid() bool {
	return slices.Contains(States(), s)
}

func (s State) Terminal() bool {
	return slices.Contains(TerminalStates(), s)
}

func (s State) Claimable() bool {
	return slices.Contains(ClaimableStates(), s)
}

func (s State) Active() bool {
	return slices.Contains(ActiveStates(), s)
}

func ActiveStates() []State {
	return []State{StateResolving, StatePinning, StateSyncing, StateVerifying}
}

func ClaimableStates() []State {
	return append([]State{StateQueued}, ActiveStates()...)
}

func (s State) CanTransitionTo(to State) bool {
	return slices.Contains(transitions()[s], to)
}

func (s State) Transition(to State) error {
	if !s.CanTransitionTo(to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, s, to)
	}
	return nil
}

func transitions() map[State][]State {
	return map[State][]State{
		StateQueued:    {StateResolving, StateFailed, StateCancelled, StateSuperseded},
		StateResolving: {StatePinning, StateFailed, StateCancelled, StateSuperseded},
		StatePinning:   {StateSyncing, StateFailed, StateCancelled},
		StateSyncing:   {StateVerifying, StateFailed, StateCancelled},
		StateVerifying: {StateSucceeded, StateFailed, StateCancelled},
	}
}
