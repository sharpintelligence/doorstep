package config

import (
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestValidateLogsURL(t *testing.T) {
	t.Parallel()

	everyPlaceholder := "https://logs.example.com/{app}/{namespace}/{job}/{uid}/{deployment}/{argocd_namespace}/{argocd_application}"
	tests := []struct {
		name     string
		template string
		wantErr  string
	}{
		{name: "argo cd ui", template: "https://argocd.example.com/applications/{argocd_namespace}/{argocd_application}?node=batch%2FJob%2F{namespace}%2F{job}%2F0&tab=logs"},
		{name: "every placeholder", template: everyPlaceholder},
		{name: "no placeholders", template: "http://grafana.internal:3000/d/jobs"},
		{name: "placeholder in host", template: "https://{app}.logs.example.com/"},
		{name: "grafana loki", template: "https://grafana.example.com/explore?orgId=1&left=%7B%22queries%22%3A%5B%7B%22expr%22%3A%22%7Bnamespace%3D%5C%22{namespace}%5C%22%2Cjob_name%3D%5C%22{job}%5C%22%7D%22%7D%5D%7D"},
		{name: "encoded braces", template: "https://grafana.example.com/explore?left=%7B%22expr%22%3A%22{job}%22%7D"},
		{name: "unknown placeholder", template: "https://logs.example.com/{pod}", wantErr: "unknown placeholder {pod}"},
		{name: "empty placeholder", template: "https://logs.example.com/{}", wantErr: "unknown placeholder {}"},
		{name: "unclosed brace", template: "https://logs.example.com/{job", wantErr: "'{' without a matching '}'"},
		{name: "stray closing brace", template: "https://logs.example.com/job}", wantErr: "'}' without a matching '{'"},
		{name: "raw json braces", template: `https://grafana.example.com/explore?left={"expr":"x"}`, wantErr: `unknown placeholder {"expr":"x"}`},
		{name: "ftp scheme", template: "ftp://logs.example.com/{job}", wantErr: "must be an absolute http or https URL"},
		{name: "javascript scheme", template: "javascript:alert({job})", wantErr: "must be an absolute http or https URL"},
		{name: "relative", template: "/logs/{job}", wantErr: "must be an absolute http or https URL"},
		{name: "scheme relative", template: "//logs.example.com/{job}", wantErr: "must be an absolute http or https URL"},
		{name: "no host", template: "https:///{job}", wantErr: "must include a host"},
		{name: "userinfo", template: "https://user:pass@logs.example.com/{job}", wantErr: "must not include credentials"},
		{name: "whitespace", template: "https://logs.example.com/{job} now", wantErr: "must not contain whitespace"},
		{name: "invalid url", template: "https://logs.example.com:port/{job}", wantErr: "is not a valid URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateLogsURL(tt.template)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateLogsURL(%q) = %v", tt.template, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateLogsURL(%q) = %v, want an error containing %q", tt.template, err, tt.wantErr)
			}
		})
	}
}

func TestJobGateRenderLogsURL(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"app":                "web",
		"namespace":          "web-prod",
		"job":                "smoke-test",
		"uid":                "0b6c7f4e-1a2b-4c3d-9e8f-001122334455",
		"deployment":         "42",
		"argocd_namespace":   "argocd",
		"argocd_application": "web-prod",
	}
	tests := []struct {
		name     string
		template string
		values   map[string]string
		want     string
	}{
		{name: "no template", template: "", values: values, want: ""},
		{
			name:     "argo cd ui",
			template: "https://argocd.example.com/applications/{argocd_namespace}/{argocd_application}?node=batch%2FJob%2F{namespace}%2F{job}%2F0&tab=logs",
			values:   values,
			want:     "https://argocd.example.com/applications/argocd/web-prod?node=batch%2FJob%2Fweb-prod%2Fsmoke-test%2F0&tab=logs",
		},
		{
			name:     "every placeholder",
			template: "https://logs.example.com/{app}/{namespace}/{job}/{uid}/{deployment}/{argocd_namespace}/{argocd_application}",
			values:   values,
			want:     "https://logs.example.com/web/web-prod/smoke-test/0b6c7f4e-1a2b-4c3d-9e8f-001122334455/42/argocd/web-prod",
		},
		{
			name:     "repeated placeholder",
			template: "https://logs.example.com/{job}/{job}",
			values:   values,
			want:     "https://logs.example.com/smoke-test/smoke-test",
		},
		{
			name:     "values are path escaped",
			template: "https://logs.example.com/{app}",
			values:   map[string]string{"app": "a/../b?c=d#e f"},
			want:     "https://logs.example.com/" + url.PathEscape("a/../b?c=d#e f"),
		},
		{
			name:     "missing value renders empty",
			template: "https://logs.example.com/{job}/{uid}",
			values:   map[string]string{"job": "smoke"},
			want:     "https://logs.example.com/smoke/",
		},
		{
			name:     "invalid template renders nothing",
			template: "https://logs.example.com/{pod}",
			values:   values,
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := JobGate{LogsURL: tt.template}.RenderLogsURL(tt.values)
			if got != tt.want {
				t.Errorf("RenderLogsURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJobLogsPlaceholders(t *testing.T) {
	t.Parallel()

	got := JobLogsPlaceholders()
	want := []string{"app", "namespace", "job", "uid", "deployment", "argocd_namespace", "argocd_application"}
	if !slices.Equal(got, want) {
		t.Errorf("JobLogsPlaceholders() = %v, want %v", got, want)
	}
	got[0] = "changed"
	if JobLogsPlaceholders()[0] != "app" {
		t.Error("JobLogsPlaceholders returns shared memory")
	}
}
