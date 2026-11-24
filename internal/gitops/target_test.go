package gitops

import (
	"reflect"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
)

func yamlRequest(format Format) Request {
	req := deployRequest()
	req.Target.Kustomize = nil
	req.Target.YAML = &YAMLValue{File: "values.yaml", Path: []string{"image", "digest"}, Format: format}
	return req
}

func TestRequestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Request)
		wantErr bool
	}{
		{name: "valid kustomize", mutate: func(*Request) {}},
		{name: "valid yaml", mutate: func(r *Request) { *r = yamlRequest(FormatReference) }},
		{name: "valid rollback", mutate: func(r *Request) { *r = rollbackRequest() }},
		{name: "repository root", mutate: func(r *Request) { r.Target.Dir = "" }},
		{name: "pull request mode", mutate: func(r *Request) { r.Target.Mode = ModePullRequest }},
		{name: "app not a DNS label", mutate: func(r *Request) { r.Target.App = "Blog_1" }, wantErr: true},
		{name: "app too long", mutate: func(r *Request) { r.Target.App = string(make([]byte, 64)) }, wantErr: true},
		{name: "missing owner", mutate: func(r *Request) { r.Target.Owner = "" }, wantErr: true},
		{name: "missing branch", mutate: func(r *Request) { r.Target.Branch = "" }, wantErr: true},
		{name: "branch with space", mutate: func(r *Request) { r.Target.Branch = "a b" }, wantErr: true},
		{name: "absolute dir", mutate: func(r *Request) { r.Target.Dir = "/apps" }, wantErr: true},
		{name: "dir escapes", mutate: func(r *Request) { r.Target.Dir = "apps/../../x" }, wantErr: true},
		{name: "image with digest", mutate: func(r *Request) { r.Target.ImageRepository = blogImage + "@" + string(digestA) }, wantErr: true},
		{name: "no strategy", mutate: func(r *Request) { r.Target.Kustomize = nil }, wantErr: true},
		{name: "two strategies", mutate: func(r *Request) { r.Target.YAML = yamlRequest(FormatDigest).Target.YAML }, wantErr: true},
		{name: "empty kustomize name", mutate: func(r *Request) { r.Target.Kustomize.Name = "" }, wantErr: true},
		{name: "yaml file escapes", mutate: func(r *Request) { *r = yamlRequest(FormatDigest); r.Target.YAML.File = "../x.yaml" }, wantErr: true},
		{name: "yaml file missing", mutate: func(r *Request) { *r = yamlRequest(FormatDigest); r.Target.YAML.File = "" }, wantErr: true},
		{name: "yaml path missing", mutate: func(r *Request) { *r = yamlRequest(FormatDigest); r.Target.YAML.Path = nil }, wantErr: true},
		{name: "yaml path empty segment", mutate: func(r *Request) { *r = yamlRequest(FormatDigest); r.Target.YAML.Path = []string{"a", ""} }, wantErr: true},
		{name: "yaml format unknown", mutate: func(r *Request) { *r = yamlRequest("tag") }, wantErr: true},
		{name: "unknown mode", mutate: func(r *Request) { r.Target.Mode = "" }, wantErr: true},
		{name: "bad digest", mutate: func(r *Request) { r.Digest = "sha256:abc" }, wantErr: true},
		{name: "zero deployment", mutate: func(r *Request) { r.Deployment = 0 }, wantErr: true},
		{name: "unknown kind", mutate: func(r *Request) { r.Kind = "promote" }, wantErr: true},
		{name: "bad app commit", mutate: func(r *Request) { r.AppCommit.SHA = "main" }, wantErr: true},
		{name: "app commit repository without owner", mutate: func(r *Request) { r.AppCommit.Repository = "blog" }, wantErr: true},
		{name: "app commit repository with newline", mutate: func(r *Request) { r.AppCommit.Repository = "acme/blog\nx" }, wantErr: true},
		{name: "rollback without target", mutate: func(r *Request) { r.Kind = core.KindRollback }, wantErr: true},
		{name: "deploy with rollback target", mutate: func(r *Request) { *r = rollbackRequest(); r.Kind = core.KindDeploy }, wantErr: true},
		{name: "bad block", mutate: func(r *Request) { r.Blocks = []core.Digest{"sha256:x"} }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := deployRequest()
			tt.mutate(&req)
			if err := req.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestTargetValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		target    Target
		wantFiles []string
		desired   string
		digests   map[string]core.Digest
	}{
		{
			name:      "kustomize",
			target:    deployRequest().Target,
			wantFiles: []string{"apps/blog/kustomization.yaml", "apps/blog/kustomization.yml", "apps/blog/Kustomization"},
			desired:   string(digestB),
			digests: map[string]core.Digest{
				string(digestA): digestA,
				"":              "",
				"v1":            "",
			},
		},
		{
			name:      "yaml digest",
			target:    yamlRequest(FormatDigest).Target,
			wantFiles: []string{"apps/blog/values.yaml"},
			desired:   string(digestB),
			digests: map[string]core.Digest{
				string(digestA):                   digestA,
				blogImage + "@" + string(digestA): "",
			},
		},
		{
			name:      "yaml reference",
			target:    yamlRequest(FormatReference).Target,
			wantFiles: []string{"apps/blog/values.yaml"},
			desired:   blogImage + "@" + string(digestB),
			digests: map[string]core.Digest{
				blogImage + "@" + string(digestA):           digestA,
				"ghcr.io/acme/other@" + string(digestA):     "",
				blogImage + ":1.0":                          "",
				string(digestA):                             "",
				blogImage + "@sha256:" + "not-a-digest-xyz": "",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.target.candidateFiles(); !reflect.DeepEqual(got, tt.wantFiles) {
				t.Errorf("candidateFiles() = %v, want %v", got, tt.wantFiles)
			}
			if got := tt.target.desiredValue(digestB); got != tt.desired {
				t.Errorf("desiredValue() = %q, want %q", got, tt.desired)
			}
			for value, want := range tt.digests {
				if got := tt.target.digestOf(value); got != want {
					t.Errorf("digestOf(%q) = %q, want %q", value, got, want)
				}
			}
		})
	}
}

func TestHeadBranch(t *testing.T) {
	t.Parallel()

	if got := deployRequest().headBranch(); got != "doorstep/blog-42" {
		t.Fatalf("headBranch() = %q", got)
	}
}
