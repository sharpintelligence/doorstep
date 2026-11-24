package cli

import (
	"log/slog"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/metrics"
	"github.com/sharpintelligence/doorstep/internal/notify"
)

func registerExternalGate(r *gates.Registry, reports gates.ExternalReports) error {
	return gates.RegisterExternal(r, reports)
}

func newGateStatusMirror(catalog config.Catalog, cfg config.Config, githubApp *github.App, logger *slog.Logger, recorder *metrics.Recorder) (*notify.GateStatuses, error) {
	if githubApp == nil {
		return nil, nil
	}
	links, err := notificationLinks(cfg)
	if err != nil {
		return nil, err
	}
	apps := make(map[string]bool, len(catalog.Apps))
	for _, a := range catalog.Apps {
		apps[a.Name] = a.Source.StatusesEnabled()
	}
	opts := notify.GateStatusOptions{Repos: githubApp, Apps: apps, Links: links, Logger: logger}
	if recorder != nil {
		opts.Observer = metricsObserver{recorder: recorder}
	}
	return notify.NewGateStatuses(opts)
}
