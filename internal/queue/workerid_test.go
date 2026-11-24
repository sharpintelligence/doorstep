package queue

import (
	"errors"
	"regexp"
	"testing"
)

func TestWorkerID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		host     string
		err      error
		wantHost string
	}{
		{name: "hostname", host: "doorstep-7d9f", wantHost: "doorstep-7d9f"},
		{name: "padded hostname", host: " pod-1 \n", wantHost: "pod-1"},
		{name: "blank hostname", host: " ", wantHost: unknownHost},
		{name: "hostname error", err: errors.New("no hostname"), wantHost: unknownHost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			id, err := workerID(func() (string, error) { return tt.host, tt.err })
			if err != nil {
				t.Fatalf("workerID: %v", err)
			}
			pattern := regexp.MustCompile("^" + regexp.QuoteMeta(tt.wantHost) + "-[0-9a-f]{8}$")
			if !pattern.MatchString(id) {
				t.Errorf("workerID = %q, want %s-<8 hex>", id, tt.wantHost)
			}
		})
	}
}
