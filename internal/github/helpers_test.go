package github

import (
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fixture struct {
	srv   *githubtest.Server
	clock *fakeClock
	app   *App
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	srv := githubtest.NewServer(t)
	clock := newFakeClock()
	srv.SetNow(clock.Now)
	return &fixture{srv: srv, clock: clock, app: newApp(t, srv, srv.URL, clock)}
}

func newApp(t *testing.T, srv *githubtest.Server, baseURL string, clock *fakeClock) *App {
	t.Helper()
	app, err := New(Options{
		AppID:      srv.AppID,
		PrivateKey: srv.PrivateKey,
		BaseURL:    baseURL,
		UserAgent:  "doorstep/test",
		Now:        clock.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return app
}

func (f *fixture) repoClient(t *testing.T, owner, repo string) *Client {
	t.Helper()
	client, err := f.app.RepoClient(t.Context(), owner, repo)
	if err != nil {
		t.Fatalf("RepoClient(%s, %s): %v", owner, repo, err)
	}
	return client
}
