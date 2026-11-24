package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestFlightGroupSharesOneCall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		val     string
		err     error
		wantVal string
	}{
		{name: "success", val: "v", wantVal: "v"},
		{name: "failure", err: errors.New("boom")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				g := flightGroup[string, string]{timeout: time.Minute}
				release := make(chan struct{})
				var calls atomic.Int64
				fn := func(context.Context) (string, error) {
					calls.Add(1)
					<-release
					return tt.val, tt.err
				}
				const callers = 20
				vals := make([]string, callers)
				errs := make([]error, callers)
				var wg sync.WaitGroup
				for i := range callers {
					wg.Go(func() { vals[i], errs[i] = g.do(t.Context(), "k", fn) })
				}
				synctest.Wait()
				close(release)
				wg.Wait()

				if got := calls.Load(); got != 1 {
					t.Errorf("calls = %d, want 1", got)
				}
				for i := range callers {
					if vals[i] != tt.wantVal || !errors.Is(errs[i], tt.err) {
						t.Errorf("caller %d = %q, %v, want %q, %v", i, vals[i], errs[i], tt.wantVal, tt.err)
					}
				}
				if _, err := g.do(t.Context(), "k", func(context.Context) (string, error) { return "again", nil }); err != nil {
					t.Errorf("call after completion: %v", err)
				}
				if got := calls.Load(); got != 1 {
					t.Errorf("finished flight was reused: calls = %d", got)
				}
			})
		})
	}
}

func TestFlightGroupCallerCancellationLeavesFlightRunning(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		g := flightGroup[string, string]{timeout: time.Minute}
		release := make(chan struct{})
		fn := func(ctx context.Context) (string, error) {
			select {
			case <-release:
				return "v", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}

		leaderCtx, cancel := context.WithCancel(t.Context())
		leader := make(chan error, 1)
		go func() {
			_, err := g.do(leaderCtx, "k", fn)
			leader <- err
		}()
		synctest.Wait()
		follower := make(chan string, 1)
		go func() {
			v, _ := g.do(t.Context(), "k", fn)
			follower <- v
		}()
		synctest.Wait()

		cancel()
		if err := <-leader; !errors.Is(err, context.Canceled) {
			t.Errorf("leader error = %v, want context.Canceled", err)
		}
		close(release)
		if v := <-follower; v != "v" {
			t.Errorf("follower = %q, want v", v)
		}
	})
}

func TestFlightGroupTimeout(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		g := flightGroup[string, string]{timeout: 30 * time.Second}
		start := time.Now()
		_, err := g.do(t.Context(), "k", func(ctx context.Context) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed != 30*time.Second {
			t.Errorf("elapsed = %s, want 30s", elapsed)
		}
	})
}

func TestAppSharesConcurrentRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		body    string
		call    func(context.Context, *App) (any, error)
		want    any
		wantErr string
	}{
		{
			name:   "token mint",
			status: http.StatusCreated,
			body:   `{"token":"ghs_shared","expires_at":"2099-01-01T00:00:00Z"}`,
			call:   func(ctx context.Context, a *App) (any, error) { return a.installationToken(ctx, 7) },
			want:   "ghs_shared",
		},
		{
			name:    "failed token mint",
			status:  http.StatusBadGateway,
			body:    `{"message":"upstream down"}`,
			call:    func(ctx context.Context, a *App) (any, error) { return a.installationToken(ctx, 7) },
			want:    "",
			wantErr: "502",
		},
		{
			name:   "installation lookup",
			status: http.StatusOK,
			body:   `{"id":11}`,
			call:   func(ctx context.Context, a *App) (any, error) { return a.installationID(ctx, "acme", "app") },
			want:   int64(11),
		},
		{
			name:    "failed installation lookup",
			status:  http.StatusBadGateway,
			body:    `{"message":"upstream down"}`,
			call:    func(ctx context.Context, a *App) (any, error) { return a.installationID(ctx, "acme", "app") },
			want:    int64(0),
			wantErr: "502",
		},
	}

	key := generateRSAKey(t, 2048)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				var requests atomic.Int64
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					requests.Add(1)
					<-release
					return &http.Response{
						StatusCode: tt.status,
						Header:     http.Header{},
						Body:       io.NopCloser(strings.NewReader(tt.body)),
						Request:    r,
					}, nil
				})}
				app, err := New(Options{AppID: "1", PrivateKey: key, BaseURL: "https://api.example.com", HTTPClient: client})
				if err != nil {
					t.Fatalf("New: %v", err)
				}

				const callers = 10
				got := make([]any, callers)
				errs := make([]error, callers)
				var wg sync.WaitGroup
				for i := range callers {
					wg.Go(func() { got[i], errs[i] = tt.call(t.Context(), app) })
				}
				synctest.Wait()
				close(release)
				wg.Wait()

				if n := requests.Load(); n != 1 {
					t.Errorf("requests = %d, want 1", n)
				}
				for i := range callers {
					if got[i] != tt.want {
						t.Errorf("caller %d = %v, want %v", i, got[i], tt.want)
					}
					if tt.wantErr == "" && errs[i] != nil || tt.wantErr != "" && (errs[i] == nil || !strings.Contains(errs[i].Error(), tt.wantErr)) {
						t.Errorf("caller %d error = %v, want %q", i, errs[i], tt.wantErr)
					}
				}
			})
		})
	}
}
