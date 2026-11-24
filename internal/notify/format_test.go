package notify

import (
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
)

func TestDescribeTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target core.Target
		want   string
	}{
		{name: "default environment", target: core.Target{App: "blog", Environment: core.DefaultEnvironment}, want: "blog"},
		{name: "no environment", target: core.Target{App: "blog"}, want: "blog"},
		{name: "environment named after the app", target: core.Target{App: "blog", Environment: "blog"}, want: "blog"},
		{name: "named environment", target: core.Target{App: "blog", Environment: "prod"}, want: "blog (prod)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := describeTarget(tt.target); got != tt.want {
				t.Errorf("describeTarget(%+v) = %q, want %q", tt.target, got, tt.want)
			}
		})
	}
}
