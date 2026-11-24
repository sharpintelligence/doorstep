package gitops

import (
	"errors"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
)

func TestCurrent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		target  Target
		path    string
		content string
		want    Pinned
		wantErr error
	}{
		{
			name:    "kustomize pinned",
			target:  deployRequest().Target,
			path:    kustomizationPath,
			content: kustomization(digestA),
			want:    Pinned{Digest: digestA, Value: string(digestA), File: kustomizationPath},
		},
		{
			name:    "kustomize not pinned yet",
			target:  deployRequest().Target,
			path:    kustomizationPath,
			content: kustomization(""),
			want:    Pinned{File: kustomizationPath},
		},
		{
			name:    "yaml digest",
			target:  yamlRequest(FormatDigest).Target,
			path:    "apps/blog/values.yaml",
			content: "image:\n  digest: " + string(digestA) + "\n",
			want:    Pinned{Digest: digestA, Value: string(digestA), File: "apps/blog/values.yaml"},
		},
		{
			name:    "yaml placeholder",
			target:  yamlRequest(FormatDigest).Target,
			path:    "apps/blog/values.yaml",
			content: "image:\n  digest: \"\"\n",
			want:    Pinned{File: "apps/blog/values.yaml"},
		},
		{
			name:    "yaml reference",
			target:  yamlRequest(FormatReference).Target,
			path:    "apps/blog/values.yaml",
			content: "image:\n  digest: " + blogImage + "@" + string(digestA) + "\n",
			want:    Pinned{Digest: digestA, Value: blogImage + "@" + string(digestA), File: "apps/blog/values.yaml"},
		},
		{
			name:    "yaml reference to a tag",
			target:  yamlRequest(FormatReference).Target,
			path:    "apps/blog/values.yaml",
			content: "image:\n  digest: " + blogImage + ":1.0\n",
			want:    Pinned{Value: blogImage + ":1.0", File: "apps/blog/values.yaml"},
		},
		{
			name:    "missing file",
			target:  deployRequest().Target,
			path:    "apps/other/kustomization.yaml",
			content: kustomization(digestA),
			wantErr: ErrPinNotFound,
		},
		{
			name:    "invalid target",
			target:  Target{App: "blog"},
			path:    kustomizationPath,
			content: kustomization(digestA),
			wantErr: ErrInvalidRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, Options{})
			f.repo.Seed("main", tt.path, tt.content)
			got, err := f.writer.Current(t.Context(), tt.target)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Current() error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("Current() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRepoNotInstalled(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	target := deployRequest().Target
	target.Repo = "missing"
	if _, err := f.writer.Current(t.Context(), target); err == nil {
		t.Fatal("Current() of a repository without an installation succeeded")
	}
	if _, err := f.writer.Pin(t.Context(), Request{Target: target, Digest: core.Digest("x")}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Pin() error = %v", err)
	}
}
