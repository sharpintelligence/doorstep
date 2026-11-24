package cli

import (
	"github.com/sharpintelligence/doorstep/internal/config"
)

type catalogApps map[string]config.App

func newCatalogApps(catalog config.Catalog) catalogApps {
	apps := make(catalogApps, len(catalog.Apps))
	for _, a := range catalog.Apps {
		apps[a.Name] = a
	}
	return apps
}

func (c catalogApps) Known(app string) bool {
	_, ok := c[app]
	return ok
}

func (c catalogApps) MaxBatch(app string) int {
	a, ok := c[app]
	if !ok {
		return 1
	}
	return a.Deploy.BatchLimit()
}
