package gitops

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
)

func TestPinCommitFirstPin(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{DeploymentURL: func(id core.DeploymentID) string {
		return "https://doorstep.example/deployments/" + strconv.FormatInt(int64(id), 10)
	}})
	f.repo.Seed("main", kustomizationPath, kustomization(""))

	got, err := f.writer.Pin(t.Context(), deployRequest())
	if err != nil {
		t.Fatalf("Pin() error = %v", err)
	}
	want := Result{Commit: core.Commit{Repository: "acme/gitops", SHA: f.mainSHA(t)}, Changed: true}
	if got.Commit != want.Commit || got.Previous != "" || !got.Changed || got.PullRequest != 0 {
		t.Fatalf("Pin() = %+v, want %+v", got, want)
	}
	if content := f.mainFile(t, kustomizationPath); content != kustomization(digestB) {
		t.Fatalf("file =\n%s\nwant\n%s", content, kustomization(digestB))
	}
	message := f.mainMessage(t)
	if !strings.HasPrefix(message, "deploy(blog): pin ghcr.io/acme/blog@sha256:bbbbbbbbbbbb\n\n") {
		t.Fatalf("message subject = %q", message)
	}
	trailers := ParseMessage(message)
	if trailers.Deployment != 42 || trailers.Digest != digestB || trailers.PreviousDigest != "" ||
		trailers.URL != "https://doorstep.example/deployments/42" {
		t.Fatalf("trailers = %+v", trailers)
	}
}

func TestPinCommitRepinReturnsPrevious(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	f.repo.Seed("main", kustomizationPath, kustomization(digestA))

	got, err := f.writer.Pin(t.Context(), deployRequest())
	if err != nil {
		t.Fatalf("Pin() error = %v", err)
	}
	if got.Previous != digestA || !got.Changed {
		t.Fatalf("Pin() = %+v, want previous %s", got, digestA)
	}
	if trailers := ParseMessage(f.mainMessage(t)); trailers.PreviousDigest != digestA {
		t.Fatalf("Doorstep-Previous-Digest = %q", trailers.PreviousDigest)
	}
}

func TestPinCommitResumeAfterWrite(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	f.repo.Seed("main", kustomizationPath, kustomization(digestA))
	first, err := f.writer.Pin(t.Context(), deployRequest())
	if err != nil {
		t.Fatalf("first Pin() error = %v", err)
	}
	other := f.repo.Commit("main", "deploy(api): pin", map[string]string{
		kustomizationPath: withAPIDigest(kustomization(digestB), digestD),
	})

	again, err := f.writer.Pin(t.Context(), deployRequest())
	if err != nil {
		t.Fatalf("second Pin() error = %v", err)
	}
	if again.Commit != first.Commit || again.Previous != digestA || again.Changed {
		t.Fatalf("second Pin() = %+v, want commit %s with previous %s", again, first.Commit.SHA, digestA)
	}
	if head := f.mainSHA(t); head != other {
		t.Fatalf("second Pin() wrote a commit: head %s, want %s", head, other)
	}
}

func TestPinCommitAlreadyCurrent(t *testing.T) {
	t.Parallel()

	earlier := deployRequest()
	earlier.Deployment = 41
	otherApp := deployRequest()
	otherApp.Target.App = "api"
	tests := []struct {
		name       string
		setup      func(t *testing.T, f *fixture) string
		wantCommit bool
	}{
		{
			name: "set by hand",
			setup: func(_ *testing.T, f *fixture) string {
				f.repo.Seed("main", kustomizationPath, kustomization(digestB))
				return ""
			},
		},
		{
			name: "pinned by an earlier deployment of the same digest",
			setup: func(t *testing.T, f *fixture) string {
				t.Helper()
				f.repo.Seed("main", kustomizationPath, kustomization(digestA))
				sha := f.repo.Commit("main", pinMessage(t, earlier, digestA), map[string]string{kustomizationPath: kustomization(digestB)})
				f.repo.Commit("main", "deploy(api)", map[string]string{kustomizationPath: withAPIDigest(kustomization(digestB), digestD)})
				return sha
			},
			wantCommit: true,
		},
		{
			name: "newest commit belongs to another app",
			setup: func(t *testing.T, f *fixture) string {
				t.Helper()
				f.repo.Seed("main", kustomizationPath, kustomization(digestB))
				f.repo.Commit("main", pinMessage(t, otherApp, digestC), map[string]string{kustomizationPath: withAPIDigest(kustomization(digestB), digestB)})
				return ""
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, Options{})
			sha := tt.setup(t, f)
			head := f.mainSHA(t)

			got, err := f.writer.Pin(t.Context(), deployRequest())
			if err != nil {
				t.Fatalf("Pin() error = %v", err)
			}
			want := Result{}
			if tt.wantCommit {
				want.Commit = core.Commit{Repository: "acme/gitops", SHA: sha}
			}
			if got != want {
				t.Fatalf("Pin() = %+v, want %+v", got, want)
			}
			if f.mainSHA(t) != head {
				t.Fatal("Pin() wrote a commit")
			}
		})
	}
}

func TestPinCommitDoesNotReplayOverwrittenPin(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	f.repo.Seed("main", kustomizationPath, kustomization(digestA))
	if _, err := f.writer.Pin(t.Context(), deployRequest()); err != nil {
		t.Fatalf("first Pin() error = %v", err)
	}
	newer := deployRequest()
	newer.Deployment = 43
	newer.Digest = digestD
	if _, err := f.writer.Pin(t.Context(), newer); err != nil {
		t.Fatalf("newer Pin() error = %v", err)
	}
	head := f.mainSHA(t)
	puts := f.countPuts("/repos/acme/gitops/contents/")

	if _, err := f.writer.Pin(t.Context(), deployRequest()); !errors.Is(err, ErrPinOverwritten) {
		t.Fatalf("replayed Pin() error = %v, want ErrPinOverwritten", err)
	}
	if puts.count.Load() != 0 || f.mainSHA(t) != head {
		t.Fatal("replayed Pin() wrote the old digest back")
	}
}

func TestPinCommitRetriesConcurrentEdit(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	f.repo.Seed("main", kustomizationPath, kustomization(digestA))
	puts := f.countPuts("/repos/acme/gitops/contents/")
	f.hook(func(method, _ string) {
		if method == http.MethodPut && puts.count.Load() == 1 {
			f.repo.Commit("main", "deploy(api)", map[string]string{kustomizationPath: withAPIDigest(kustomization(digestA), digestD)})
		}
	})

	got, err := f.writer.Pin(t.Context(), deployRequest())
	if err != nil {
		t.Fatalf("Pin() error = %v", err)
	}
	if puts.count.Load() != 2 {
		t.Fatalf("PUT calls = %d, want 2", puts.count.Load())
	}
	if want := withAPIDigest(kustomization(digestB), digestD); f.mainFile(t, kustomizationPath) != want {
		t.Fatalf("file =\n%s\nwant\n%s", f.mainFile(t, kustomizationPath), want)
	}
	if got.Previous != digestA || got.Commit.SHA != f.mainSHA(t) {
		t.Fatalf("Pin() = %+v", got)
	}
}

func TestPinCommitErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(f *fixture)
		mutate    func(r *Request)
		opts      Options
		wantErr   error
		wantNot   error
		wantPuts  int64
		wantState int
	}{
		{
			name: "conflicts exhaust the attempts",
			opts: Options{MaxAttempts: 3},
			setup: func(f *fixture) {
				f.repo.Seed("main", kustomizationPath, kustomization(digestA))
				n := 0
				f.hook(func(method, _ string) {
					if method == http.MethodPut {
						n++
						f.repo.Commit("main", "touch", map[string]string{kustomizationPath: kustomization(digestA) + "# touch " + strconv.Itoa(n) + "\n"})
					}
				})
			},
			wantErr:   ErrConflict,
			wantPuts:  3,
			wantState: http.StatusConflict,
		},
		{
			name: "protected branch is not retried",
			setup: func(f *fixture) {
				f.repo.Seed("main", kustomizationPath, kustomization(digestA))
				f.repo.RejectPut(http.StatusConflict, "Changes must be made through a pull request.")
			},
			wantErr:   ErrBranchProtected,
			wantNot:   ErrConflict,
			wantPuts:  1,
			wantState: http.StatusConflict,
		},
		{
			name: "ruleset violation is not retried",
			setup: func(f *fixture) {
				f.repo.Seed("main", kustomizationPath, kustomization(digestA))
				f.repo.RejectPut(http.StatusConflict, "Repository rule violations found\n\nGH013: Cannot update this protected ref.")
			},
			wantErr:   ErrBranchProtected,
			wantNot:   ErrConflict,
			wantPuts:  1,
			wantState: http.StatusConflict,
		},
		{
			name: "protected branch validation failure",
			setup: func(f *fixture) {
				f.repo.Seed("main", kustomizationPath, kustomization(digestA))
				f.repo.RejectPut(http.StatusUnprocessableEntity, "Invalid request.\n\nbranch is protected")
			},
			wantErr:   ErrBranchProtected,
			wantPuts:  1,
			wantState: http.StatusUnprocessableEntity,
		},
		{
			name: "unexplained conflict is not retried",
			setup: func(f *fixture) {
				f.repo.Seed("main", kustomizationPath, kustomization(digestA))
				f.repo.RejectPut(http.StatusConflict, "Git Repository is empty.")
			},
			wantNot:   ErrConflict,
			wantPuts:  1,
			wantState: http.StatusConflict,
		},
		{
			name: "stale sha is retried",
			opts: Options{MaxAttempts: 2},
			setup: func(f *fixture) {
				f.repo.Seed("main", kustomizationPath, kustomization(digestA))
				f.repo.RejectPut(http.StatusConflict, "is at 1111 but expected 2222")
			},
			wantErr:   ErrConflict,
			wantPuts:  2,
			wantState: http.StatusConflict,
		},
		{
			name: "forbidden is not retried",
			setup: func(f *fixture) {
				f.repo.Seed("main", kustomizationPath, kustomization(digestA))
				f.repo.RejectPut(http.StatusForbidden, "Resource not accessible by integration")
			},
			wantNot:   ErrConflict,
			wantPuts:  1,
			wantState: http.StatusForbidden,
		},
		{
			name: "unrelated validation failure is not retried",
			setup: func(f *fixture) {
				f.repo.Seed("main", kustomizationPath, kustomization(digestA))
				f.repo.RejectPut(http.StatusUnprocessableEntity, "Invalid request.\n\nmessage is too long")
			},
			wantNot:   ErrConflict,
			wantPuts:  1,
			wantState: http.StatusUnprocessableEntity,
		},
		{
			name:    "file missing",
			setup:   func(f *fixture) { f.repo.Seed("main", "README.md", "hi") },
			wantErr: ErrPinNotFound,
		},
		{
			name:    "branch missing",
			setup:   func(f *fixture) { f.repo.Seed("main", kustomizationPath, kustomization("")) },
			mutate:  func(r *Request) { r.Target.Branch = "prod" },
			wantErr: github.ErrRefNotFound,
			wantNot: ErrPinNotFound,
		},
		{
			name:    "entry missing",
			setup:   func(f *fixture) { f.repo.Seed("main", kustomizationPath, kustomization("")) },
			mutate:  func(r *Request) { r.Target.Kustomize.Name = "ghcr.io/acme/typo" },
			wantErr: ErrPinNotFound,
		},
		{
			name:    "oversize file",
			setup:   func(f *fixture) { f.repo.Seed("main", kustomizationPath, strings.Repeat("#", github.MaxFileSize+1)) },
			wantErr: github.ErrFileTooLarge,
		},
		{
			name:    "directory instead of a file",
			setup:   func(f *fixture) { f.repo.Seed("main", kustomizationPath+"/x", "a") },
			wantErr: github.ErrNotAFile,
		},
		{
			name: "unsupported yaml",
			setup: func(f *fixture) {
				f.repo.Seed("main", kustomizationPath, "images:\n- name: ghcr.io/acme/blog\n  digest: |\n    x\n")
			},
			wantErr: ErrUnsupportedYAML,
		},
		{
			name:    "invalid request",
			mutate:  func(r *Request) { r.Digest = "latest" },
			wantErr: ErrInvalidRequest,
		},
		{
			name:    "not installed",
			mutate:  func(r *Request) { r.Target.Owner = "other" },
			wantErr: github.ErrNotInstalled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, tt.opts)
			if tt.setup != nil {
				tt.setup(f)
			}
			puts := f.countPuts("/repos/acme/gitops/contents/")
			req := deployRequest()
			if tt.mutate != nil {
				tt.mutate(&req)
			}
			_, err := f.writer.Pin(t.Context(), req)
			if err == nil {
				t.Fatal("Pin() succeeded")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("Pin() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantNot != nil && errors.Is(err, tt.wantNot) {
				t.Fatalf("Pin() error = %v, must not be %v", err, tt.wantNot)
			}
			if got := puts.count.Load(); got != tt.wantPuts {
				t.Fatalf("PUT calls = %d, want %d", got, tt.wantPuts)
			}
			if tt.wantState != 0 && !github.HasStatus(err, tt.wantState) {
				t.Fatalf("Pin() error = %v, want status %d", err, tt.wantState)
			}
		})
	}
}

func TestPinCommitKustomizationFallback(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"kustomization.yml", "Kustomization"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, Options{})
			path := "apps/blog/" + name
			f.repo.Seed("main", path, kustomization(digestA))
			if _, err := f.writer.Pin(t.Context(), deployRequest()); err != nil {
				t.Fatalf("Pin() error = %v", err)
			}
			if got := f.mainFile(t, path); got != kustomization(digestB) {
				t.Fatalf("file =\n%s", got)
			}
			pinned, err := f.writer.Current(t.Context(), deployRequest().Target)
			if err != nil || pinned.File != path {
				t.Fatalf("Current() = %+v, %v", pinned, err)
			}
		})
	}
}

func TestPinCommitCancelledDuringBackoff(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waits := 0
	f := newFixture(t, Options{Wait: func(ctx context.Context, d time.Duration) error {
		waits++
		if d < initialBackoff/2 || d > initialBackoff {
			t.Errorf("first backoff = %s, want between %s and %s", d, initialBackoff/2, initialBackoff)
		}
		cancel()
		return ctx.Err()
	}})
	f.repo.Seed("main", kustomizationPath, kustomization(digestA))
	f.repo.RejectPut(http.StatusConflict, "is at x but expected y")

	_, err := f.writer.Pin(ctx, deployRequest())
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrConflict) {
		t.Fatalf("Pin() error = %v, want context.Canceled", err)
	}
	if waits != 1 {
		t.Fatalf("waits = %d, want 1", waits)
	}
}

func TestBackoff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 0, want: initialBackoff},
		{attempt: 1, want: initialBackoff},
		{attempt: 2, want: 2 * initialBackoff},
		{attempt: 6, want: 32 * initialBackoff},
		{attempt: 7, want: maxBackoff},
		{attempt: 40, want: maxBackoff},
		{attempt: 100, want: maxBackoff},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.attempt), func(t *testing.T) {
			t.Parallel()
			for range 20 {
				if d := backoff(tt.attempt); d < tt.want/2 || d > tt.want {
					t.Fatalf("backoff(%d) = %s, want between %s and %s", tt.attempt, d, tt.want/2, tt.want)
				}
			}
		})
	}
}

func pinMessage(t *testing.T, req Request, previous core.Digest) string {
	t.Helper()
	subject, body, err := FormatMessage(req, previous, "")
	if err != nil {
		t.Fatalf("FormatMessage() error = %v", err)
	}
	return subject + "\n\n" + body
}

func TestPinCommitYAMLReference(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	manifest := "apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n        - name: blog\n          image: ghcr.io/acme/blog:1.0\n"
	f.repo.Seed("main", "apps/blog/deployment.yaml", manifest)
	req := deployRequest()
	req.Target.Kustomize = nil
	req.Target.YAML = &YAMLValue{File: "deployment.yaml", Path: []string{"spec", "template", "spec", "containers", "0", "image"}, Format: FormatReference}

	before, err := f.writer.Current(t.Context(), req.Target)
	if err != nil || before.Digest != "" || before.Value != "ghcr.io/acme/blog:1.0" {
		t.Fatalf("Current() before = %+v, %v", before, err)
	}
	if _, err := f.writer.Pin(t.Context(), req); err != nil {
		t.Fatalf("Pin() error = %v", err)
	}
	want := strings.Replace(manifest, "ghcr.io/acme/blog:1.0", "ghcr.io/acme/blog@"+string(digestB), 1)
	if got := f.mainFile(t, "apps/blog/deployment.yaml"); got != want {
		t.Fatalf("file =\n%s\nwant\n%s", got, want)
	}
	after, err := f.writer.Current(t.Context(), req.Target)
	if err != nil || after.Digest != digestB || after.File != "apps/blog/deployment.yaml" {
		t.Fatalf("Current() after = %+v, %v", after, err)
	}
}
