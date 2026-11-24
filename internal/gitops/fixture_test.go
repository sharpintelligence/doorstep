package gitops

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
)

const (
	kustomizationPath = "apps/blog/kustomization.yaml"
	installationID    = 7
)

var _ RepoOpener = (*github.App)(nil)

type fixture struct {
	srv    *githubtest.Server
	repo   *githubtest.Repo
	app    *github.App
	writer *Writer

	mu    sync.Mutex
	hooks []func(method, path string)
}

func (f *fixture) hook(fn func(method, path string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hooks = append(f.hooks, fn)
}

func (f *fixture) countPuts(path string) *requestCounter {
	c := &requestCounter{path: path}
	f.hook(func(method, p string) {
		if method == http.MethodPut && strings.HasPrefix(p, c.path) {
			c.count.Add(1)
		}
	})
	return c
}

func (f *fixture) dispatch(method, path string) {
	f.mu.Lock()
	hooks := slices.Clone(f.hooks)
	f.mu.Unlock()
	for _, fn := range hooks {
		fn(method, path)
	}
}

func newFixture(t *testing.T, opts Options) *fixture {
	t.Helper()
	srv := githubtest.NewServer(t)
	repo := srv.AddRepo("acme", "gitops", installationID)
	app, err := github.New(github.Options{AppID: srv.AppID, PrivateKey: srv.PrivateKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = time.Millisecond
	}
	if opts.Wait == nil {
		opts.Wait = func(ctx context.Context, d time.Duration) error {
			return sleep(ctx, min(d, time.Millisecond))
		}
	}
	f := &fixture{srv: srv, repo: repo, app: app, writer: New(app, opts)}
	repo.OnRequest(f.dispatch)
	return f
}

func kustomization(blogDigest core.Digest) string {
	var b strings.Builder
	b.WriteString("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nimages:\n  - name: ghcr.io/acme/blog\n")
	if blogDigest != "" {
		b.WriteString("    digest: " + string(blogDigest) + "\n")
	}
	b.WriteString("    newTag: v1\n  - name: ghcr.io/acme/api\n    digest: " + string(digestC) + "\nresources:\n  - deployment.yaml\n")
	return b.String()
}

func withAPIDigest(content string, d core.Digest) string {
	return strings.Replace(content, "digest: "+string(digestC), "digest: "+string(d), 1)
}

func pullRequestRequest() Request {
	req := deployRequest()
	req.Target.Mode = ModePullRequest
	return req
}

func (f *fixture) mainMessage(t *testing.T) string {
	t.Helper()
	commits := f.repo.Commits("main")
	if len(commits) == 0 {
		t.Fatal("main has no commits")
	}
	return commits[0].Message
}

func (f *fixture) mainSHA(t *testing.T) string {
	t.Helper()
	sha, ok := f.repo.BranchSHA("main")
	if !ok {
		t.Fatal("main does not exist")
	}
	return sha
}

func (f *fixture) mainFile(t *testing.T, path string) string {
	t.Helper()
	content, ok := f.repo.File("main", path)
	if !ok {
		t.Fatalf("%s does not exist on main", path)
	}
	return content
}

type requestCounter struct {
	path  string
	count atomic.Int64
}
