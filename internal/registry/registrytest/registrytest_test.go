package registrytest_test

import (
	"errors"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/registry"
	"github.com/sharpintelligence/doorstep/internal/registry/registrytest"
)

func TestServerResolvesPushedTags(t *testing.T) {
	t.Parallel()

	srv := registrytest.NewServer(t)
	first := srv.Push("team/app", "v1")
	second := srv.Push("team/app", "v2")
	if first == second {
		t.Fatalf("different tags got the same digest %s", first)
	}
	resolver := registry.New(registry.Options{})
	t.Cleanup(func() {
		if srv.Requests() == 0 {
			t.Error("no requests counted")
		}
	})

	tests := []struct {
		name string
		repo string
		tag  string
		want registry.Digest
		err  error
	}{
		{name: "pushed tag", repo: srv.Repo("team/app"), tag: "v1", want: registry.Digest(first)},
		{name: "other tag", repo: srv.Repo("team/app"), tag: "v2", want: registry.Digest(second)},
		{name: "unknown tag", repo: srv.Repo("team/app"), tag: "v3", err: registry.ErrNotFound},
		{name: "unknown repository", repo: srv.Repo("team/other"), tag: "v1", err: registry.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolver.Resolve(t.Context(), tt.repo, tt.tag)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("Resolve = %s, %v, want %v", got, err, tt.err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Resolve = %s, %v, want %s", got, err, tt.want)
			}
		})
	}
}
