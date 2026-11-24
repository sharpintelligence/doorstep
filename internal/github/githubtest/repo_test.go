package githubtest_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
)

func newRepo(t *testing.T) (*githubtest.Repo, *github.Repo) {
	t.Helper()
	srv := githubtest.NewServer(t)
	fake := srv.AddRepo("acme", "gitops", 3)
	app, err := github.New(github.Options{AppID: srv.AppID, PrivateKey: srv.PrivateKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	repo, err := app.Repo(t.Context(), "acme", "gitops")
	if err != nil {
		t.Fatalf("Repo: %v", err)
	}
	return fake, repo
}

func TestFakeRepoContents(t *testing.T) {
	t.Parallel()

	fake, repo := newRepo(t)
	seed := fake.Seed("main", "apps/a.yaml", "a: 1\n")
	ctx := t.Context()

	file, err := repo.GetFile(ctx, "apps/a.yaml", "main")
	if err != nil || string(file.Content) != "a: 1\n" || file.SHA == "" {
		t.Fatalf("GetFile() = %+v, %v", file, err)
	}
	if bySHA, err := repo.GetFile(ctx, "apps/a.yaml", seed); err != nil || bySHA.SHA != file.SHA {
		t.Fatalf("GetFile(sha) = %+v, %v", bySHA, err)
	}
	if _, err := repo.GetFile(ctx, "apps", "main"); !errors.Is(err, github.ErrNotAFile) {
		t.Fatalf("GetFile(dir) error = %v", err)
	}
	if _, err := repo.GetFile(ctx, "missing.yaml", "main"); !github.HasStatus(err, http.StatusNotFound) {
		t.Fatalf("GetFile(missing) error = %v", err)
	}

	commit, err := repo.PutFile(ctx, github.PutFileRequest{Path: "apps/a.yaml", Branch: "main", Message: "edit", Content: []byte("a: 2\n"), SHA: file.SHA})
	if err != nil {
		t.Fatalf("PutFile() error = %v", err)
	}
	if head, _ := fake.BranchSHA("main"); head != commit {
		t.Fatalf("head = %s, want %s", head, commit)
	}
	if content, _ := fake.File("main", "apps/a.yaml"); content != "a: 2\n" {
		t.Fatalf("content = %q", content)
	}
	if commits := fake.Commits("main"); len(commits) != 2 || commits[0].Message != "edit" || commits[0].Parents[0] != seed {
		t.Fatalf("commits = %+v", commits)
	}

	stale := github.PutFileRequest{Path: "apps/a.yaml", Branch: "main", Message: "stale", Content: []byte("x"), SHA: file.SHA}
	if _, err := repo.PutFile(ctx, stale); !github.HasStatus(err, http.StatusConflict) {
		t.Fatalf("stale PutFile() error = %v", err)
	}
	stale.SHA = ""
	if _, err := repo.PutFile(ctx, stale); !github.HasStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("PutFile() without sha error = %v", err)
	}
	if _, err := repo.PutFile(ctx, github.PutFileRequest{Path: "new.yaml", Branch: "main", Message: "new", Content: []byte("n")}); err != nil {
		t.Fatalf("PutFile(new) error = %v", err)
	}
	if _, err := repo.PutFile(ctx, github.PutFileRequest{Path: "new.yaml", Branch: "nope", Message: "m", Content: []byte("n")}); !github.HasStatus(err, http.StatusNotFound) {
		t.Fatalf("PutFile(missing branch) error = %v", err)
	}
	fake.RejectPut(http.StatusForbidden, "denied")
	if _, err := repo.PutFile(ctx, github.PutFileRequest{Path: "new2.yaml", Branch: "main", Message: "m", Content: []byte("n")}); !github.HasStatus(err, http.StatusForbidden) {
		t.Fatalf("rejected PutFile() error = %v", err)
	}
	fake.RejectPut(0, "")

	big := strings.Repeat("x", github.MaxFileSize+1)
	fake.Seed("main", "big.txt", big)
	if _, err := repo.GetFile(ctx, "big.txt", "main"); !errors.Is(err, github.ErrFileTooLarge) {
		t.Fatalf("GetFile(big) error = %v", err)
	}
}

func TestFakeRepoRefsAndCommits(t *testing.T) {
	t.Parallel()

	fake, repo := newRepo(t)
	ctx := t.Context()
	first := fake.Seed("main", "a.yaml", "1")
	fake.Seed("main", "b.yaml", "1")
	third := fake.Seed("main", "a.yaml", "2")

	commits, err := repo.ListCommits(ctx, "main", "a.yaml", 10)
	if err != nil {
		t.Fatalf("ListCommits() error = %v", err)
	}
	if len(commits) != 2 || commits[0].SHA != third || commits[1].SHA != first {
		t.Fatalf("ListCommits() = %+v", commits)
	}
	if limited, _ := repo.ListCommits(ctx, "main", "a.yaml", 1); len(limited) != 1 {
		t.Fatalf("ListCommits(1) = %+v", limited)
	}

	head, err := repo.BranchSHA(ctx, "main")
	if err != nil || head != third {
		t.Fatalf("BranchSHA() = %q, %v", head, err)
	}
	if err := repo.CreateBranch(ctx, "doorstep/x-1", head); err != nil {
		t.Fatalf("CreateBranch() error = %v", err)
	}
	if err := repo.CreateBranch(ctx, "doorstep/x-1", head); !errors.Is(err, github.ErrRefExists) {
		t.Fatalf("second CreateBranch() error = %v", err)
	}
	if err := repo.CreateBranch(ctx, "doorstep/x-2", "0000"); err == nil || errors.Is(err, github.ErrRefExists) {
		t.Fatalf("CreateBranch(unknown sha) error = %v", err)
	}
	if err := repo.DeleteBranch(ctx, "doorstep/x-1"); err != nil {
		t.Fatalf("DeleteBranch() error = %v", err)
	}
	if err := repo.DeleteBranch(ctx, "doorstep/x-1"); !errors.Is(err, github.ErrRefNotFound) {
		t.Fatalf("second DeleteBranch() error = %v", err)
	}
	if _, err := repo.BranchSHA(ctx, "doorstep/x-1"); !errors.Is(err, github.ErrRefNotFound) {
		t.Fatalf("BranchSHA(deleted) error = %v", err)
	}
	if got := fake.Branches(); !slices.Equal(got, []string{"main"}) {
		t.Fatalf("Branches() = %v", got)
	}
}

func TestFakeRepoPullRequests(t *testing.T) {
	t.Parallel()

	fake, repo := newRepo(t)
	ctx := t.Context()
	fake.Seed("main", "a.yaml", "a: 1\n")
	fake.CreateBranch("feature", "main")

	if _, err := repo.CreatePullRequest(ctx, github.NewPullRequest{Title: "t", Head: "feature", Base: "main"}); !github.HasStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("CreatePullRequest() without commits error = %v", err)
	}
	fake.Commit("feature", "edit", map[string]string{"a.yaml": "a: 2\n"})
	pr, err := repo.CreatePullRequest(ctx, github.NewPullRequest{Title: "edit a", Body: "body", Head: "feature", Base: "main"})
	if err != nil {
		t.Fatalf("CreatePullRequest() error = %v", err)
	}
	if pr.Number != 1 || pr.State != "open" || pr.ChangedFiles != 1 || pr.HeadRepo != "acme/gitops" || pr.MergeableState != "clean" {
		t.Fatalf("pull request = %+v", pr)
	}
	if _, err := repo.CreatePullRequest(ctx, github.NewPullRequest{Title: "dup", Head: "feature", Base: "main"}); !github.HasStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("duplicate CreatePullRequest() error = %v", err)
	}

	fake.BlockMerges(1)
	if got, _ := repo.GetPullRequest(ctx, 1); got.MergeableState != "blocked" {
		t.Fatalf("mergeable state = %q, want blocked", got.MergeableState)
	}
	if _, err := repo.MergePullRequest(ctx, 1, github.MergeRequest{SHA: pr.HeadSHA, Method: github.MergeMethodSquash}); !github.HasStatus(err, http.StatusMethodNotAllowed) {
		t.Fatalf("blocked merge error = %v", err)
	}
	if _, err := repo.MergePullRequest(ctx, 1, github.MergeRequest{SHA: "other", Method: github.MergeMethodSquash}); !github.HasStatus(err, http.StatusConflict) {
		t.Fatalf("stale merge error = %v", err)
	}
	if _, err := repo.MergePullRequest(ctx, 1, github.MergeRequest{SHA: pr.HeadSHA, Method: "merge"}); !github.HasStatus(err, http.StatusMethodNotAllowed) {
		t.Fatalf("merge commit error = %v", err)
	}

	fake.RequireUpToDate(true)
	fake.Commit("main", "other", map[string]string{"b.yaml": "b"})
	behind, _ := repo.GetPullRequest(ctx, 1)
	if behind.MergeableState != "behind" {
		t.Fatalf("mergeable state = %q, want behind", behind.MergeableState)
	}
	if err := repo.UpdatePullRequestBranch(ctx, 1, "stale"); !github.HasStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("UpdatePullRequestBranch(stale) error = %v", err)
	}
	if err := repo.UpdatePullRequestBranch(ctx, 1, behind.HeadSHA); err != nil {
		t.Fatalf("UpdatePullRequestBranch() error = %v", err)
	}
	updated, _ := repo.GetPullRequest(ctx, 1)
	if updated.MergeableState != "clean" || updated.ChangedFiles != 1 || updated.HeadSHA == behind.HeadSHA {
		t.Fatalf("updated pull request = %+v", updated)
	}

	sha, err := repo.MergePullRequest(ctx, 1, github.MergeRequest{SHA: updated.HeadSHA, Title: "squash", Message: "body\n\nX-Trailer: 1", Method: github.MergeMethodSquash})
	if err != nil {
		t.Fatalf("MergePullRequest() error = %v", err)
	}
	commits := fake.Commits("main")
	if commits[0].SHA != sha || commits[0].Message != "squash\n\nbody\n\nX-Trailer: 1" || len(commits[0].Parents) != 1 {
		t.Fatalf("squash commit = %+v", commits[0])
	}
	if a, _ := fake.File("main", "a.yaml"); a != "a: 2\n" {
		t.Fatalf("a.yaml = %q", a)
	}
	if b, _ := fake.File("main", "b.yaml"); b != "b" {
		t.Fatalf("b.yaml = %q", b)
	}
	merged, _ := repo.GetPullRequest(ctx, 1)
	if !merged.Merged || merged.State != "closed" || merged.MergeCommitSHA != sha {
		t.Fatalf("merged pull request = %+v", merged)
	}
	pulls, err := repo.ListPullRequests(ctx, "feature", "main")
	if err != nil || len(pulls) != 1 || !pulls[0].Merged {
		t.Fatalf("ListPullRequests() = %+v, %v", pulls, err)
	}
}

func TestFakeRepoPullRequestLifecycle(t *testing.T) {
	t.Parallel()

	fake, repo := newRepo(t)
	ctx := t.Context()
	fake.Seed("main", "a.yaml", "1")
	fake.CreateBranch("feature", "main")
	fake.Commit("feature", "edit", map[string]string{"a.yaml": "2"})
	if _, err := repo.CreatePullRequest(ctx, github.NewPullRequest{Title: "one", Head: "feature", Base: "main"}); err != nil {
		t.Fatalf("CreatePullRequest() error = %v", err)
	}

	fake.Commit("main", "conflict", map[string]string{"a.yaml": "3"})
	if pr, _ := repo.GetPullRequest(ctx, 1); pr.MergeableState != "dirty" {
		t.Fatalf("mergeable state = %q, want dirty", pr.MergeableState)
	}
	fake.SetMergeableState("unstable")
	if pr, _ := repo.GetPullRequest(ctx, 1); pr.MergeableState != "unstable" {
		t.Fatalf("mergeable state = %q, want unstable", pr.MergeableState)
	}
	fake.SetPullHeadRepo(1, "mallory/gitops")
	if pr, _ := repo.GetPullRequest(ctx, 1); pr.HeadRepo != "mallory/gitops" {
		t.Fatalf("head repo = %q", pr.HeadRepo)
	}

	if _, err := repo.UpdatePullRequest(ctx, 1, github.PullRequestUpdate{Body: "new body"}); err != nil {
		t.Fatalf("UpdatePullRequest() error = %v", err)
	}
	if err := repo.DeleteBranch(ctx, "feature"); err != nil {
		t.Fatalf("DeleteBranch() error = %v", err)
	}
	pr, _ := repo.GetPullRequest(ctx, 1)
	if pr.State != "closed" || pr.Merged || pr.Body != "new body" || pr.HeadSHA == "" {
		t.Fatalf("pull request after branch deletion = %+v", pr)
	}
	if got := fake.Pulls(); len(got) != 1 || got[0].State != "closed" || got[0].Title != "one" {
		t.Fatalf("Pulls() = %+v", got)
	}
	if _, err := repo.GetPullRequest(ctx, 9); !github.HasStatus(err, http.StatusNotFound) {
		t.Fatalf("GetPullRequest(9) error = %v", err)
	}

	fake.CreateBranch("feature", "main")
	fake.Commit("feature", "edit", map[string]string{"a.yaml": "4"})
	if _, err := repo.CreatePullRequest(ctx, github.NewPullRequest{Title: "two", Head: "feature", Base: "main"}); err != nil {
		t.Fatalf("CreatePullRequest() error = %v", err)
	}
	fake.ClosePull(2)
	pulls, err := repo.ListPullRequests(ctx, "feature", "main")
	if err != nil || len(pulls) != 2 || pulls[0].Number != 2 || pulls[1].Number != 1 {
		t.Fatalf("ListPullRequests() = %+v, %v", pulls, err)
	}
}

func TestFakeRepoRequiresItsInstallation(t *testing.T) {
	t.Parallel()

	srv := githubtest.NewServer(t)
	srv.AddRepo("acme", "gitops", 3)
	srv.Install("acme", "other", 4)
	app, err := github.New(github.Options{AppID: srv.AppID, PrivateKey: srv.PrivateKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	other, err := app.Repo(t.Context(), "acme", "other")
	if err != nil {
		t.Fatalf("Repo: %v", err)
	}
	if _, err := other.BranchSHA(t.Context(), "main"); !errors.Is(err, github.ErrRefNotFound) {
		t.Fatalf("BranchSHA() on a repository without a fake error = %v", err)
	}
	var mu sync.Mutex
	var seen []string
	srv.AddRepo("acme", "hooks", 3).OnRequest(func(method, path string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, method+" "+path)
	})
	hooks, err := app.Repo(t.Context(), "acme", "hooks")
	if err != nil {
		t.Fatalf("Repo: %v", err)
	}
	_, _ = hooks.BranchSHA(t.Context(), "main")
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []string{"GET /repos/acme/hooks/git/ref/heads/main"}) {
		t.Fatalf("hook saw %v", seen)
	}
}
