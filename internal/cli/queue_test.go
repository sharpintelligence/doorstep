package cli

import (
	"testing"

	"github.com/sharpintelligence/doorstep/internal/config"
)

func TestCatalogApps(t *testing.T) {
	t.Parallel()

	three := 3
	apps := newCatalogApps(config.Catalog{Apps: []config.App{
		{Name: "blog", Deploy: config.Deploy{MaxBatch: &three}},
		{Name: "shop"},
	}})
	tests := []struct {
		app       string
		wantKnown bool
		wantBatch int
	}{
		{app: "blog", wantKnown: true, wantBatch: 3},
		{app: "shop", wantKnown: true, wantBatch: config.DefaultMaxBatch},
		{app: "removed", wantKnown: false, wantBatch: 1},
	}
	for _, tt := range tests {
		if got := apps.Known(tt.app); got != tt.wantKnown {
			t.Errorf("Known(%q) = %v, want %v", tt.app, got, tt.wantKnown)
		}
		if got := apps.MaxBatch(tt.app); got != tt.wantBatch {
			t.Errorf("MaxBatch(%q) = %d, want %d", tt.app, got, tt.wantBatch)
		}
	}
}
