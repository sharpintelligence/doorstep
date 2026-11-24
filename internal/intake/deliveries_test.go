package intake_test

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sharpintelligence/doorstep/internal/intake"
)

func TestSweeperRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		opts          intake.SweeperOptions
		wait          time.Duration
		wantRetention time.Duration
		wantCalls     int
	}{
		{name: "defaults sweep hourly with seven days of retention", wait: 2 * time.Hour, wantRetention: 7 * 24 * time.Hour, wantCalls: 3},
		{name: "custom interval and retention", opts: intake.SweeperOptions{Every: time.Minute, Retention: time.Hour}, wait: 5 * time.Minute, wantRetention: time.Hour, wantCalls: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				expired := &fakeExpired{deleted: 2}
				opts := tt.opts
				opts.Deliveries = expired
				sweeper, err := intake.NewSweeper(opts)
				if err != nil {
					t.Fatalf("NewSweeper: %v", err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- sweeper.Run(ctx) }()
				time.Sleep(tt.wait)
				synctest.Wait()
				calls := expired.recorded()
				if want := slices.Repeat([]time.Duration{tt.wantRetention}, tt.wantCalls); !slices.Equal(calls, want) {
					t.Errorf("sweeps = %v, want %v", calls, want)
				}
				cancel()
				synctest.Wait()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("Run = %v, want nil", err)
					}
				default:
					t.Fatal("Run did not return after cancellation")
				}
			})
		})
	}
}

func TestSweeperLogsFailuresAndKeepsGoing(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		expired := &fakeExpired{err: errBoom}
		sweeper, err := intake.NewSweeper(intake.SweeperOptions{
			Deliveries: expired,
			Logger:     slog.New(slog.NewJSONHandler(&logs, nil)),
		})
		if err != nil {
			t.Fatalf("NewSweeper: %v", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = sweeper.Run(ctx) }()
		time.Sleep(time.Hour)
		synctest.Wait()
		if got := len(expired.recorded()); got != 2 {
			t.Errorf("sweeps = %d, want 2", got)
		}
		if n := strings.Count(logs.String(), "webhook delivery sweep failed"); n != 2 {
			t.Errorf("logged %d failures, want 2: %s", n, logs.String())
		}
	})
}

func TestNewSweeperValidatesOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    intake.SweeperOptions
		wantErr string
	}{
		{name: "valid", opts: intake.SweeperOptions{Deliveries: &fakeExpired{}}},
		{name: "no deliveries", opts: intake.SweeperOptions{}, wantErr: "deliveries store is required"},
		{name: "negative interval", opts: intake.SweeperOptions{Deliveries: &fakeExpired{}, Every: -time.Second}, wantErr: "must not be negative"},
		{name: "negative retention", opts: intake.SweeperOptions{Deliveries: &fakeExpired{}, Retention: -time.Second}, wantErr: "must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := intake.NewSweeper(tt.opts)
			if (err == nil) != (tt.wantErr == "") || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("NewSweeper error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
