package store_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func TestClaimPoll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		polledAgo string
		every     time.Duration
		want      bool
	}{
		{name: "never polled", every: time.Minute, want: true},
		{name: "due", polledAgo: "61 seconds", every: time.Minute, want: true},
		{name: "exactly due", polledAgo: "10 minutes", every: 10 * time.Minute, want: true},
		{name: "not due", polledAgo: "30 seconds", every: time.Minute},
		{name: "just polled", polledAgo: "0 seconds", every: 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			if tt.polledAgo != "" {
				mustExec(t, st, "UPDATE intake_poll_state SET polled_at = date_trunc('milliseconds', now()) - $1::interval", tt.polledAgo)
			}
			got, err := st.ClaimPoll(t.Context(), tt.every)
			if err != nil {
				t.Fatalf("ClaimPoll: %v", err)
			}
			if got != tt.want {
				t.Errorf("ClaimPoll = %t, want %t", got, tt.want)
			}
			if again, err := st.ClaimPoll(t.Context(), tt.every); err != nil || again {
				t.Errorf("second ClaimPoll = %t, %v, want false", again, err)
			}
		})
	}
}

func TestClaimPollConcurrentClaimsHaveOneWinner(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	const replicas = 8
	var (
		wg      sync.WaitGroup
		winners atomic.Int32
		start   = make(chan struct{})
	)
	for range replicas {
		wg.Go(func() {
			<-start
			claimed, err := st.ClaimPoll(t.Context(), time.Minute)
			if err != nil {
				t.Errorf("ClaimPoll: %v", err)
			}
			if claimed {
				winners.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Errorf("winners = %d, want 1", got)
	}
}

func TestClaimPollRejectsNonPositiveInterval(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	for _, every := range []time.Duration{0, -time.Second} {
		if claimed, err := st.ClaimPoll(t.Context(), every); err == nil || claimed {
			t.Errorf("ClaimPoll(%s) = %t, %v, want an error", every, claimed, err)
		}
	}
}
