package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
)

func TestInstallationTokenLifetime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		advance        time.Duration
		wantSame       bool
		wantTokenCalls int64
	}{
		{name: "reused well before expiry", advance: 30 * time.Minute, wantSame: true, wantTokenCalls: 1},
		{name: "reused just outside the margin", advance: 55*time.Minute - time.Second, wantSame: true, wantTokenCalls: 1},
		{name: "refreshed at the margin", advance: 55 * time.Minute, wantTokenCalls: 2},
		{name: "refreshed inside the margin", advance: 56 * time.Minute, wantTokenCalls: 2},
		{name: "refreshed after expiry", advance: 2 * time.Hour, wantTokenCalls: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.srv.Install("acme", "app", 7)
			first, err := f.app.installationToken(t.Context(), 7)
			if err != nil {
				t.Fatalf("first token: %v", err)
			}
			f.clock.Advance(tt.advance)
			second, err := f.app.installationToken(t.Context(), 7)
			if err != nil {
				t.Fatalf("second token: %v", err)
			}
			if (first == second) != tt.wantSame {
				t.Errorf("tokens equal = %t, want %t", first == second, tt.wantSame)
			}
			if got := f.srv.TokenCalls(); got != tt.wantTokenCalls {
				t.Errorf("token calls = %d, want %d", got, tt.wantTokenCalls)
			}
		})
	}
}

func TestInstallationTokenSeparatePerInstallation(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Install("acme", "app", 1)
	f.srv.Install("alice", "dotfiles", 2)
	acme, err := f.app.installationToken(t.Context(), 1)
	if err != nil {
		t.Fatalf("token 1: %v", err)
	}
	alice, err := f.app.installationToken(t.Context(), 2)
	if err != nil {
		t.Fatalf("token 2: %v", err)
	}
	if acme == alice {
		t.Error("installations share a token")
	}
	if got := f.srv.TokenCalls(); got != 2 {
		t.Errorf("token calls = %d, want 2", got)
	}
}

func TestInstallationTokenConcurrentCallersMintOnce(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Install("acme", "app", 7)
	const callers = 50
	tokens := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			tokens[i], errs[i] = f.app.installationToken(t.Context(), 7)
		})
	}
	wg.Wait()
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if tokens[i] != tokens[0] {
			t.Errorf("caller %d got a different token", i)
		}
	}
	if got := f.srv.TokenCalls(); got != 1 {
		t.Errorf("token calls = %d, want 1", got)
	}
}

func TestInstallationTokenWaiterHonoursContext(t *testing.T) {
	t.Parallel()

	key := generateRSAKey(t, 2048)
	entered := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		githubtest.WriteJSON(w, http.StatusCreated, map[string]string{"token": "ghs_slow", "expires_at": "2099-01-01T00:00:00Z"})
	}))
	t.Cleanup(upstream.Close)
	app, err := New(Options{AppID: "1", PrivateKey: key, BaseURL: upstream.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	minted := make(chan error, 1)
	go func() {
		_, err := app.installationToken(context.Background(), 7)
		minted <- err
	}()
	<-entered

	ctx, cancel := context.WithCancel(t.Context())
	waited := make(chan error, 1)
	go func() {
		_, err := app.installationToken(ctx, 7)
		waited <- err
	}()
	cancel()
	select {
	case err := <-waited:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waiter error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not return after cancellation")
	}

	close(release)
	if err := <-minted; err != nil {
		t.Fatalf("minting caller: %v", err)
	}
	token, err := app.installationToken(t.Context(), 7)
	if err != nil || token != "ghs_slow" {
		t.Errorf("cached token = %q, %v", token, err)
	}
}

func TestInstallationTokenMintNotFoundEvictsLookups(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Install("acme", "app", 7)
	f.srv.Install("acme", "gitops", 7)
	f.srv.Install("alice", "dotfiles", 8)
	for _, repo := range [][2]string{{"acme", "app"}, {"acme", "gitops"}, {"alice", "dotfiles"}} {
		f.repoClient(t, repo[0], repo[1])
	}
	f.srv.Uninstall(7)
	f.srv.Install("acme", "app", 9)

	if _, err := f.app.installationToken(t.Context(), 7); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("token error = %v, want ErrNotInstalled", err)
	}
	f.app.mu.Lock()
	remaining := len(f.app.installations)
	_, hasToken := f.app.tokens[7]
	f.app.mu.Unlock()
	if remaining != 1 || hasToken {
		t.Errorf("after eviction: %d cached lookups (want 1), token entry present %t", remaining, hasToken)
	}

	id, err := f.app.installationID(t.Context(), "acme", "app")
	if err != nil || id != 9 {
		t.Errorf("reinstalled lookup = %d, %v, want 9", id, err)
	}
	if got := f.srv.LookupCalls(); got != 4 {
		t.Errorf("lookup calls = %d, want 4", got)
	}
}

func TestInstallationTokenMintFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
		wantAPI int
	}{
		{name: "missing expires_at", status: http.StatusCreated, body: `{"token":"ghs_x"}`, wantErr: "no expires_at"},
		{name: "missing token", status: http.StatusCreated, body: `{"expires_at":"2099-01-01T00:00:00Z"}`, wantErr: "no token"},
		{name: "malformed json", status: http.StatusCreated, body: `{`, wantErr: "decode"},
		{name: "forbidden", status: http.StatusForbidden, body: `{"message":"This installation has been suspended"}`, wantErr: "suspended", wantAPI: http.StatusForbidden},
		{name: "server error", status: http.StatusBadGateway, body: `oops`, wantErr: "502", wantAPI: http.StatusBadGateway},
	}

	key := generateRSAKey(t, 2048)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(upstream.Close)
			app, err := New(Options{AppID: "1", PrivateKey: key, BaseURL: upstream.URL})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = app.installationToken(t.Context(), 7)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
			var apiErr *APIError
			if tt.wantAPI != 0 && (!errors.As(err, &apiErr) || apiErr.StatusCode != tt.wantAPI) {
				t.Errorf("error %v is not an APIError with status %d", err, tt.wantAPI)
			}
			app.mu.Lock()
			cached := app.tokens[7].value
			app.mu.Unlock()
			if cached != "" {
				t.Errorf("failed mint cached token %q", cached)
			}
		})
	}
}

func TestInvalidateToken(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Install("acme", "app", 7)
	token, err := f.app.installationToken(t.Context(), 7)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	f.app.invalidateToken(7, "ghs_stale")
	if again, _ := f.app.installationToken(t.Context(), 7); again != token {
		t.Error("invalidating a different value dropped the cached token")
	}
	f.app.invalidateToken(7, token)
	f.app.invalidateToken(99, token)
	if again, _ := f.app.installationToken(t.Context(), 7); again == token {
		t.Error("invalidated token was reused")
	}
	if got := f.srv.TokenCalls(); got != 2 {
		t.Errorf("token calls = %d, want 2", got)
	}
}
