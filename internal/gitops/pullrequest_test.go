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
	"github.com/sharpintelligence/doorstep/internal/github"
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

func TestPinPullRequestResetsStaleBranch(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	f.repo.Seed("main", kustomizationPath, kustomization(digestA))
	f.repo.CreateBranch(headBranch, "main")
	f.repo.Commit(headBranch, "stale", map[string]string{kustomizationPath: kustomization(digestD)})
	f.repo.Commit("main", "deploy(api)", map[string]string{kustomizationPath: withAPIDigest(kustomization(digestA), digestD)})

	got, err := f.writer.Pin(t.Context(), pullRequestRequest())
	if err != nil {
		t.Fatalf("Pin() error = %v", err)
	}
	f.assertMerged(t, got, 1, digestA)
	if want := withAPIDigest(kustomization(digestB), digestD); f.mainFile(t, kustomizationPath) != want {
		t.Fatalf("file =\n%s\nwant\n%s", f.mainFile(t, kustomizationPath), want)
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
	if got != (Result{}) {
		t.Fatalf("Pin() = %+v, want no commit attributed to this deployment", got)
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

func TestPinPullRequestMergedWithOtherContent(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	f.repo.Seed("main", kustomizationPath, kustomization(digestA))
	f.repo.BlockMerges(1 << 30)
	var once atomic.Bool
	f.hook(func(method, path string) {
		if method == http.MethodGet && path == "/repos/acme/gitops/pulls/1" && once.CompareAndSwap(false, true) {
			f.repo.Commit(headBranch, "someone else's digest", map[string]string{kustomizationPath: kustomization(digestD)})
			f.repo.BlockMerges(0)
			if err := f.mergeAsSomeoneElse(t.Context(), 1); err != nil {
				t.Errorf("merge as someone else: %v", err)
			}
		}
	})

	_, err := f.writer.Pin(t.Context(), pullRequestRequest())
	if !errors.Is(err, ErrPinOverwritten) {
		t.Fatalf("Pin() error = %v, want ErrPinOverwritten", err)
	}
	if content := f.mainFile(t, kustomizationPath); content != kustomization(digestD) {
		t.Fatalf("file =\n%s", content)
	}
	if n := len(f.repo.Pulls()); n != 1 {
		t.Fatalf("pull requests = %d, want 1", n)
	}
}

func TestPinPullRequestDoesNotReplayOverwrittenPin(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	f.repo.Seed("main", kustomizationPath, kustomization(digestA))
	if _, err := f.writer.Pin(t.Context(), pullRequestRequest()); err != nil {
		t.Fatalf("first Pin() error = %v", err)
	}
	newer := pullRequestRequest()
	newer.Deployment = 43
	newer.Digest = digestD
	if _, err := f.writer.Pin(t.Context(), newer); err != nil {
		t.Fatalf("newer Pin() error = %v", err)
	}
	head := f.mainSHA(t)

	if _, err := f.writer.Pin(t.Context(), pullRequestRequest()); !errors.Is(err, ErrPinOverwritten) {
		t.Fatalf("replayed Pin() error = %v, want ErrPinOverwritten", err)
	}
	if f.mainSHA(t) != head || len(f.repo.Pulls()) != 2 {
		t.Fatalf("replayed Pin() changed the repository: pulls %+v", f.repo.Pulls())
	}
}

func TestPinPullRequestAdoptsConcurrentPullRequest(t *testing.T) {
	t.Parallel()

	f := newFixture(t, Options{})
	f.repo.Seed("main", kustomizationPath, kustomization(digestA))
	var once atomic.Bool
	f.hook(func(method, path string) {
		if method == http.MethodPost && path == "/repos/acme/gitops/pulls" && once.CompareAndSwap(false, true) {
			repo, err := f.app.Repo(t.Context(), "acme", "gitops")
			if err == nil {
				_, err = repo.CreatePullRequest(t.Context(), github.NewPullRequest{Title: "concurrent", Head: headBranch, Base: "main"})
			}
			if err != nil {
				t.Errorf("concurrent CreatePullRequest() error = %v", err)
			}
		}
	})

	got, err := f.writer.Pin(t.Context(), pullRequestRequest())
	if err != nil {
		t.Fatalf("Pin() error = %v", err)
	}
	if got.PullRequest != 1 || !got.Changed || len(f.repo.Pulls()) != 1 {
		t.Fatalf("Pin() = %+v, pulls %+v", got, f.repo.Pulls())
	}
}

func TestPinPullRequestUpdateBranchRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		onUpdate   func(f *fixture)
		wantPull   int
		wantPrev   core.Digest
		wantClosed bool
	}{
		{
			name:     "nothing to update",
			onUpdate: func(*fixture) {},
			wantPull: 1,
			wantPrev: digestA,
		},
		{
			name: "update conflicts",
			onUpdate: func(f *fixture) {
				f.repo.Commit("main", "manual pin", map[string]string{kustomizationPath: kustomization(digestD)})
			},
			wantPull:   2,
			wantPrev:   digestD,
			wantClosed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, Options{})
			f.repo.Seed("main", kustomizationPath, kustomization(digestA))
			f.repo.SetMergeableState(github.MergeableBehind)
			var once atomic.Bool
			f.hook(func(method, path string) {
				if method == http.MethodPut && strings.HasSuffix(path, "/update-branch") && once.CompareAndSwap(false, true) {
					tt.onUpdate(f)
					f.repo.SetMergeableState("")
				}
			})

			got, err := f.writer.Pin(t.Context(), pullRequestRequest())
			if err != nil {
				t.Fatalf("Pin() error = %v", err)
			}
			f.assertMerged(t, got, tt.wantPull, tt.wantPrev)
			if first := f.repo.Pulls()[0]; tt.wantClosed && (first.Merged || !strings.Contains(first.Body, supersededMarker)) {
				t.Fatalf("first pull request = %+v", first)
			}
		})
	}
}
