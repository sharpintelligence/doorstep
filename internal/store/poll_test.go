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
		name string
		next string
		want bool
	}{
		{name: "never polled", want: true},
		{name: "due", next: "now() - interval '1 second'", want: true},
		{name: "exactly due", next: "now()", want: true},
		{name: "not due", next: "now() + interval '30 seconds'"},
		{name: "leased by another replica", next: "now() + interval '2 minutes'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			if tt.next != "" {
				mustExec(t, st, "UPDATE intake_poll_state SET next_poll_at = "+tt.next)
			}
			before := time.Now()
			leasedUntil, got, err := st.ClaimPoll(t.Context(), time.Minute)
			if err != nil {
				t.Fatalf("ClaimPoll: %v", err)
			}
			if got != tt.want {
				t.Fatalf("ClaimPoll = %t, want %t", got, tt.want)
			}
			if got && leasedUntil.Before(before.Add(time.Minute-5*time.Second)) {
				t.Errorf("leased until %s, want about a minute from %s", leasedUntil, before)
			}
			if !got && !leasedUntil.IsZero() {
				t.Errorf("unclaimed poll returned lease %s", leasedUntil)
			}
			if _, again, err := st.ClaimPoll(t.Context(), time.Minute); err != nil || again {
				t.Errorf("second ClaimPoll = %t, %v, want false", again, err)
			}
		})
	}
}

func TestFinishPoll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		takenOver    bool
		wantFinished bool
		wantNext     string
	}{
		{name: "holder schedules the next cycle an interval after it finished", wantFinished: true, wantNext: "now() + interval '1 minute'"},
		{name: "lease taken over by another replica is left alone", takenOver: true, wantNext: "now() + interval '10 minutes'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			leasedUntil, claimed, err := st.ClaimPoll(t.Context(), time.Hour)
			if err != nil || !claimed {
				t.Fatalf("ClaimPoll = %t, %v", claimed, err)
			}
			if tt.takenOver {
				mustExec(t, st, "UPDATE intake_poll_state SET next_poll_at = now() + interval '10 minutes'")
			}
			finished, err := st.FinishPoll(t.Context(), leasedUntil, time.Minute)
			if err != nil {
				t.Fatalf("FinishPoll: %v", err)
			}
			if finished != tt.wantFinished {
				t.Errorf("FinishPoll = %t, want %t", finished, tt.wantFinished)
			}
			if n := queryInt(t, st, "SELECT count(*) FROM intake_poll_state WHERE abs(extract(epoch FROM next_poll_at - ("+tt.wantNext+"))) < 5"); n != 1 {
				t.Errorf("next poll is not near %s", tt.wantNext)
			}
			if _, again, err := st.ClaimPoll(t.Context(), time.Hour); err != nil || again {
				t.Errorf("ClaimPoll right after the cycle = %t, %v, want false", again, err)
			}
		})
	}
}

func TestClaimPollAfterAnExpiredLease(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	stale, claimed, err := st.ClaimPoll(t.Context(), time.Hour)
	if err != nil || !claimed {
		t.Fatalf("ClaimPoll = %t, %v", claimed, err)
	}
	mustExec(t, st, "UPDATE intake_poll_state SET next_poll_at = now() - interval '1 second'")
	if _, claimed, err := st.ClaimPoll(t.Context(), time.Hour); err != nil || !claimed {
		t.Fatalf("ClaimPoll after the lease expired = %t, %v, want claimed", claimed, err)
	}
	if finished, err := st.FinishPoll(t.Context(), stale, time.Minute); err != nil || finished {
		t.Errorf("FinishPoll with the expired lease = %t, %v, want false", finished, err)
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
			_, claimed, err := st.ClaimPoll(t.Context(), time.Minute)
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

func TestClaimPollRejectsNonPositiveDurations(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	for _, d := range []time.Duration{0, -time.Second} {
		if _, claimed, err := st.ClaimPoll(t.Context(), d); err == nil || claimed {
			t.Errorf("ClaimPoll(%s) = %t, %v, want an error", d, claimed, err)
		}
		if finished, err := st.FinishPoll(t.Context(), time.Now(), d); err == nil || finished {
			t.Errorf("FinishPoll(%s) = %t, %v, want an error", d, finished, err)
		}
	}
}
