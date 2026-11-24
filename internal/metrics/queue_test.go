package metrics

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newBufferLogger() (*slog.Logger, *lockedBuffer) {
	var buf lockedBuffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestQueueDepthCollector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		depth       QueueDepthFunc
		timeout     time.Duration
		wantSamples int
		wantValue   string
		wantLog     string
	}{
		{
			name:        "value reported",
			depth:       func(context.Context) (int, error) { return 7, nil },
			wantSamples: 2,
			wantValue: `
# HELP doorstep_queue_depth Deployments waiting in the queue, read from the database at scrape time.
# TYPE doorstep_queue_depth gauge
doorstep_queue_depth 7
# HELP doorstep_queue_depth_errors_total Scrapes where the queue depth could not be read.
# TYPE doorstep_queue_depth_errors_total counter
doorstep_queue_depth_errors_total 0
`,
		},
		{
			name:        "zero is reported",
			depth:       func(context.Context) (int, error) { return 0, nil },
			wantSamples: 2,
			wantValue: `
# HELP doorstep_queue_depth Deployments waiting in the queue, read from the database at scrape time.
# TYPE doorstep_queue_depth gauge
doorstep_queue_depth 0
# HELP doorstep_queue_depth_errors_total Scrapes where the queue depth could not be read.
# TYPE doorstep_queue_depth_errors_total counter
doorstep_queue_depth_errors_total 0
`,
		},
		{
			name:        "error omits the depth, counts the failure and logs a warning",
			depth:       func(context.Context) (int, error) { return 0, errors.New("db down") },
			wantSamples: 1,
			wantLog:     "queue depth unavailable",
		},
		{
			name:        "blocked callback is cancelled by the timeout",
			timeout:     20 * time.Millisecond,
			wantSamples: 1,
			depth: func(ctx context.Context) (int, error) {
				<-ctx.Done()
				return 0, ctx.Err()
			},
			wantLog: "context deadline exceeded",
		},
		{
			name:        "callback ignoring its context cannot block the scrape",
			timeout:     20 * time.Millisecond,
			wantSamples: 1,
			depth: func(context.Context) (int, error) {
				time.Sleep(time.Second)
				return 1, nil
			},
			wantLog: "context deadline exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, logs := newBufferLogger()
			collector := newQueueDepthCollector(tt.depth, logger)
			if tt.timeout > 0 {
				collector.timeout = tt.timeout
			}

			if got := testutil.CollectAndCount(collector); got != tt.wantSamples {
				t.Errorf("samples = %d, want %d", got, tt.wantSamples)
			}
			if tt.wantValue != "" {
				if err := testutil.CollectAndCompare(collector, strings.NewReader(tt.wantValue)); err != nil {
					t.Error(err)
				}
			}
			if tt.wantLog != "" && !strings.Contains(logs.String(), tt.wantLog) {
				t.Errorf("logs %q missing %q", logs.String(), tt.wantLog)
			}
			if tt.wantLog == "" && logs.String() != "" {
				t.Errorf("unexpected logs %q", logs.String())
			}
		})
	}
}

func TestQueueDepthCollectorFailureLogging(t *testing.T) {
	t.Parallel()

	var fail atomic.Bool
	fail.Store(true)
	logger, logs := newBufferLogger()
	collector := newQueueDepthCollector(func(context.Context) (int, error) {
		if fail.Load() {
			return 0, errors.New("db down")
		}
		return 3, nil
	}, logger)

	steps := []struct {
		name         string
		failing      bool
		wantWarnings int
		wantRecovery int
		wantFailures string
	}{
		{name: "first failure warns", failing: true, wantWarnings: 1, wantFailures: "1"},
		{name: "repeated failure is silent", failing: true, wantWarnings: 1, wantFailures: "2"},
		{name: "recovery is logged once", failing: false, wantWarnings: 1, wantRecovery: 1, wantFailures: "2"},
		{name: "healthy scrape is silent", failing: false, wantWarnings: 1, wantRecovery: 1, wantFailures: "2"},
		{name: "new failure warns again", failing: true, wantWarnings: 2, wantRecovery: 1, wantFailures: "3"},
	}

	for _, step := range steps {
		fail.Store(step.failing)
		want := "\n# HELP doorstep_queue_depth_errors_total Scrapes where the queue depth could not be read.\n# TYPE doorstep_queue_depth_errors_total counter\ndoorstep_queue_depth_errors_total " + step.wantFailures + "\n"
		if err := testutil.CollectAndCompare(collector, strings.NewReader(want), "doorstep_queue_depth_errors_total"); err != nil {
			t.Errorf("%s: %v", step.name, err)
		}
		if got := strings.Count(logs.String(), "queue depth unavailable"); got != step.wantWarnings {
			t.Errorf("%s: warnings = %d, want %d", step.name, got, step.wantWarnings)
		}
		if got := strings.Count(logs.String(), "queue depth available again"); got != step.wantRecovery {
			t.Errorf("%s: recoveries = %d, want %d", step.name, got, step.wantRecovery)
		}
	}
}
