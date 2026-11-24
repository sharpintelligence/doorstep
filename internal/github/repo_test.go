package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
)

func (f *fixture) repo(t *testing.T) *Repo {
	t.Helper()
	f.srv.Install("acme", "gitops", 9)
	repo, err := f.app.Repo(t.Context(), "acme", "gitops")
	if err != nil {
		t.Fatalf("Repo: %v", err)
	}
	return repo
}

func TestAppRepo(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	repo := f.repo(t)
	if repo.Owner() != "acme" || repo.Name() != "gitops" || repo.FullName() != "acme/gitops" {
		t.Fatalf("repo = %s (%s, %s)", repo.FullName(), repo.Owner(), repo.Name())
	}
	if _, err := f.app.Repo(t.Context(), "acme", "../x"); !errors.Is(err, ErrInvalidRepository) {
		t.Fatalf("Repo(../x) error = %v", err)
	}
	if _, err := f.app.Repo(t.Context(), "acme", "missing"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Repo(missing) error = %v", err)
	}
}

func contentsHandler(body any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		githubtest.WriteJSON(w, http.StatusOK, body)
	}
}

func fileBody(path, content string) map[string]any {
	return map[string]any{
		"type":     "file",
		"encoding": "base64",
		"size":     len(content),
		"path":     path,
		"sha":      "blob1",
		"content":  base64.StdEncoding.EncodeToString([]byte(content)),
	}
}

func TestGetFile(t *testing.T) {
	t.Parallel()

	wrapped := fileBody("a.yaml", "hello: world\n")
	encoded := wrapped["content"].(string)
	wrapped["content"] = encoded[:8] + "\n" + encoded[8:] + "\n"

	tests := []struct {
		name    string
		path    string
		ref     string
		handler http.HandlerFunc
		want    File
		wantErr error
	}{
		{name: "file", path: "a.yaml", ref: "main", handler: contentsHandler(fileBody("a.yaml", "x: 1\n")), want: File{Path: "a.yaml", SHA: "blob1", Content: []byte("x: 1\n")}},
		{name: "wrapped base64", path: "a.yaml", handler: contentsHandler(wrapped), want: File{Path: "a.yaml", SHA: "blob1", Content: []byte("hello: world\n")}},
		{name: "directory", path: "a.yaml", handler: contentsHandler([]any{map[string]string{"type": "file"}}), wantErr: ErrNotAFile},
		{name: "symlink", path: "a.yaml", handler: contentsHandler(map[string]any{"type": "symlink", "path": "a.yaml", "target": "b.yaml"}), wantErr: ErrNotAFile},
		{name: "submodule", path: "a.yaml", handler: contentsHandler(map[string]any{"type": "submodule", "path": "a.yaml"}), wantErr: ErrNotAFile},
		{name: "resolved elsewhere", path: "a.yaml", handler: contentsHandler(fileBody("b.yaml", "x")), wantErr: ErrNotAFile},
		{
			name: "too large", path: "a.yaml",
			handler: contentsHandler(map[string]any{"type": "file", "path": "a.yaml", "encoding": "none", "size": MaxFileSize + 1, "sha": "s", "content": ""}),
			wantErr: ErrFileTooLarge,
		},
		{
			name: "unsupported encoding", path: "a.yaml",
			handler: contentsHandler(map[string]any{"type": "file", "path": "a.yaml", "encoding": "none", "size": 1, "sha": "s", "content": ""}),
			wantErr: ErrNotAFile,
		},
		{name: "empty path", path: "", wantErr: ErrInvalidPath},
		{name: "dot dot", path: "apps/../secrets", wantErr: ErrInvalidPath},
		{name: "empty segment", path: "apps//a.yaml", wantErr: ErrInvalidPath},
		{name: "control character", path: "a\n.yaml", wantErr: ErrInvalidPath},
		{name: "ref with space", path: "a.yaml", ref: "a b", wantErr: ErrInvalidPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			if tt.handler != nil {
				f.srv.Handle("GET /repos/acme/gitops/contents/{path...}", tt.handler)
			}
			got, err := f.repo(t).GetFile(t.Context(), tt.path, tt.ref)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("GetFile() error = %v, want %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("GetFile() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestGetFileEscapesPathSegments(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	var gotPath, gotRawPath, gotRef string
	f.srv.Handle("GET /repos/acme/gitops/contents/{path...}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotRawPath, gotRef = r.PathValue("path"), r.URL.EscapedPath(), r.URL.Query().Get("ref")
		githubtest.WriteJSON(w, http.StatusOK, fileBody(gotPath, "x"))
	}))
	if _, err := f.repo(t).GetFile(t.Context(), "apps/my app/v#1?.yaml", "feature/x&y"); err != nil {
		t.Fatalf("GetFile() error = %v", err)
	}
	if gotPath != "apps/my app/v#1?.yaml" || gotRef != "feature/x&y" {
		t.Fatalf("path = %q, ref = %q", gotPath, gotRef)
	}
	if want := "/repos/acme/gitops/contents/apps/my%20app/v%231%3F.yaml"; gotRawPath != want {
		t.Fatalf("escaped path = %q, want %q", gotRawPath, want)
	}
}

func TestPutFile(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	var got map[string]string
	f.srv.Handle("PUT /repos/acme/gitops/contents/{path...}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			githubtest.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		got["path"] = r.PathValue("path")
		githubtest.WriteJSON(w, http.StatusOK, map[string]any{"commit": map[string]string{"sha": "c0ffee"}})
	}))
	repo := f.repo(t)
	sha, err := repo.PutFile(t.Context(), PutFileRequest{Path: "apps/a.yaml", Branch: "main", Message: "pin", Content: []byte("x"), SHA: "blob1"})
	if err != nil || sha != "c0ffee" {
		t.Fatalf("PutFile() = %q, %v", sha, err)
	}
	want := map[string]string{"path": "apps/a.yaml", "branch": "main", "message": "pin", "content": "eA==", "sha": "blob1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("request = %v, want %v", got, want)
	}

	tests := []struct {
		name    string
		req     PutFileRequest
		wantErr error
	}{
		{name: "too large", req: PutFileRequest{Path: "a", Branch: "main", Message: "m", Content: make([]byte, MaxFileSize+1)}, wantErr: ErrFileTooLarge},
		{name: "bad path", req: PutFileRequest{Path: "../a", Branch: "main", Message: "m"}, wantErr: ErrInvalidPath},
		{name: "bad branch", req: PutFileRequest{Path: "a", Branch: "", Message: "m"}, wantErr: ErrInvalidPath},
	}
	for _, tt := range tests {
		if _, err := repo.PutFile(t.Context(), tt.req); !errors.Is(err, tt.wantErr) {
			t.Errorf("%s: PutFile() error = %v, want %v", tt.name, err, tt.wantErr)
		}
	}
	if _, err := repo.PutFile(t.Context(), PutFileRequest{Path: "a", Branch: "main", Message: " "}); err == nil {
		t.Error("PutFile() without a message succeeded")
	}
}

func TestPutFileConflictIsAPIError(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Handle("PUT /repos/acme/gitops/contents/{path...}", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		githubtest.WriteError(w, http.StatusConflict, "a.yaml does not match blob1")
	}))
	_, err := f.repo(t).PutFile(t.Context(), PutFileRequest{Path: "a.yaml", Branch: "main", Message: "m", SHA: "blob1"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict || !strings.Contains(apiErr.Message, "does not match") {
		t.Fatalf("PutFile() error = %v", err)
	}
	if !HasStatus(err, http.StatusConflict) || HasStatus(err, http.StatusNotFound) {
		t.Fatal("HasStatus does not classify the error")
	}
}

func TestBranchCalls(t *testing.T) {
	t.Parallel()

	errPlainAPI := errors.New("plain api error")
	tests := []struct {
		name    string
		status  int
		message string
		call    func(ctx context.Context, r *Repo) error
		wantErr error
	}{
		{
			name: "branch sha", status: http.StatusOK,
			call: func(ctx context.Context, r *Repo) error {
				sha, err := r.BranchSHA(ctx, "doorstep/blog-1")
				if err == nil && sha != "abc" {
					return fmt.Errorf("BranchSHA() = %q", sha)
				}
				return err
			},
		},
		{
			name: "branch missing", status: http.StatusNotFound, message: "Not Found",
			call:    func(ctx context.Context, r *Repo) error { _, err := r.BranchSHA(ctx, "nope"); return err },
			wantErr: ErrRefNotFound,
		},
		{
			name: "create", status: http.StatusCreated,
			call: func(ctx context.Context, r *Repo) error { return r.CreateBranch(ctx, "doorstep/blog-1", "abc") },
		},
		{
			name: "create existing", status: http.StatusUnprocessableEntity, message: "Reference already exists",
			call:    func(ctx context.Context, r *Repo) error { return r.CreateBranch(ctx, "doorstep/blog-1", "abc") },
			wantErr: ErrRefExists,
		},
		{
			name: "create rejected", status: http.StatusUnprocessableEntity, message: "Object does not exist",
			call:    func(ctx context.Context, r *Repo) error { return r.CreateBranch(ctx, "doorstep/blog-1", "zzz") },
			wantErr: errPlainAPI,
		},
		{
			name: "delete", status: http.StatusNoContent,
			call: func(ctx context.Context, r *Repo) error { return r.DeleteBranch(ctx, "doorstep/blog-1") },
		},
		{
			name: "delete missing", status: http.StatusUnprocessableEntity, message: "Reference does not exist",
			call:    func(ctx context.Context, r *Repo) error { return r.DeleteBranch(ctx, "doorstep/blog-1") },
			wantErr: ErrRefNotFound,
		},
		{
			name:    "invalid name",
			call:    func(ctx context.Context, r *Repo) error { return r.DeleteBranch(ctx, "a/../b") },
			wantErr: ErrInvalidPath,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.PathValue("ref") != "heads/doorstep/blog-1" {
					githubtest.WriteError(w, http.StatusNotFound, "Not Found")
					return
				}
				if tt.message != "" {
					githubtest.WriteError(w, tt.status, tt.message)
					return
				}
				githubtest.WriteJSON(w, tt.status, map[string]any{"object": map[string]string{"sha": "abc"}})
			})
			f.srv.Handle("GET /repos/acme/gitops/git/ref/{ref...}", handler)
			f.srv.Handle("POST /repos/acme/gitops/git/refs", handler)
			f.srv.Handle("DELETE /repos/acme/gitops/git/refs/{ref...}", handler)
			err := tt.call(t.Context(), f.repo(t))
			var apiErr *APIError
			switch {
			case errors.Is(tt.wantErr, errPlainAPI):
				if !errors.As(err, &apiErr) || errors.Is(err, ErrRefExists) || errors.Is(err, ErrRefNotFound) {
					t.Fatalf("error = %v, want a plain API error", err)
				}
			case !errors.Is(err, tt.wantErr):
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestListCommits(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Handle("GET /repos/acme/gitops/commits", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("sha") != "main" || q.Get("path") != "apps/a.yaml" || q.Get("per_page") != "2" {
			githubtest.WriteError(w, http.StatusBadRequest, "query "+r.URL.RawQuery)
			return
		}
		githubtest.WriteJSON(w, http.StatusOK, []map[string]any{
			{"sha": "1", "commit": map[string]string{"message": "one"}},
			{"sha": "2", "commit": map[string]string{"message": "two"}},
			{"sha": "3", "commit": map[string]string{"message": "three"}},
		})
	}))
	repo := f.repo(t)
	got, err := repo.ListCommits(t.Context(), "main", "apps/a.yaml", 2)
	if err != nil {
		t.Fatalf("ListCommits() error = %v", err)
	}
	if want := []CommitInfo{{SHA: "1", Message: "one"}, {SHA: "2", Message: "two"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ListCommits() = %+v, want %+v", got, want)
	}
	for _, limit := range []int{0, 101} {
		if _, err := repo.ListCommits(t.Context(), "main", "a", limit); err == nil {
			t.Errorf("ListCommits(limit %d) succeeded", limit)
		}
	}
}

func TestPullRequestCalls(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	var mu sync.Mutex
	requests := map[string]map[string]any{}
	record := func(r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		requests[r.Method+" "+r.URL.Path] = body
	}
	pull := map[string]any{
		"number": 5, "state": "closed", "merged_at": "2026-01-01T00:00:00Z", "mergeable_state": "unknown",
		"merge_commit_sha": "m1", "title": "t", "body": nil, "html_url": "https://github.test/pull/5", "changed_files": 1,
		"head": map[string]any{"ref": "doorstep/blog-1", "sha": "h1", "repo": nil},
		"base": map[string]any{"ref": "main"},
	}
	f.srv.Handle("GET /repos/acme/gitops/pulls", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("head") != "acme:doorstep/blog-1" || q.Get("base") != "main" || q.Get("state") != "all" {
			githubtest.WriteError(w, http.StatusBadRequest, "query "+r.URL.RawQuery)
			return
		}
		githubtest.WriteJSON(w, http.StatusOK, []any{pull})
	}))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(r)
		switch {
		case strings.HasSuffix(r.URL.Path, "/merge"):
			githubtest.WriteJSON(w, http.StatusOK, map[string]any{"sha": "squash1", "merged": true})
		case strings.HasSuffix(r.URL.Path, "/update-branch"):
			githubtest.WriteJSON(w, http.StatusAccepted, map[string]string{"message": "Updating pull request branch."})
		default:
			githubtest.WriteJSON(w, http.StatusOK, pull)
		}
	})
	f.srv.Handle("POST /repos/acme/gitops/pulls", handler)
	f.srv.Handle("GET /repos/acme/gitops/pulls/{n}", handler)
	f.srv.Handle("PATCH /repos/acme/gitops/pulls/{n}", handler)
	f.srv.Handle("PUT /repos/acme/gitops/pulls/{n}/merge", handler)
	f.srv.Handle("PUT /repos/acme/gitops/pulls/{n}/update-branch", handler)
	repo := f.repo(t)
	ctx := t.Context()

	want := PullRequest{
		Number: 5, State: "closed", Merged: true, MergeableState: "unknown", MergeCommitSHA: "m1",
		HeadRef: "doorstep/blog-1", HeadSHA: "h1", BaseRef: "main", Title: "t", HTMLURL: "https://github.test/pull/5", ChangedFiles: 1,
	}
	pulls, err := repo.ListPullRequests(ctx, "doorstep/blog-1", "main")
	if err != nil || len(pulls) != 1 || !reflect.DeepEqual(pulls[0], want) {
		t.Fatalf("ListPullRequests() = %+v, %v", pulls, err)
	}
	if got, err := repo.GetPullRequest(ctx, 5); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("GetPullRequest() = %+v, %v", got, err)
	}
	if _, err := repo.CreatePullRequest(ctx, NewPullRequest{Title: "t", Body: "b", Head: "doorstep/blog-1", Base: "main"}); err != nil {
		t.Fatalf("CreatePullRequest() error = %v", err)
	}
	if _, err := repo.CreatePullRequest(ctx, NewPullRequest{Head: "doorstep/blog-1", Base: "main"}); err == nil {
		t.Fatal("CreatePullRequest() without a title succeeded")
	}
	if err := repo.ClosePullRequest(ctx, 5); err != nil {
		t.Fatalf("ClosePullRequest() error = %v", err)
	}
	sha, err := repo.MergePullRequest(ctx, 5, MergeRequest{SHA: "h1", Title: "s", Message: "m", Method: MergeMethodSquash})
	if err != nil || sha != "squash1" {
		t.Fatalf("MergePullRequest() = %q, %v", sha, err)
	}
	if _, err := repo.MergePullRequest(ctx, 5, MergeRequest{}); err == nil {
		t.Fatal("MergePullRequest() without a head sha succeeded")
	}
	if err := repo.UpdatePullRequestBranch(ctx, 5, "h1"); err != nil {
		t.Fatalf("UpdatePullRequestBranch() error = %v", err)
	}

	wantRequests := map[string]map[string]any{
		"POST /repos/acme/gitops/pulls":                {"title": "t", "body": "b", "head": "doorstep/blog-1", "base": "main"},
		"PATCH /repos/acme/gitops/pulls/5":             {"state": "closed"},
		"PUT /repos/acme/gitops/pulls/5/merge":         {"sha": "h1", "commit_title": "s", "commit_message": "m", "merge_method": "squash"},
		"PUT /repos/acme/gitops/pulls/5/update-branch": {"expected_head_sha": "h1"},
		"GET /repos/acme/gitops/pulls/5":               nil,
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(requests, wantRequests) {
		t.Fatalf("requests = %v\nwant %v", requests, wantRequests)
	}
}
