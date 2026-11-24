package cli

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const (
	intakeTestSHA    = "89abcdef0123456789abcdef0123456789abcdef"
	intakeTestSecret = "webhook-s3cret"
)

const intakeConfig = `
apps:
  - name: blog
    source:
      repo: example-user/blog
      required_checks: [test]
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: apps/blog
`

func newIntakeGitHub(t *testing.T) *githubtest.Server {
	t.Helper()
	srv := githubtest.NewServer(t)
	srv.Install("example-user", "blog", 1)
	srv.SetBranch("example-user", "blog", "main", intakeTestSHA)
	srv.SetCheckRuns("example-user", "blog", intakeTestSHA, githubtest.CheckRun{ID: 1, Name: "test", Status: "completed", Conclusion: "success"})
	return srv
}

func githubEnv(srv *githubtest.Server, extra map[string]string) map[string]string {
	env := map[string]string{
		config.EnvGitHubAppID:         srv.AppID,
		config.EnvGitHubAppPrivateKey: string(srv.PrivateKeyPEM()),
		config.EnvGitHubAPIURL:        srv.URL,
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func postPush(t *testing.T, url, delivery, secret, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(github.EventHeader, "push")
	req.Header.Set(github.DeliveryHeader, delivery)
	req.Header.Set(github.SignatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func waitForTrackedCommit(t *testing.T, databaseURL string) core.TrackedCommit {
	t.Helper()
	st, err := store.Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		commits, err := st.TrackedCommits(t.Context(), "example-user/blog", intakeTestSHA)
		if err != nil {
			t.Fatalf("TrackedCommits: %v", err)
		}
		if len(commits) == 1 && commits[0].DeploymentID != nil {
			return commits[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no enqueued tracked commit after 10s: %+v", commits)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunServeIntake(t *testing.T) {
	t.Parallel()

	pushBody := `{"ref":"refs/heads/main","after":"` + intakeTestSHA + `","repository":{"full_name":"example-user/blog"},"sender":{"login":"octocat"}}`
	tests := []struct {
		name        string
		env         map[string]string
		wantLog     string
		wantWebhook bool
		wantSource  core.Source
		wantBy      string
	}{
		{
			name:        "webhooks only",
			env:         map[string]string{config.EnvGitHubWebhookSecret: intakeTestSecret, config.EnvPollInterval: "0s"},
			wantLog:     "github polling disabled, webhooks only",
			wantWebhook: true,
			wantSource:  core.SourceWebhook,
			wantBy:      "octocat",
		},
		{
			name:       "polling only",
			env:        map[string]string{config.EnvPollInterval: "10s"},
			wantLog:    "github webhooks disabled, polling only",
			wantSource: core.SourcePoll,
			wantBy:     "doorstep-poller",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := newIntakeGitHub(t)
			args, env := withDatabase(t, databaseURLEnv, []string{
				"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "", "--shutdown-delay", "0s",
				"--config", writeConfig(t, intakeConfig),
			}, githubEnv(srv, tt.env))
			var stdout bytes.Buffer
			var stderr lockedBuffer
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan int, 1)
			go func() { done <- Run(ctx, args, &stdout, &stderr, envFrom(env)) }()

			addr := waitForListening(t, &stderr, done)
			waitForLog(t, &stderr, done, tt.wantLog)
			url := "http://" + addr + "/webhooks/github"
			if tt.wantWebhook {
				if code, body := postPush(t, url, "d-0", "wrong", pushBody); code != http.StatusUnauthorized {
					t.Errorf("badly signed push = %d %s, want 401", code, body)
				}
				if code, body := postPush(t, url, "d-1", intakeTestSecret, pushBody); code != http.StatusOK || !strings.Contains(body, `"processed"`) {
					t.Errorf("signed push = %d %s, want processed", code, body)
				}
				if code, body := postPush(t, url, "d-1", intakeTestSecret, pushBody); code != http.StatusOK || !strings.Contains(body, `"duplicate"`) {
					t.Errorf("redelivered push = %d %s, want duplicate", code, body)
				}
			} else if code, _ := postPush(t, url, "d-2", intakeTestSecret, pushBody); code != http.StatusNotFound {
				t.Errorf("POST /webhooks/github without a secret = %d, want 404", code)
			}

			commit := waitForTrackedCommit(t, env[config.EnvDatabaseURL])
			if commit.Source != tt.wantSource || commit.PushedBy != tt.wantBy || commit.ChecksState != core.ChecksSuccess {
				t.Errorf("tracked commit = %+v, want %s by %s and green", commit, tt.wantSource, tt.wantBy)
			}
			_, metricsBody := get(t, "http://"+addr+"/metrics")
			if tt.wantWebhook && !strings.Contains(metricsBody, `doorstep_webhook_deliveries_total{event="push",result="processed"} 1`) {
				t.Errorf("metrics do not count the webhook delivery")
			}
			if !tt.wantWebhook && !strings.Contains(metricsBody, `doorstep_intake_poll_cycles_total{result="succeeded"}`) {
				t.Errorf("metrics do not count poll cycles")
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
			if strings.Contains(stderr.String(), intakeTestSecret) {
				t.Errorf("logs leak the webhook secret")
			}
			if !hasMessage(parseLogs(t, stderr.String()), "stopped") {
				t.Errorf("logs missing stopped: %s", stderr.String())
			}
		})
	}
}

func TestRunServeIntakeDisabled(t *testing.T) {
	t.Parallel()

	srv := newIntakeGitHub(t)
	args, env := withDatabase(t, databaseURLEnv, []string{
		"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "127.0.0.1:0", "--shutdown-delay", "0s",
		"--config", writeConfig(t, intakeConfig),
	}, githubEnv(srv, map[string]string{config.EnvPollInterval: "0"}))
	var stdout bytes.Buffer
	var stderr lockedBuffer
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- Run(ctx, args, &stdout, &stderr, envFrom(env)) }()

	entry := waitForLog(t, &stderr, done, "github webhooks and polling are both disabled: new commits are not detected")
	if entry["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", entry["level"])
	}
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit code = %d, stderr %q", code, stderr.String())
	}
	if srv.BranchCalls() != 0 {
		t.Errorf("branch reads = %d, want none with polling disabled", srv.BranchCalls())
	}
}

func TestRunServeWebhookSecretErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	emptyFile := filepath.Join(dir, "empty")
	if err := os.WriteFile(emptyFile, []byte("\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	tests := []struct {
		name    string
		env     map[string]string
		github  bool
		wantErr string
	}{
		{
			name:    "empty secret file",
			env:     map[string]string{config.EnvGitHubWebhookSecretFile: emptyFile},
			github:  true,
			wantErr: "intake: github webhook secret: file " + emptyFile + " is empty",
		},
		{
			name:    "missing secret file",
			env:     map[string]string{config.EnvGitHubWebhookSecretFile: filepath.Join(dir, "missing")},
			github:  true,
			wantErr: "intake: read github webhook secret",
		},
		{
			name:    "secret without the app",
			env:     map[string]string{config.EnvGitHubWebhookSecret: intakeTestSecret},
			wantErr: "github webhook secret: requires the GitHub App",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := tt.env
			if tt.github {
				env = githubEnv(newIntakeGitHub(t), tt.env)
			}
			args, env := withDatabase(t, databaseURLEnv, []string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "127.0.0.1:0"}, env)
			var stdout, stderr bytes.Buffer
			if code := Run(t.Context(), args, &stdout, &stderr, envFrom(env)); code != 1 {
				t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), tt.wantErr)
			}
			if strings.Contains(stderr.String(), intakeTestSecret) {
				t.Errorf("stderr leaks the secret: %q", stderr.String())
			}
		})
	}
}
