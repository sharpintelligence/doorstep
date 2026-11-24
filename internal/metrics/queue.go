package metrics

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const queueDepthTimeout = 2 * time.Second

type QueueDepthFunc func(context.Context) (int, error)

type queueDepthCollector struct {
	desc     *prometheus.Desc
	errDesc  *prometheus.Desc
	depth    QueueDepthFunc
	logger   *slog.Logger
	timeout  time.Duration
	failing  atomic.Bool
	failures atomic.Uint64
}

type queueDepthResult struct {
	depth int
	err   error
}

func newQueueDepthCollector(depth QueueDepthFunc, logger *slog.Logger) *queueDepthCollector {
	return &queueDepthCollector{
		desc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "queue_depth"),
			"Deployments waiting in the queue, read from the database at scrape time.",
			nil, nil,
		),
		errDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "queue_depth_errors_total"),
			"Scrapes where the queue depth could not be read.",
			nil, nil,
		),
		depth:   depth,
		logger:  logger,
		timeout: queueDepthTimeout,
	}
}

func (c *queueDepthCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
	ch <- c.errDesc
}

func (c *queueDepthCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	results := make(chan queueDepthResult, 1)
	go func() {
		depth, err := c.depth(ctx)
		results <- queueDepthResult{depth: depth, err: err}
	}()

	var result queueDepthResult
	select {
	case result = <-results:
	case <-ctx.Done():
		result.err = ctx.Err()
	}

	if result.err != nil {
		c.failures.Add(1)
		if !c.failing.Swap(true) {
			c.logger.WarnContext(ctx, "queue depth unavailable", slog.Any("error", result.err))
		}
	} else {
		if c.failing.Swap(false) {
			c.logger.InfoContext(ctx, "queue depth available again")
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(result.depth))
	}
	ch <- prometheus.MustNewConstMetric(c.errDesc, prometheus.CounterValue, float64(c.failures.Load()))
}
