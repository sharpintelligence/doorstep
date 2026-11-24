package core

import "testing"

func TestAppTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		app  string
		want Target
	}{
		{app: "blog", want: Target{App: "blog", Environment: DefaultEnvironment}},
		{app: "api-gateway", want: Target{App: "api-gateway", Environment: "default"}},
	}
	for _, tt := range tests {
		t.Run(tt.app, func(t *testing.T) {
			t.Parallel()
			got := AppTarget(tt.app)
			if got != tt.want {
				t.Errorf("AppTarget(%q) = %+v, want %+v", tt.app, got, tt.want)
			}
			if err := got.Validate(); err != nil {
				t.Errorf("AppTarget(%q).Validate() = %v", tt.app, err)
			}
		})
	}
}
