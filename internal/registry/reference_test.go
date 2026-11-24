package registry

import (
	"strings"
	"testing"
)

func TestParseReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		repository   string
		tag          string
		wantRegistry string
		wantRepo     string
		wantErr      string
	}{
		{name: "official image", repository: "nginx", tag: "1.27", wantRegistry: "index.docker.io", wantRepo: "library/nginx"},
		{name: "explicit library", repository: "library/nginx", tag: "1.27", wantRegistry: "index.docker.io", wantRepo: "library/nginx"},
		{name: "docker hub user", repository: "owner/app", tag: commitSHA, wantRegistry: "index.docker.io", wantRepo: "owner/app"},
		{name: "docker.io alias", repository: "docker.io/owner/app", tag: commitSHA, wantRegistry: "index.docker.io", wantRepo: "owner/app"},
		{name: "index.docker.io", repository: "index.docker.io/owner/app", tag: commitSHA, wantRegistry: "index.docker.io", wantRepo: "owner/app"},
		{name: "ghcr", repository: "ghcr.io/owner/app", tag: commitSHA, wantRegistry: "ghcr.io", wantRepo: "owner/app"},
		{name: "nested path", repository: "harbor.example.com/project/team/app", tag: "v1.2.3", wantRegistry: "harbor.example.com", wantRepo: "project/team/app"},
		{name: "localhost with port", repository: "localhost:5000/app", tag: "latest", wantRegistry: "localhost:5000", wantRepo: "app"},
		{name: "ip with port", repository: "10.0.0.5:5000/team/app", tag: "v1", wantRegistry: "10.0.0.5:5000", wantRepo: "team/app"},
		{name: "underscore tag", repository: "ghcr.io/owner/app", tag: "_build", wantRegistry: "ghcr.io", wantRepo: "owner/app"},
		{name: "128 character tag", repository: "ghcr.io/owner/app", tag: strings.Repeat("a", 128), wantRegistry: "ghcr.io", wantRepo: "owner/app"},
		{name: "empty tag", repository: "ghcr.io/owner/app", tag: "", wantErr: "tag must not be empty"},
		{name: "tag with slash", repository: "ghcr.io/owner/app", tag: "feature/x", wantErr: "not a valid OCI tag"},
		{name: "tag starting with dash", repository: "ghcr.io/owner/app", tag: "-x", wantErr: "not a valid OCI tag"},
		{name: "tag with space", repository: "ghcr.io/owner/app", tag: "v 1", wantErr: "not a valid OCI tag"},
		{name: "129 character tag", repository: "ghcr.io/owner/app", tag: strings.Repeat("a", 129), wantErr: "not a valid OCI tag"},
		{name: "empty repository", repository: "", tag: "v1", wantErr: "repository"},
		{name: "uppercase repository", repository: "ghcr.io/Owner/app", tag: "v1", wantErr: "repository"},
		{name: "repository with tag", repository: "ghcr.io/owner/app:v1", tag: "v1", wantErr: "repository"},
		{name: "official image with tag", repository: "nginx:1.27", tag: "1.27", wantErr: "repository"},
		{name: "repository with digest", repository: "ghcr.io/owner/app@sha256:" + strings.Repeat("a", 64), tag: "v1", wantErr: "repository"},
		{name: "scheme", repository: "https://ghcr.io/owner/app", tag: "v1", wantErr: "repository"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ref, err := parseReference(tt.repository, tt.tag)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parseReference(%q, %q) = %s, want error", tt.repository, tt.tag, ref)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseReference(%q, %q): %v", tt.repository, tt.tag, err)
			}
			if got := ref.Context().RegistryStr(); got != tt.wantRegistry {
				t.Errorf("registry = %q, want %q", got, tt.wantRegistry)
			}
			if got := ref.Context().RepositoryStr(); got != tt.wantRepo {
				t.Errorf("repository = %q, want %q", got, tt.wantRepo)
			}
			if got := ref.TagStr(); got != tt.tag {
				t.Errorf("tag = %q, want %q", got, tt.tag)
			}
		})
	}
}
