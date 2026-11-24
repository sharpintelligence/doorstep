package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const queueDepthTimeout = 2 * time.Second

type QueueDepthFunc func(context.Context) (int, error)

type queueDepthCollector struct {
	desc    *prometheus.Desc
	depth   QueueDepthFunc
	logger  *slog.Logger
	timeout time.Duration
}

func newQueueDepthCollector(depth QueueDepthFunc, logger *slog.Logger) *queueDepthCollector {
	return &queueDepthCollector{
		desc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "queue_depth"),
			"Deployments waiting in the queue, read from the database at scrape time.",
			nil, nil,
		),
		depth:   depth,
		logger:  logger,
		timeout: queueDepthTimeout,
	}
}

func (c *queueDepthCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *queueDepthCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	n, err := c.depth(ctx)
	if err != nil {
		c.logger.WarnContext(ctx, "queue depth unavailable", slog.Any("error", err))
		return
	}
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n))
}
