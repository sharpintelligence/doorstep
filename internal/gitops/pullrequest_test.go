package gitops

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const headBranch = "doorstep/blog-42"

func (f *fixture) assertMerged(t *testing.T, got Result, wantPull int, wantPrevious core.Digest) {
	t.Helper()
	head := f.mainSHA(t)
	if got.Commit != (core.Commit{Repository: "acme/gitops", SHA: head}) || !got.Changed ||
		got.PullRequest != wantPull || got.Previous != wantPrevious {
		t.Fatalf("Pin() = %+v, want merge commit %s of #%d with previous %q", got, head, wantPull, wantPrevious)
	}
	message := f.mainMessage(t)
	if !strings.HasPrefix(message, "deploy(blog): pin ghcr.io/acme/blog@sha256:bbbbbbbbbbbb\n\nDeploy ") {
		t.Fatalf("merge commit message = %q", message)
	}
	if trailers := ParseMessage(message); trailers.Deployment != 42 || trailers.Digest != digestB || trailers.PreviousDigest != wantPrevious {
		t.Fatalf("merge commit trailers = %+v", trailers)
	}
	if slices.Contains(f.repo.Branches(), headBranch) {
		t.Fatalf("head branch %s was not deleted", headBranch)
	}
	pulls := f.repo.Pulls()
	if last := pulls[len(pulls)-1]; !last.Merged || last.Number != wantPull || last.MergeCommitSHA != head {
		t.Fatalf("pull request = %+v", last)
	}
}

func TestPinPullRequestHappyPath(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	f.repo.Seed("main", kustomizationPath, kustomization(digestA))

	got, err := f.writer.Pin(t.Context(), pullRequestRequest())
	if err != nil {
		t.Fatalf("Pin() error = %v", err)
	}
	f.assertMerged(t, got, 1, digestA)
	if content := f.mainFile(t, kustomizationPath); content != kustomization(digestB) {
		t.Fatalf("file =\n%s", content)
	}
	pull := f.repo.Pulls()[0]
	if pull.Title != "deploy(blog): pin ghcr.io/acme/blog@sha256:bbbbbbbbbbbb" || pull.Head != headBranch || pull.Base != "main" ||
		ParseMessage(pull.Body).Deployment != 42 {
		t.Fatalf("pull request = %+v", pull)
	}

	again, err := f.writer.Pin(t.Context(), pullRequestRequest())
	if err != nil {
		t.Fatalf("second Pin() error = %v", err)
	}
	if again.Commit != got.Commit || again.Previous != digestA || again.Changed || again.PullRequest != 1 {
		t.Fatalf("second Pin() = %+v, want %+v unchanged", again, got)
	}
	if len(f.repo.Pulls()) != 1 {
		t.Fatalf("second Pin() opened another pull request: %+v", f.repo.Pulls())
	}
}

func TestPinPullRequestWaitsForChecks(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	f.repo.Seed("main", kustomizationPath, kustomization(""))
	f.repo.BlockMerges(3)
	merges := f.countPuts("/repos/acme/gitops/pulls/1/merge")

	got, err := f.writer.Pin(t.Context(), pullRequestRequest())
	if err != nil {
		t.Fatalf("Pin() error = %v", err)
	}
	f.assertMerged(t, got, 1, "")
	if n := merges.count.Load(); n != 4 {
		t.Fatalf("merge attempts = %d, want 4", n)
	}
}

func TestPinPullRequestResumes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, f *fixture)
	}{
		{
			name: "branch exists",
			setup: func(_ *testing.T, f *fixture) {
				f.repo.CreateBranch(headBranch, "main")
			},
		},
		{
			name: "file already written on the branch",
			setup: func(_ *testing.T, f *fixture) {
				f.repo.CreateBranch(headBranch, "main")
				f.repo.Commit(headBranch, "pin", map[string]string{kustomizationPath: kustomization(digestB)})
			},
		},
		{
			name: "branch built from an older base",
			setup: func(_ *testing.T, f *fixture) {
				f.repo.CreateBranch(headBranch, "main")
				f.repo.Commit(headBranch, "pin", map[string]string{kustomizationPath: kustomization(digestB)})
				f.repo.Commit("main", "other", map[string]string{"README.md": "hello"})
			},
		},
		{
			name: "pull request open",
			setup: func(t *testing.T, f *fixture) {
				t.Helper()
				f.repo.BlockMerges(1 << 30)
				short := New(f.app, Options{PollInterval: time.Millisecond, MergeTimeout: 500 * time.Millisecond})
				if _, err := short.Pin(t.Context(), pullRequestRequest()); !errors.Is(err, ErrMergeTimeout) {
					t.Fatalf("Pin() error = %v, want ErrMergeTimeout", err)
				}
				if pulls := f.repo.Pulls(); len(pulls) != 1 || pulls[0].State != "open" {
					t.Fatalf("pull requests after timeout = %+v", pulls)
				}
				f.repo.BlockMerges(0)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, Options{})
			f.repo.Seed("main", kustomizationPath, kustomization(digestA))
			tt.setup(t, f)

			got, err := f.writer.Pin(t.Context(), pullRequestRequest())
			if err != nil {
				t.Fatalf("Pin() error = %v", err)
			}
			f.assertMerged(t, got, 1, digestA)
			if content := f.mainFile(t, kustomizationPath); content != kustomization(digestB) {
				t.Fatalf("file =\n%s", content)
			}
		})
	}
}

func TestPinPullRequestUpdatesBehindBranch(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	f.repo.Seed("main", kustomizationPath, kustomization(digestA))
	f.repo.RequireUpToDate(true)
	var once atomic.Bool
	f.hook(func(method, path string) {
		if method == http.MethodPost && path == "/repos/acme/gitops/pulls" && once.CompareAndSwap(false, true) {
			f.repo.Commit("main", "docs", map[string]string{"README.md": "hello"})
		}
	})
	updates := f.countPuts("/repos/acme/gitops/pulls/1/update-branch")

	got, err := f.writer.Pin(t.Context(), pullRequestRequest())
	if err != nil {
		t.Fatalf("Pin() error = %v", err)
	}
	f.assertMerged(t, got, 1, digestA)
	if updates.count.Load() != 1 {
		t.Fatalf("update-branch calls = %d, want 1", updates.count.Load())
	}
	if readme, _ := f.repo.File("main", "README.md"); readme != "hello" {
		t.Fatalf("README.md = %q", readme)
	}
}

func TestPinPullRequestRecreatesOnConflict(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	f.repo.Seed("main", kustomizationPath, kustomization(""))
	var once atomic.Bool
	f.hook(func(method, path string) {
		if method == http.MethodPost && path == "/repos/acme/gitops/pulls" && once.CompareAndSwap(false, true) {
			f.repo.Commit("main", "manual pin", map[string]string{kustomizationPath: kustomization(digestD)})
		}
	})

	got, err := f.writer.Pin(t.Context(), pullRequestRequest())
	if err != nil {
		t.Fatalf("Pin() error = %v", err)
	}
	f.assertMerged(t, got, 2, digestD)
	first := f.repo.Pulls()[0]
	if first.State != "closed" || first.Merged || !strings.Contains(first.Body, supersededMarker) {
		t.Fatalf("first pull request = %+v", first)
	}
}

func TestPinPullRequestKeepsConflicting(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	f.repo.Seed("main", kustomizationPath, kustomization(""))
	f.repo.SetMergeableState("dirty")

	_, err := f.writer.Pin(t.Context(), pullRequestRequest())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Pin() error = %v, want ErrConflict", err)
	}
	if n := len(f.repo.Pulls()); n != maxRestarts+1 {
		t.Fatalf("pull requests = %d, want %d", n, maxRestarts+1)
	}
}

func TestPinPullRequestRewritesEditedFile(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	f.repo.Seed("main", kustomizationPath, kustomization(digestA))
	var once atomic.Bool
	f.hook(func(method, path string) {
		if method == http.MethodGet && path == "/repos/acme/gitops/pulls/1" && once.CompareAndSwap(false, true) {
			f.repo.Commit(headBranch, "sneaky", map[string]string{kustomizationPath: kustomization(digestD)})
		}
	})

	got, err := f.writer.Pin(t.Context(), pullRequestRequest())
	if err != nil {
		t.Fatalf("Pin() error = %v", err)
	}
	f.assertMerged(t, got, 1, digestA)
	if content := f.mainFile(t, kustomizationPath); content != kustomization(digestB) {
		t.Fatalf("file =\n%s", content)
	}
}

func TestPinPullRequestRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tamper  func(f *fixture)
		wantErr error
	}{
		{
			name:    "closed without merging",
			tamper:  func(f *fixture) { f.repo.ClosePull(1) },
			wantErr: ErrPullRequestClosed,
		},
		{
			name:    "head from a fork",
			tamper:  func(f *fixture) { f.repo.SetPullHeadRepo(1, "mallory/gitops") },
			wantErr: ErrPullRequestTampered,
		},
		{
			name: "extra file on the branch",
			tamper: func(f *fixture) {
				f.repo.Commit(headBranch, "extra", map[string]string{"apps/evil/deployment.yaml": "kind: Pod\n"})
			},
			wantErr: ErrPullRequestTampered,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, Options{})
			f.repo.Seed("main", kustomizationPath, kustomization(digestA))
			base := f.mainSHA(t)
			var once atomic.Bool
			f.hook(func(method, path string) {
				if method == http.MethodGet && path == "/repos/acme/gitops/pulls/1" && once.CompareAndSwap(false, true) {
					tt.tamper(f)
				}
			})

			if _, err := f.writer.Pin(t.Context(), pullRequestRequest()); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Pin() error = %v, want %v", err, tt.wantErr)
			}
			if head := f.mainSHA(t); head != base {
				t.Fatalf("base branch moved to %s", head)
			}
			if _, err := f.writer.Pin(t.Context(), pullRequestRequest()); !errors.Is(err, tt.wantErr) {
				t.Fatalf("second Pin() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestPinPullRequestMergedElsewhere(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	f.repo.Seed("main", kustomizationPath, kustomization(digestA))
	f.repo.BlockMerges(1 << 30)
	var once atomic.Bool
	f.hook(func(method, path string) {
		if method == http.MethodGet && path == "/repos/acme/gitops/pulls/1" && once.CompareAndSwap(false, true) {
			f.repo.Commit("main", "someone else pinned it", map[string]string{kustomizationPath: kustomization(digestB)})
		}
	})

	got, err := f.writer.Pin(t.Context(), pullRequestRequest())
	if err != nil {
		t.Fatalf("Pin() error = %v", err)
	}
	if got.Changed || got.Commit.SHA != f.mainSHA(t) || got.PullRequest != 0 {
		t.Fatalf("Pin() = %+v", got)
	}
	pulls := f.repo.Pulls()
	if len(pulls) != 1 || pulls[0].State != "closed" || pulls[0].Merged {
		t.Fatalf("pull requests = %+v", pulls)
	}
	if slices.Contains(f.repo.Branches(), headBranch) {
		t.Fatal("head branch was not deleted")
	}
}

func TestAbandon(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{PollInterval: time.Millisecond, MergeTimeout: 500 * time.Millisecond})
	f.repo.Seed("main", kustomizationPath, kustomization(digestA))
	f.repo.BlockMerges(1 << 30)
	if _, err := f.writer.Pin(t.Context(), pullRequestRequest()); !errors.Is(err, ErrMergeTimeout) {
		t.Fatalf("Pin() error = %v, want ErrMergeTimeout", err)
	}

	for range 2 {
		if err := f.writer.Abandon(t.Context(), pullRequestRequest()); err != nil {
			t.Fatalf("Abandon() error = %v", err)
		}
	}
	pulls := f.repo.Pulls()
	if len(pulls) != 1 || pulls[0].State != "closed" || pulls[0].Merged {
		t.Fatalf("pull requests = %+v", pulls)
	}
	if slices.Contains(f.repo.Branches(), headBranch) {
		t.Fatal("head branch was not deleted")
	}
	if content := f.mainFile(t, kustomizationPath); content != kustomization(digestA) {
		t.Fatalf("base changed:\n%s", content)
	}
	if _, err := f.writer.Pin(t.Context(), pullRequestRequest()); !errors.Is(err, ErrPullRequestClosed) {
		t.Fatalf("Pin() after Abandon error = %v, want ErrPullRequestClosed", err)
	}
	if err := f.writer.Abandon(t.Context(), deployRequest()); err != nil {
		t.Fatalf("Abandon() in commit mode error = %v", err)
	}
	if err := f.writer.Abandon(t.Context(), Request{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Abandon() of an invalid request error = %v", err)
	}
}

func TestPinPullRequestRespectsCallerDeadline(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	f.repo.Seed("main", kustomizationPath, kustomization(digestA))
	f.repo.BlockMerges(1 << 30)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	_, err := f.writer.Pin(ctx, pullRequestRequest())
	if errors.Is(err, ErrMergeTimeout) || ctx.Err() == nil {
		t.Fatalf("Pin() error = %v, want the caller's deadline", err)
	}
}
