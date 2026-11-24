package core

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

type edge struct{ from, to State }

func TestStateTransitionMatrix(t *testing.T) {
	t.Parallel()

	allowed := map[edge]bool{
		{StateQueued, StateResolving}:     true,
		{StateQueued, StateFailed}:        true,
		{StateQueued, StateCancelled}:     true,
		{StateQueued, StateSuperseded}:    true,
		{StateResolving, StatePinning}:    true,
		{StateResolving, StateFailed}:     true,
		{StateResolving, StateCancelled}:  true,
		{StateResolving, StateSuperseded}: true,
		{StatePinning, StateSyncing}:      true,
		{StatePinning, StateFailed}:       true,
		{StatePinning, StateCancelled}:    true,
		{StateSyncing, StateVerifying}:    true,
		{StateSyncing, StateFailed}:       true,
		{StateSyncing, StateCancelled}:    true,
		{StateVerifying, StateSucceeded}:  true,
		{StateVerifying, StateFailed}:     true,
		{StateVerifying, StateCancelled}:  true,
	}

	for _, from := range States() {
		for _, to := range States() {
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				t.Parallel()
				want := allowed[edge{from, to}]
				if got := from.CanTransitionTo(to); got != want {
					t.Errorf("%s.CanTransitionTo(%s) = %v, want %v", from, to, got, want)
				}
				err := from.Transition(to)
				if want && err != nil {
					t.Errorf("%s.Transition(%s) = %v, want nil", from, to, err)
				}
				if !want && !errors.Is(err, ErrInvalidTransition) {
					t.Errorf("%s.Transition(%s) = %v, want ErrInvalidTransition", from, to, err)
				}
			})
		}
	}
}

func TestStateTransitionsRejectUnknownStates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		from, to State
	}{
		{name: "unknown source", from: "paused", to: StateResolving},
		{name: "unknown destination", from: StateQueued, to: "paused"},
		{name: "empty source", from: "", to: StateQueued},
		{name: "empty destination", from: StateQueued, to: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.from.CanTransitionTo(tt.to) {
				t.Errorf("%q.CanTransitionTo(%q) = true", tt.from, tt.to)
			}
		})
	}
}

func TestStateClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state     State
		terminal  bool
		active    bool
		claimable bool
	}{
		{state: StateQueued, claimable: true},
		{state: StateResolving, active: true, claimable: true},
		{state: StatePinning, active: true, claimable: true},
		{state: StateSyncing, active: true, claimable: true},
		{state: StateVerifying, active: true, claimable: true},
		{state: StateSucceeded, terminal: true},
		{state: StateFailed, terminal: true},
		{state: StateCancelled, terminal: true},
		{state: StateSuperseded, terminal: true},
	}

	var covered []State
	for _, tt := range tests {
		covered = append(covered, tt.state)
		t.Run(string(tt.state), func(t *testing.T) {
			t.Parallel()
			if got := tt.state.Terminal(); got != tt.terminal {
				t.Errorf("Terminal() = %v, want %v", got, tt.terminal)
			}
			if got := tt.state.Active(); got != tt.active {
				t.Errorf("Active() = %v, want %v", got, tt.active)
			}
			if got := tt.state.Claimable(); got != tt.claimable {
				t.Errorf("Claimable() = %v, want %v", got, tt.claimable)
			}
			if !tt.state.Valid() {
				t.Errorf("Valid() = false")
			}
		})
	}
	if !slices.Equal(covered, States()) {
		t.Errorf("test table covers %v, States() = %v", covered, States())
	}
}

func TestTerminalStatesHaveNoOutgoingTransitions(t *testing.T) {
	t.Parallel()

	for _, from := range States() {
		if !from.Terminal() {
			continue
		}
		for _, to := range States() {
			if from.CanTransitionTo(to) {
				t.Errorf("terminal state %s may move to %s", from, to)
			}
		}
	}
}

func TestNonTerminalStatesCanBeCancelled(t *testing.T) {
	t.Parallel()

	for _, s := range States() {
		if s.Terminal() != s.CanTransitionTo(StateCancelled) {
			continue
		}
		t.Errorf("%s: Terminal() = %v but CanTransitionTo(cancelled) = %v", s, s.Terminal(), s.CanTransitionTo(StateCancelled))
	}
}

func TestParseState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    State
		wantErr bool
	}{
		{in: "queued", want: StateQueued},
		{in: "verifying", want: StateVerifying},
		{in: "failed", want: StateFailed},
		{in: "rolling_back", wantErr: true},
		{in: "rolled_back", wantErr: true},
		{in: "superseded", want: StateSuperseded},
		{in: "", wantErr: true},
		{in: "Queued", wantErr: true},
		{in: "Failed", wantErr: true},
		{in: " queued", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseState(tt.in)
			if tt.wantErr {
				if !errors.Is(err, ErrUnknownState) {
					t.Fatalf("ParseState(%q) error = %v, want ErrUnknownState", tt.in, err)
				}
				if !strings.Contains(err.Error(), "\""+tt.in+"\"") {
					t.Errorf("error %q does not quote the input", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("ParseState(%q) = %q, %v, want %q", tt.in, got, err, tt.want)
			}
		})
	}
}

func TestStatesRoundTrip(t *testing.T) {
	t.Parallel()

	seen := map[State]bool{}
	for _, s := range States() {
		if seen[s] {
			t.Errorf("duplicate state %s", s)
		}
		seen[s] = true
		got, err := ParseState(string(s))
		if err != nil || got != s {
			t.Errorf("ParseState(%q) = %q, %v", s, got, err)
		}
	}
	if len(ActiveStates()) != 4 {
		t.Errorf("ActiveStates() = %v, want 4 states", ActiveStates())
	}
}
