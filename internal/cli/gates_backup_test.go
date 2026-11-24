package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/notify"
)

func TestRegisterBackupGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		webhook    func(server string, secretFile string) *config.GateWebhook
		env        func(server string) map[string]string
		wantStatus core.GateStatus
		wantSigned bool
	}{
		{
			name: "url from env, secret from env",
			path: "/backup",
			webhook: func(string, string) *config.GateWebhook {
				return &config.GateWebhook{URLEnv: "BACKUP_URL", SecretEnv: "BACKUP_SECRET"}
			},
			env: func(server string) map[string]string {
				return map[string]string{"BACKUP_URL": server + "/backup", "BACKUP_SECRET": "s3cret"}
			},
			wantStatus: core.GatePassed,
			wantSigned: true,
		},
		{
			name: "literal url, secret from file",
			path: "/backup",
			webhook: func(server, file string) *config.GateWebhook {
				return &config.GateWebhook{URL: server + "/backup", SecretFile: file}
			},
			wantStatus: core.GatePassed,
			wantSigned: true,
		},
		{
			name:       "redirect is not followed",
			path:       "/moved",
			webhook:    func(server, _ string) *config.GateWebhook { return &config.GateWebhook{URL: server + "/moved"} },
			wantStatus: core.GateFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var (
				mu       sync.Mutex
				requests []*http.Request
				bodies   [][]byte
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				requests = append(requests, r)
				bodies = append(bodies, body)
				mu.Unlock()
				if r.URL.Path == "/moved" {
					http.Redirect(w, r, "/backup", http.StatusFound)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(server.Close)
			secretFile := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(secretFile, []byte("s3cret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{}
			if tt.env != nil {
				env = tt.env(server.URL)
			}
			r := gates.NewRegistry()
			now := func() time.Time { return time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC) }
			if err := registerBackupGate(r, mapEnv(env), "doorstep/test", now); err != nil {
				t.Fatalf("registerBackupGate: %v", err)
			}
			app := config.App{Name: "web", Gates: config.Gates{Before: []config.Gate{{
				Type: config.GateBackup, Name: "backup", When: config.GateWhenAlways,
				Webhook: tt.webhook(server.URL, secretFile),
			}}}}
			plan, err := r.Plan(app)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			d := core.Deployment{ID: 3, Target: core.AppTarget("web"), Kind: core.KindDeploy}
			ev, err := plan.Before[0].Gate.Evaluate(t.Context(), gates.Request{Deployment: d, Phase: core.GatePhaseBefore})
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if ev.Status != tt.wantStatus {
				t.Errorf("status = %s (%q), want %s", ev.Status, ev.Message, tt.wantStatus)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(requests) != 1 || requests[0].URL.Path != tt.path {
				t.Fatalf("requests = %d, want exactly one to %s", len(requests), tt.path)
			}
			sig := requests[0].Header.Get(gates.HeaderBackupSignature)
			if tt.wantSigned && sig != notify.Sign([]byte("s3cret"), bodies[0]) {
				t.Errorf("signature = %q, want notify.Sign over the body", sig)
			}
			if !tt.wantSigned && sig != "" {
				t.Errorf("unexpected signature %q", sig)
			}
			if ua := requests[0].Header.Get("User-Agent"); !strings.HasPrefix(ua, "doorstep") {
				t.Errorf("User-Agent = %q", ua)
			}
		})
	}
}

func mapEnv(env map[string]string) LookupEnv {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}
