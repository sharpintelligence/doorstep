package metrics

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sharpintelligence/doorstep/internal/buildinfo"
)

const (
	maxScrapesInFlight = 4
	scrapeTimeout      = 10 * time.Second
)

type Options struct {
	Logger     *slog.Logger
	Build      buildinfo.Info
	QueueDepth QueueDepthFunc
}

type Metrics struct {
	registry *prometheus.Registry
	recorder *Recorder
	logger   *slog.Logger
}

func New(opts Options) *Metrics {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		recorder: newRecorder(),
		logger:   logger,
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		newBuildInfoCollector(opts.Build),
	)
	m.registry.MustRegister(m.recorder.collectors()...)
	if opts.QueueDepth != nil {
		m.registry.MustRegister(newQueueDepthCollector(opts.QueueDepth, logger))
	}
	return m
}

func newBuildInfoCollector(info buildinfo.Info) prometheus.Collector {
	return prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "build_info",
		Help:      "Build information about the running doorstep binary; the value is always 1.",
		ConstLabels: prometheus.Labels{
			"version":    info.Version,
			"commit":     info.Commit,
			"go_version": info.GoVersion,
		},
	}, func() float64 { return 1 })
}

func (m *Metrics) Recorder() *Recorder {
	return m.recorder
}

func (m *Metrics) Gatherer() prometheus.Gatherer {
	return m.registry
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		Registry:            m.registry,
		ErrorHandling:       promhttp.ContinueOnError,
		ErrorLog:            errorLogger{logger: m.logger},
		MaxRequestsInFlight: maxScrapesInFlight,
		Timeout:             scrapeTimeout,
	})
}

type errorLogger struct {
	logger *slog.Logger
}

func (l errorLogger) Println(v ...any) {
	l.logger.Error("metrics scrape error", slog.String("error", fmt.Sprint(v...)))
}
