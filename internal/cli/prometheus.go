package cli

import (
	"fmt"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/promquery"
)

func registerPrometheusGate(r *gates.Registry, catalog config.Catalog, secrets secretSource) error {
	servers := make(map[string]gates.MetricsQuerier, len(catalog.Prometheus))
	for _, s := range catalog.Prometheus {
		client, err := newPrometheusClient(s, secrets)
		if err != nil {
			return err
		}
		servers[s.Name] = client
	}
	return gates.RegisterPrometheus(r, gates.Prometheus{Servers: servers})
}

func newPrometheusClient(s config.PrometheusServer, secrets secretSource) (*promquery.Client, error) {
	opts := promquery.Options{BaseURL: s.URL, HTTPClient: promquery.NewHTTPClient(), UserAgent: userAgent()}
	if token, ok := s.BearerToken(); ok {
		opts.Authorize = promquery.BearerAuth(secrets.reader(token))
	} else if password, ok := s.Password(); ok {
		opts.Authorize = promquery.BasicAuth(s.Username, secrets.reader(password))
	}
	client, err := promquery.New(opts)
	if err != nil {
		return nil, fmt.Errorf("prometheus %s: %w", s.Name, err)
	}
	return client, nil
}
