package cli

import (
	"reflect"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/intake"
)

func TestIntakeAppsMigrationPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		migrations *config.Migrations
		want       *intake.MigrationPolicy
	}{
		{name: "no block", want: nil},
		{name: "trailer by default", migrations: &config.Migrations{}, want: &intake.MigrationPolicy{Trailer: true}},
		{
			name:       "labels and trailer",
			migrations: &config.Migrations{Labels: []string{"db-migration"}, Trailer: new(true)},
			want:       &intake.MigrationPolicy{Labels: []string{"db-migration"}, Trailer: true},
		},
		{
			name:       "labels only",
			migrations: &config.Migrations{Labels: []string{"db-migration", "schema"}, Trailer: new(false)},
			want:       &intake.MigrationPolicy{Labels: []string{"db-migration", "schema"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			catalog := config.Catalog{Apps: []config.App{{
				Name:       "blog",
				Source:     config.Source{Repo: "acme/blog", Branch: "main"},
				Image:      config.Image{Repository: "ghcr.io/acme/blog"},
				Migrations: tt.migrations,
			}}}
			apps := intakeApps(catalog)
			if len(apps) != 1 {
				t.Fatalf("intakeApps returned %d apps", len(apps))
			}
			if !reflect.DeepEqual(apps[0].Migrations, tt.want) {
				t.Errorf("Migrations = %+v, want %+v", apps[0].Migrations, tt.want)
			}
			if err := apps[0].Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}
