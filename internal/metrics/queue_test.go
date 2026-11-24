package metrics

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
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
			wantSamples: 1,
			wantValue: `
# HELP doorstep_queue_depth Deployments waiting in the queue, read from the database at scrape time.
# TYPE doorstep_queue_depth gauge
doorstep_queue_depth 7
`,
		},
		{
			name:        "zero is reported",
			depth:       func(context.Context) (int, error) { return 0, nil },
			wantSamples: 1,
			wantValue: `
# HELP doorstep_queue_depth Deployments waiting in the queue, read from the database at scrape time.
# TYPE doorstep_queue_depth gauge
doorstep_queue_depth 0
`,
		},
		{
			name:    "error omits the sample and logs a warning",
			depth:   func(context.Context) (int, error) { return 0, errors.New("db down") },
			wantLog: "queue depth unavailable",
		},
		{
			name:    "blocked callback is cancelled by the timeout",
			timeout: 20 * time.Millisecond,
			depth: func(ctx context.Context) (int, error) {
				<-ctx.Done()
				return 0, ctx.Err()
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
