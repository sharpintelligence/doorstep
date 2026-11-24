package config

import (
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestDurationUnmarshalYAML(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    time.Duration
		wantErr []string
	}{
		{name: "seconds", input: "d: 30s", want: 30 * time.Second},
		{name: "compound", input: "d: 1h30m", want: 90 * time.Minute},
		{name: "quoted", input: `d: "10m"`, want: 10 * time.Minute},
		{name: "quoted zero", input: `d: "0s"`, want: 0},
		{name: "negative is left to validation", input: "d: -5s", want: -5 * time.Second},
		{name: "null keeps zero", input: "d: ~", want: 0},
		{name: "bare integer", input: "d: 30", wantErr: []string{"line 1", "duration must be a string with a unit"}},
		{name: "bare zero", input: "d: 0", wantErr: []string{"line 1", "with a unit"}},
		{name: "float", input: "d: 1.5", wantErr: []string{"line 1", "with a unit"}},
		{name: "boolean", input: "d: true", wantErr: []string{"with a unit"}},
		{name: "missing unit in string", input: `d: "30"`, wantErr: []string{`invalid duration "30"`}},
		{name: "garbage", input: "\n\nd: soon", wantErr: []string{"line 3", `invalid duration "soon"`}},
		{name: "mapping", input: "d:\n  seconds: 3", wantErr: []string{"line 2", "with a unit"}},
		{name: "sequence", input: "d: [1s]", wantErr: []string{"with a unit"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got struct {
				D Duration `yaml:"d"`
			}
			err := yaml.Unmarshal([]byte(tt.input), &got)
			assertErrorContains(t, err, tt.wantErr)
			if err == nil && got.D.Std() != tt.want {
				t.Errorf("duration = %s, want %s", got.D, tt.want)
			}
		})
	}
}

func TestDurationString(t *testing.T) {
	t.Parallel()

	if got := Duration(90 * time.Second).String(); got != "1m30s" {
		t.Errorf("String() = %q, want 1m30s", got)
	}
}
