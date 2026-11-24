package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/github"
)

type scripted struct {
	errs  []error
	calls int
	ctxs  []context.Context
}

func (s *scripted) delivery(ctx context.Context) error {
	s.calls++
	s.ctxs = append(s.ctxs, ctx)
	if len(s.errs) == 0 {
		return nil
	}
	err := s.errs[0]
	s.errs = s.errs[1:]
	return err
}

func TestPolicyDo(t *testing.T) {
	t.Parallel()

	netErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	tests := []struct {
		name       string
		errs       []error
		random     float64
		wantCalls  int
		wantErr    bool
		wantSleeps []time.Duration
	}{
		{name: "first attempt succeeds", wantCalls: 1},
		{
			name:       "network error then success",
			errs:       []error{netErr},
			random:     0.5,
			wantCalls:  2,
			wantSleeps: []time.Duration{time.Second},
		},
		{
			name:       "500 then 200",
			errs:       []error{&StatusError{StatusCode: http.StatusInternalServerError}},
			random:     0.5,
			wantCalls:  2,
			wantSleeps: []time.Duration{time.Second},
		},
		{
			name:       "429 with retry after",
			errs:       []error{&StatusError{StatusCode: http.StatusTooManyRequests, RetryAfter: 7 * time.Second}},
			random:     0.5,
			wantCalls:  2,
			wantSleeps: []time.Duration{7 * time.Second},
		},
		{
			name:       "retry after is capped",
			errs:       []error{&StatusError{StatusCode: http.StatusServiceUnavailable, RetryAfter: time.Hour}},
			random:     0.5,
			wantCalls:  2,
			wantSleeps: []time.Duration{30 * time.Second},
		},
		{
			name:       "408 is retried",
			errs:       []error{&StatusError{StatusCode: http.StatusRequestTimeout}},
			random:     0.5,
			wantCalls:  2,
			wantSleeps: []time.Duration{time.Second},
		},
		{
			name: "gives up after four attempts with exponential backoff",
			errs: []error{
				&StatusError{StatusCode: http.StatusBadGateway},
				&StatusError{StatusCode: http.StatusBadGateway},
				&StatusError{StatusCode: http.StatusBadGateway},
				&StatusError{StatusCode: http.StatusBadGateway},
			},
			random:     0.5,
			wantCalls:  4,
			wantErr:    true,
			wantSleeps: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
		},
		{
			name:       "jitter low",
			errs:       []error{netErr, netErr},
			random:     0,
			wantCalls:  3,
			wantSleeps: []time.Duration{800 * time.Millisecond, 1600 * time.Millisecond},
		},
		{
			name:       "jitter high",
			errs:       []error{netErr},
			random:     1,
			wantCalls:  2,
			wantSleeps: []time.Duration{1200 * time.Millisecond},
		},
		{name: "400 is permanent", errs: []error{&StatusError{StatusCode: http.StatusBadRequest}}, wantCalls: 1, wantErr: true},
		{name: "404 is permanent", errs: []error{&StatusError{StatusCode: http.StatusNotFound}}, wantCalls: 1, wantErr: true},
		{name: "302 is permanent", errs: []error{&StatusError{StatusCode: http.StatusFound}}, wantCalls: 1, wantErr: true},
		{name: "explicitly permanent", errs: []error{permanent(errors.New("bad config"))}, wantCalls: 1, wantErr: true},
		{name: "cancelled", errs: []error{context.Canceled}, wantCalls: 1, wantErr: true},
		{
			name:       "secret read error is retried",
			errs:       []error{fmt.Errorf("read webhook secret: %w", errors.New("file missing"))},
			random:     0.5,
			wantCalls:  2,
			wantSleeps: []time.Duration{time.Second},
		},
		{
			name:       "github 502 is retried",
			errs:       []error{&github.APIError{StatusCode: http.StatusBadGateway}},
			random:     0.5,
			wantCalls:  2,
			wantSleeps: []time.Duration{time.Second},
		},
		{name: "github 422 is permanent", errs: []error{&github.APIError{StatusCode: http.StatusUnprocessableEntity}}, wantCalls: 1, wantErr: true},
		{
			name:       "attempt timeout is retried",
			errs:       []error{context.DeadlineExceeded},
			random:     0.5,
			wantCalls:  2,
			wantSleeps: []time.Duration{time.Second},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var sleeps []time.Duration
			p := Policy{
				Sleep: func(_ context.Context, d time.Duration) error {
					sleeps = append(sleeps, d)
					return nil
				},
				Random: func() float64 { return tt.random },
			}
			s := &scripted{errs: slices.Clone(tt.errs)}
			attempts, err := p.Do(t.Context(), s.delivery)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Do() error = %v, wantErr %v", err, tt.wantErr)
			}
			if attempts != tt.wantCalls || s.calls != tt.wantCalls {
				t.Errorf("attempts = %d, calls = %d, want %d", attempts, s.calls, tt.wantCalls)
			}
			if !slices.Equal(sleeps, tt.wantSleeps) {
				t.Errorf("sleeps = %v, want %v", sleeps, tt.wantSleeps)
			}
		})
	}
}

func TestPolicyAttemptTimeout(t *testing.T) {
	t.Parallel()

	p := Policy{Attempts: 2, AttemptTimeout: 20 * time.Millisecond, Sleep: noSleep, Random: func() float64 { return 0.5 }}
	calls := 0
	attempts, err := p.Do(t.Context(), func(ctx context.Context) error {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 20*time.Millisecond {
			t.Errorf("attempt deadline = %v, %v; want within 20ms", deadline, ok)
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) || attempts != 2 || calls != 2 {
		t.Errorf("Do() = %d, %v after %d calls; want 2 attempts ending in DeadlineExceeded", attempts, err, calls)
	}
}

func TestPolicyStopsWhenContextEnds(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	p := Policy{Sleep: func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}}
	s := &scripted{errs: []error{&StatusError{StatusCode: http.StatusServiceUnavailable}}}
	attempts, err := p.Do(ctx, s.delivery)
	if attempts != 1 || err == nil {
		t.Errorf("Do() = %d, %v; want to stop after the first attempt", attempts, err)
	}
}

func TestDefaultPolicy(t *testing.T) {
	t.Parallel()

	p := DefaultPolicy()
	if p.Attempts != 4 || p.BaseDelay != time.Second || p.MaxDelay != 30*time.Second || p.AttemptTimeout != 10*time.Second || p.Jitter != 0.2 {
		t.Errorf("DefaultPolicy() = %+v", p)
	}
	if SingleAttempt().Attempts != 1 {
		t.Errorf("SingleAttempt().Attempts = %d", SingleAttempt().Attempts)
	}
	for range 100 {
		if r := randomFraction(); r < 0 || r >= 1 {
			t.Fatalf("randomFraction() = %v, want [0, 1)", r)
		}
	}
	if err := sleep(t.Context(), time.Millisecond); err != nil {
		t.Errorf("sleep() = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleep() after cancel = %v", err)
	}
}
