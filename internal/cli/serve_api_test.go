package cli

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRunServeAPI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		metricsAddr  string
		wantSeparate bool
	}{
		{name: "separate metrics listener", metricsAddr: "127.0.0.1:0", wantSeparate: true},
		{name: "metrics on the main listener", metricsAddr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			var stderr lockedBuffer
			args, env := withDatabase(t, databaseURLEnv, []string{
				"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", tt.metricsAddr, "--shutdown-delay", "0s",
			}, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			done := make(chan int, 1)
			go func() { done <- Run(ctx, args, &stdout, &stderr, envFrom(env)) }()

			mainAddr := waitForListening(t, &stderr, done)
			code, body := get(t, "http://"+mainAddr+"/api/v1/deployments/1")
			if code != http.StatusNotFound || !strings.Contains(body, `"code":"not_found"`) {
				t.Errorf("GET /api/v1/deployments/1 = %d %q, want 404 not_found", code, body)
			}
			code, body = get(t, "http://"+mainAddr+"/api/v1/apps/web/deployments?environment=prod")
			if code != http.StatusOK || strings.TrimSpace(body) != `{"items":[]}` {
				t.Errorf("GET /api/v1/apps/web/deployments = %d %q, want an empty list", code, body)
			}
			code, _ = get(t, "http://"+mainAddr+"/metrics")
			if wantMetrics := !tt.wantSeparate; (code == http.StatusOK) != wantMetrics {
				t.Errorf("main GET /metrics = %d, metrics expected on main listener: %v", code, wantMetrics)
			}
			if tt.wantSeparate {
				metricsAddr := waitForLogAddr(t, &stderr, done, "metrics listening")
				if code, _ := get(t, "http://"+metricsAddr+"/api/v1/deployments/1"); code != http.StatusNotFound {
					t.Errorf("metrics listener GET /api/v1/deployments/1 = %d, want 404", code)
				}
				if code, body := get(t, "http://"+metricsAddr+"/api/v1/apps/web/deployments"); code != http.StatusNotFound || strings.Contains(body, "items") {
					t.Errorf("metrics listener serves the API: %d %q", code, body)
				}
			}

			cancel()
			select {
			case code := <-done:
				if code != 0 {
					t.Errorf("exit code = %d, stderr %q", code, stderr.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("serve did not exit after cancellation")
			}
		})
	}
}
