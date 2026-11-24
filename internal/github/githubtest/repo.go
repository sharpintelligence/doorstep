package githubtest

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxContentsSize   = 1 << 20
	maxComparePerPage = 250
	base64LineLength  = 60
)

type Commit struct {
	SHA     string
	Message string
	Parents []string
}

type PullRequest struct {
	Number         int
	Title          string
	Body           string
	Head           string
	Base           string
	State          string
	Merged         bool
	MergeCommitSHA string
	Labels         []string
}

type fakeCommit struct {
	sha     string
	message string
	parents []string
	files   map[string][]byte
	seq     int
}

type fakePull struct {
	number      int
	title       string
	body        string
	head        string
	base        string
	state       string
	merged      bool
	mergeCommit string
	headSHA     string
	headRepo    string
	labels      []string
}

type rejection struct {
	status  int
	message string
}

type Repo struct {
	owner         string
	name          string
	defaultBranch string

	mu              sync.Mutex
	seq             int
	branches        map[string]string
	commits         map[string]*fakeCommit
	pulls           []*fakePull
	onRequest       func(method, path string)
	putRejection    *rejection
	blockedMerges   int
	mergeableState  string
	requireUpToDate bool
}

func (s *Server) AddRepo(owner, name string, installationID int64) *Repo {
	s.Install(owner, name, installationID)
	repo := &Repo{
		owner:         owner,
		name:          name,
		defaultBranch: "main",
		branches:      make(map[string]string),
		commits:       make(map[string]*fakeCommit),
	}
	s.mu.Lock()
	if s.fakeRepos == nil {
		s.fakeRepos = make(map[string]*Repo)
	}
	s.fakeRepos[repoKey(owner, name)] = repo
	s.mu.Unlock()
	s.fakeRepoRoute.Do(s.routeFakeRepos)
	return repo
}

func (r *Repo) FullName() string {
	return r.owner + "/" + r.name
}

func (r *Repo) Seed(branch, path, content string) string {
	return r.Commit(branch, "seed "+path, map[string]string{path: content})
}

func (r *Repo) Commit(branch, message string, files map[string]string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var parents []string
	tree := map[string][]byte{}
	head, ok := r.branches[branch]
	if !ok {
		head, ok = r.branches[r.defaultBranch]
	}
	if ok {
		parents = []string{head}
		tree = maps.Clone(r.commits[head].files)
	}
	for path, content := range files {
		tree[path] = []byte(content)
	}
	c := r.newCommitLocked(message, parents, tree)
	r.branches[branch] = c.sha
	return c.sha
}

func (r *Repo) CreateBranch(name, from string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	sha := r.branches[from]
	r.branches[name] = sha
	return sha
}

func (r *Repo) File(branch, path string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.resolveLocked(branch)
	if !ok {
		return "", false
	}
	content, ok := c.files[path]
	return string(content), ok
}

func (r *Repo) BranchSHA(branch string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sha, ok := r.branches[branch]
	return sha, ok
}

func (r *Repo) Branches() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Sorted(maps.Keys(r.branches))
}

func (r *Repo) Commits(branch string) []Commit {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Commit
	c, ok := r.resolveLocked(branch)
	for ok {
		out = append(out, Commit{SHA: c.sha, Message: c.message, Parents: slices.Clone(c.parents)})
		if len(c.parents) == 0 {
			break
		}
		c, ok = r.commits[c.parents[0]]
	}
	return out
}

func (r *Repo) Pulls() []PullRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]PullRequest, 0, len(r.pulls))
	for _, p := range r.pulls {
		out = append(out, PullRequest{
			Number:         p.number,
			Title:          p.title,
			Body:           p.body,
			Head:           p.head,
			Base:           p.base,
			State:          p.state,
			Merged:         p.merged,
			MergeCommitSHA: p.mergeCommit,
			Labels:         slices.Clone(p.labels),
		})
	}
	return out
}

func (r *Repo) OnRequest(fn func(method, path string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onRequest = fn
}

func (r *Repo) RejectPut(status int, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if status == 0 {
		r.putRejection = nil
		return
	}
	r.putRejection = &rejection{status: status, message: message}
}

func (r *Repo) BlockMerges(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blockedMerges = n
}

func (r *Repo) SetMergeableState(state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mergeableState = state
}

func (r *Repo) RequireUpToDate(required bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requireUpToDate = required
}

func (r *Repo) ClosePull(number int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.pullLocked(number); p != nil && p.state == "open" {
		p.headSHA = r.headLocked(p)
		p.state = "closed"
	}
}

func (r *Repo) AddPull(p PullRequest) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := p.State
	if state == "" {
		state = "open"
		if p.Merged {
			state = "closed"
		}
	}
	base := p.Base
	if base == "" {
		base = r.defaultBranch
	}
	pull := &fakePull{
		number:      len(r.pulls) + 1,
		title:       p.Title,
		body:        p.Body,
		head:        p.Head,
		base:        base,
		state:       state,
		merged:      p.Merged,
		mergeCommit: p.MergeCommitSHA,
		headSHA:     p.MergeCommitSHA,
		headRepo:    r.FullName(),
		labels:      slices.Clone(p.Labels),
	}
	r.pulls = append(r.pulls, pull)
	return pull.number
}

func (r *Repo) LabelPull(number int, labels ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.pullLocked(number); p != nil {
		p.labels = append(p.labels, labels...)
	}
}

func (r *Repo) SetPullHeadRepo(number int, fullName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.pullLocked(number); p != nil {
		p.headRepo = fullName
	}
}

func (s *Server) routeFakeRepos() {
	routes := map[string]func(*Repo, http.ResponseWriter, *http.Request){
		"GET /repos/{owner}/{repo}/contents/{path...}":           (*Repo).handleGetContents,
		"PUT /repos/{owner}/{repo}/contents/{path...}":           (*Repo).handlePutContents,
		"POST /repos/{owner}/{repo}/git/refs":                    (*Repo).handleCreateRef,
		"DELETE /repos/{owner}/{repo}/git/refs/{ref...}":         (*Repo).handleDeleteRef,
		"PATCH /repos/{owner}/{repo}/git/refs/{ref...}":          (*Repo).handleUpdateRef,
		"GET /repos/{owner}/{repo}/commits":                      (*Repo).handleListCommits,
		"GET /repos/{owner}/{repo}/commits/{sha}":                (*Repo).handleGetCommit,
		"GET /repos/{owner}/{repo}/commits/{sha}/pulls":          (*Repo).handleCommitPulls,
		"GET /repos/{owner}/{repo}/compare/{basehead}":           (*Repo).handleCompare,
		"GET /repos/{owner}/{repo}/pulls":                        (*Repo).handleListPulls,
		"POST /repos/{owner}/{repo}/pulls":                       (*Repo).handleCreatePull,
		"GET /repos/{owner}/{repo}/pulls/{number}":               (*Repo).handleGetPull,
		"PATCH /repos/{owner}/{repo}/pulls/{number}":             (*Repo).handlePatchPull,
		"PUT /repos/{owner}/{repo}/pulls/{number}/merge":         (*Repo).handleMergePull,
		"PUT /repos/{owner}/{repo}/pulls/{number}/update-branch": (*Repo).handleUpdateBranch,
	}
	for pattern, handle := range routes {
		s.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			repo, ok := s.fakeRepo(req)
			if !ok {
				WriteError(w, http.StatusNotFound, "Not Found")
				return
			}
			repo.serve(handle, w, req)
		}))
	}
}

func (r *Repo) serve(handle func(*Repo, http.ResponseWriter, *http.Request), w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	hook := r.onRequest
	r.mu.Unlock()
	if hook != nil {
		hook(req.Method, req.URL.Path)
	}
	handle(r, w, req)
}

func (s *Server) fakeRepo(req *http.Request) (*Repo, bool) {
	key := repoKey(req.PathValue("owner"), req.PathValue("repo"))
	id, _ := InstallationID(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	repo, ok := s.fakeRepos[key]
	return repo, ok && s.repos[key] == id
}

func (r *Repo) handleGetContents(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref := req.URL.Query().Get("ref")
	if ref == "" {
		ref = r.defaultBranch
	}
	c, ok := r.resolveLocked(ref)
	if !ok {
		WriteError(w, http.StatusNotFound, "No commit found for the ref "+ref)
		return
	}
	path := req.PathValue("path")
	content, ok := c.files[path]
	if !ok {
		var entries []map[string]string
		for _, name := range slices.Sorted(maps.Keys(c.files)) {
			if strings.HasPrefix(name, path+"/") {
				entries = append(entries, map[string]string{"type": "file", "path": name})
			}
		}
		if entries == nil {
			WriteError(w, http.StatusNotFound, "Not Found")
			return
		}
		WriteJSON(w, http.StatusOK, entries)
		return
	}
	encoding, encoded := "base64", wrapBase64(content)
	if len(content) > maxContentsSize {
		encoding, encoded = "none", ""
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"type":     "file",
		"encoding": encoding,
		"size":     len(content),
		"name":     path[strings.LastIndex(path, "/")+1:],
		"path":     path,
		"sha":      blobSHA(content),
		"content":  encoded,
	})
}

func (r *Repo) handlePutContents(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Message string `json:"message"`
		Content string `json:"content"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		WriteError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	content, err := base64.StdEncoding.DecodeString(body.Content)
	if err != nil || body.Message == "" {
		WriteError(w, http.StatusUnprocessableEntity, "Invalid request.\n\ncontent is not valid Base64")
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.putRejection != nil {
		WriteError(w, r.putRejection.status, r.putRejection.message)
		return
	}
	branch := body.Branch
	if branch == "" {
		branch = r.defaultBranch
	}
	head, ok := r.branches[branch]
	if !ok {
		WriteError(w, http.StatusNotFound, "Branch "+branch+" not found")
		return
	}
	path := req.PathValue("path")
	existing, exists := r.commits[head].files[path]
	switch {
	case exists && body.SHA == "":
		WriteError(w, http.StatusUnprocessableEntity, "Invalid request.\n\n\"sha\" wasn't supplied.")
		return
	case exists && body.SHA != blobSHA(existing):
		WriteError(w, http.StatusConflict, path+" does not match "+body.SHA)
		return
	case !exists && body.SHA != "":
		WriteError(w, http.StatusUnprocessableEntity, "Invalid request.\n\n\"sha\" does not match any file")
		return
	}
	tree := maps.Clone(r.commits[head].files)
	tree[path] = content
	c := r.newCommitLocked(body.Message, []string{head}, tree)
	r.branches[branch] = c.sha
	status := http.StatusOK
	if !exists {
		status = http.StatusCreated
	}
	WriteJSON(w, status, map[string]any{
		"content": map[string]string{"path": path, "sha": blobSHA(content)},
		"commit":  map[string]string{"sha": c.sha, "message": c.message},
	})
}

func (r *Repo) handleGetRef(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	branch, ok := strings.CutPrefix(req.PathValue("ref"), "heads/")
	sha, exists := r.branches[branch]
	if !ok || !exists {
		WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"ref":    "refs/heads/" + branch,
		"object": map[string]string{"sha": sha, "type": "commit"},
	})
}

func (r *Repo) handleCreateRef(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		WriteError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	branch, ok := strings.CutPrefix(body.Ref, "refs/heads/")
	switch {
	case !ok || branch == "":
		WriteError(w, http.StatusUnprocessableEntity, "Reference name must start with 'refs/' and have at least two slashes.")
		return
	case r.branches[branch] != "":
		WriteError(w, http.StatusUnprocessableEntity, "Reference already exists")
		return
	case r.commits[body.SHA] == nil:
		WriteError(w, http.StatusUnprocessableEntity, "Object does not exist")
		return
	}
	r.branches[branch] = body.SHA
	WriteJSON(w, http.StatusCreated, map[string]any{
		"ref":    body.Ref,
		"object": map[string]string{"sha": body.SHA, "type": "commit"},
	})
}

func (r *Repo) handleUpdateRef(w http.ResponseWriter, req *http.Request) {
	var body struct {
		SHA   string `json:"sha"`
		Force bool   `json:"force"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		WriteError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	branch, ok := strings.CutPrefix(req.PathValue("ref"), "heads/")
	_, exists := r.branches[branch]
	switch {
	case !body.Force:
		WriteError(w, http.StatusUnprocessableEntity, "Update is not a fast forward")
		return
	case !ok || !exists:
		WriteError(w, http.StatusUnprocessableEntity, "Reference does not exist")
		return
	case r.commits[body.SHA] == nil:
		WriteError(w, http.StatusUnprocessableEntity, "Object does not exist")
		return
	}
	r.branches[branch] = body.SHA
	WriteJSON(w, http.StatusOK, map[string]any{
		"ref":    "refs/heads/" + branch,
		"object": map[string]string{"sha": body.SHA, "type": "commit"},
	})
}

func (r *Repo) handleDeleteRef(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	branch, ok := strings.CutPrefix(req.PathValue("ref"), "heads/")
	if _, exists := r.branches[branch]; !ok || !exists {
		WriteError(w, http.StatusUnprocessableEntity, "Reference does not exist")
		return
	}
	for _, p := range r.pulls {
		if p.head == branch && p.state == "open" {
			p.headSHA = r.headLocked(p)
			p.state = "closed"
		}
	}
	delete(r.branches, branch)
	w.WriteHeader(http.StatusNoContent)
}

func (r *Repo) handleListCommits(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	query := req.URL.Query()
	ref := query.Get("sha")
	if ref == "" {
		ref = r.defaultBranch
	}
	start, ok := r.resolveLocked(ref)
	if !ok {
		WriteError(w, http.StatusNotFound, "No commit found for SHA: "+ref)
		return
	}
	perPage := defaultPerPage
	if n, err := strconv.Atoi(query.Get("per_page")); err == nil && n > 0 {
		perPage = min(n, 100)
	}
	path := query.Get("path")
	out := []map[string]any{}
	for _, c := range r.ancestorsLocked(start) {
		if len(out) == perPage {
			break
		}
		if path != "" && !r.touchesLocked(c, path) {
			continue
		}
		out = append(out, map[string]any{"sha": c.sha, "commit": map[string]string{"message": c.message}})
	}
	WriteJSON(w, http.StatusOK, out)
}

func (r *Repo) handleCompare(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	baseRef, headRef, ok := strings.Cut(req.PathValue("basehead"), "...")
	base, baseOK := r.resolveLocked(baseRef)
	head, headOK := r.resolveLocked(headRef)
	if !ok || !baseOK || !headOK {
		WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	mergeBase := r.mergeBaseLocked(base.sha, head.sha)
	if mergeBase == nil {
		WriteError(w, http.StatusNotFound, "No common ancestor between "+baseRef+" and "+headRef+".")
		return
	}
	var status string
	switch {
	case base.sha == head.sha:
		status = "identical"
	case mergeBase.sha == base.sha:
		status = "ahead"
	case mergeBase.sha == head.sha:
		status = "behind"
	default:
		status = "diverged"
	}
	inBase := map[string]bool{}
	for _, c := range r.ancestorsLocked(mergeBase) {
		inBase[c.sha] = true
	}
	var ahead []*fakeCommit
	for _, c := range r.ancestorsLocked(head) {
		if !inBase[c.sha] {
			ahead = append(ahead, c)
		}
	}
	slices.Reverse(ahead)
	perPage := maxComparePerPage
	if n, err := strconv.Atoi(req.URL.Query().Get("per_page")); err == nil && n > 0 {
		perPage = min(n, maxComparePerPage)
	}
	commits := []any{}
	for _, c := range ahead[:min(len(ahead), perPage)] {
		commits = append(commits, commitJSON(c))
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"status":            status,
		"ahead_by":          len(ahead),
		"behind_by":         len(r.ancestorsLocked(base)) - len(r.ancestorsLocked(mergeBase)),
		"total_commits":     len(ahead),
		"merge_base_commit": map[string]string{"sha": mergeBase.sha},
		"commits":           commits,
	})
}

func (r *Repo) handleGetCommit(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.resolveLocked(req.PathValue("sha"))
	if !ok {
		WriteError(w, http.StatusUnprocessableEntity, "No commit found for SHA: "+req.PathValue("sha"))
		return
	}
	WriteJSON(w, http.StatusOK, commitJSON(c))
}

func (r *Repo) handleCommitPulls(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sha := req.PathValue("sha")
	if _, ok := r.commits[sha]; !ok {
		WriteError(w, http.StatusUnprocessableEntity, "No commit found for SHA: "+sha)
		return
	}
	out := []any{}
	for _, p := range r.pulls {
		if p.mergeCommit == sha || r.headLocked(p) == sha {
			out = append(out, r.pullJSONLocked(p))
		}
	}
	WriteJSON(w, http.StatusOK, out)
}

func commitJSON(c *fakeCommit) map[string]any {
	parents := make([]map[string]string, 0, len(c.parents))
	for _, p := range c.parents {
		parents = append(parents, map[string]string{"sha": p})
	}
	return map[string]any{"sha": c.sha, "commit": map[string]string{"message": c.message}, "parents": parents}
}

func (r *Repo) handleListPulls(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	query := req.URL.Query()
	state := query.Get("state")
	if state == "" {
		state = "open"
	}
	head := query.Get("head")
	if owner, branch, ok := strings.Cut(head, ":"); ok {
		if !strings.EqualFold(owner, r.owner) {
			WriteJSON(w, http.StatusOK, []any{})
			return
		}
		head = branch
	}
	out := []any{}
	pulls := slices.Clone(r.pulls)
	if query.Get("direction") != "asc" {
		slices.Reverse(pulls)
	}
	for _, p := range pulls {
		if (state != "all" && p.state != state) || (head != "" && p.head != head) || (query.Get("base") != "" && p.base != query.Get("base")) {
			continue
		}
		out = append(out, r.pullJSONLocked(p))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (r *Repo) handleCreatePull(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		WriteError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	head := body.Head
	if _, branch, ok := strings.Cut(head, ":"); ok {
		head = branch
	}
	headSHA, headOK := r.branches[head]
	baseSHA, baseOK := r.branches[body.Base]
	if !headOK || !baseOK || body.Title == "" {
		WriteError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	for _, p := range r.pulls {
		if p.head == head && p.base == body.Base && p.state == "open" {
			WriteError(w, http.StatusUnprocessableEntity, "A pull request already exists for "+r.owner+":"+head+".")
			return
		}
	}
	if len(r.changedFilesLocked(baseSHA, headSHA)) == 0 {
		WriteError(w, http.StatusUnprocessableEntity, "No commits between "+body.Base+" and "+head)
		return
	}
	p := &fakePull{
		number:   len(r.pulls) + 1,
		title:    body.Title,
		body:     body.Body,
		head:     head,
		base:     body.Base,
		state:    "open",
		headSHA:  headSHA,
		headRepo: r.FullName(),
	}
	r.pulls = append(r.pulls, p)
	WriteJSON(w, http.StatusCreated, r.pullJSONLocked(p))
}

func (r *Repo) handleGetPull(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pullFromRequestLocked(req)
	if p == nil {
		WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	WriteJSON(w, http.StatusOK, r.pullJSONLocked(p))
}

func (r *Repo) handlePatchPull(w http.ResponseWriter, req *http.Request) {
	var body struct {
		State *string `json:"state"`
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		WriteError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pullFromRequestLocked(req)
	if p == nil {
		WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	if body.Title != nil {
		p.title = *body.Title
	}
	if body.Body != nil {
		p.body = *body.Body
	}
	if body.State != nil && *body.State == "closed" && p.state == "open" {
		p.headSHA = r.headLocked(p)
		p.state = "closed"
	}
	WriteJSON(w, http.StatusOK, r.pullJSONLocked(p))
}

func (r *Repo) handleMergePull(w http.ResponseWriter, req *http.Request) {
	var body struct {
		CommitTitle   string `json:"commit_title"`
		CommitMessage string `json:"commit_message"`
		SHA           string `json:"sha"`
		MergeMethod   string `json:"merge_method"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		WriteError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pullFromRequestLocked(req)
	if p == nil {
		WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	if p.state != "open" {
		WriteError(w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
		return
	}
	head := r.headLocked(p)
	base := r.branches[p.base]
	switch {
	case body.SHA != "" && body.SHA != head:
		WriteError(w, http.StatusConflict, "Head branch was modified. Review and try the merge again.")
		return
	case body.MergeMethod != "squash":
		WriteError(w, http.StatusMethodNotAllowed, "Merge commits are not allowed on this repository.")
		return
	case r.blockedMerges > 0:
		r.blockedMerges--
		WriteError(w, http.StatusMethodNotAllowed, "Required status check \"ci\" is expected.")
		return
	case r.requireUpToDate && !r.isAncestorLocked(base, head):
		WriteError(w, http.StatusMethodNotAllowed, "Head branch is not up to date with the base branch.")
		return
	}
	tree, ok := r.mergeLocked(base, head)
	if !ok {
		WriteError(w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
		return
	}
	title := body.CommitTitle
	if title == "" {
		title = p.title + " (#" + strconv.Itoa(p.number) + ")"
	}
	message := title
	if body.CommitMessage != "" {
		message += "\n\n" + body.CommitMessage
	}
	c := r.newCommitLocked(message, []string{base}, tree)
	r.branches[p.base] = c.sha
	p.headSHA = head
	p.state = "closed"
	p.merged = true
	p.mergeCommit = c.sha
	WriteJSON(w, http.StatusOK, map[string]any{"sha": c.sha, "merged": true, "message": "Pull Request successfully merged"})
}

func (r *Repo) handleUpdateBranch(w http.ResponseWriter, req *http.Request) {
	var body struct {
		ExpectedHeadSHA string `json:"expected_head_sha"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		WriteError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pullFromRequestLocked(req)
	if p == nil || p.state != "open" {
		WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	head := r.headLocked(p)
	base := r.branches[p.base]
	switch {
	case body.ExpectedHeadSHA != "" && body.ExpectedHeadSHA != head:
		WriteError(w, http.StatusUnprocessableEntity, "expected head sha didn't match current head ref.")
		return
	case r.isAncestorLocked(base, head):
		WriteError(w, http.StatusUnprocessableEntity, "There are no new commits on the base branch.")
		return
	}
	tree, ok := r.mergeLocked(base, head)
	if !ok {
		WriteError(w, http.StatusUnprocessableEntity, "merge conflict between base and head")
		return
	}
	c := r.newCommitLocked("Merge branch '"+p.base+"' into "+p.head, []string{head, base}, tree)
	r.branches[p.head] = c.sha
	p.headSHA = c.sha
	WriteJSON(w, http.StatusAccepted, map[string]string{"message": "Updating pull request branch."})
}

func (r *Repo) pullJSONLocked(p *fakePull) map[string]any {
	head := r.headLocked(p)
	base := r.branches[p.base]
	var mergedAt, mergeCommit any
	if p.merged {
		mergedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		mergeCommit = p.mergeCommit
	}
	var headRepo any
	if p.headRepo != "" {
		headRepo = map[string]string{"full_name": p.headRepo}
	}
	return map[string]any{
		"number":           p.number,
		"state":            p.state,
		"merged":           p.merged,
		"merged_at":        mergedAt,
		"mergeable_state":  r.mergeableStateLocked(p, base, head),
		"merge_commit_sha": mergeCommit,
		"title":            p.title,
		"body":             p.body,
		"html_url":         "https://github.test/" + r.FullName() + "/pull/" + strconv.Itoa(p.number),
		"changed_files":    len(r.changedFilesLocked(base, head)),
		"head":             map[string]any{"ref": p.head, "sha": head, "repo": headRepo},
		"base":             map[string]any{"ref": p.base, "sha": base},
		"labels":           labelsJSON(p.labels),
	}
}

func labelsJSON(labels []string) []map[string]string {
	out := make([]map[string]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, map[string]string{"name": l})
	}
	return out
}

func (r *Repo) mergeableStateLocked(p *fakePull, base, head string) string {
	switch {
	case p.state != "open":
		return "unknown"
	case r.mergeableState != "":
		return r.mergeableState
	}
	if _, ok := r.mergeLocked(base, head); !ok {
		return "dirty"
	}
	switch {
	case r.requireUpToDate && !r.isAncestorLocked(base, head):
		return "behind"
	case r.blockedMerges > 0:
		return "blocked"
	default:
		return "clean"
	}
}

func (r *Repo) pullFromRequestLocked(req *http.Request) *fakePull {
	number, err := strconv.Atoi(req.PathValue("number"))
	if err != nil {
		return nil
	}
	return r.pullLocked(number)
}

func (r *Repo) pullLocked(number int) *fakePull {
	if number < 1 || number > len(r.pulls) {
		return nil
	}
	return r.pulls[number-1]
}

func (r *Repo) headLocked(p *fakePull) string {
	if p.state == "open" {
		if sha, ok := r.branches[p.head]; ok {
			return sha
		}
	}
	return p.headSHA
}

func (r *Repo) newCommitLocked(message string, parents []string, files map[string][]byte) *fakeCommit {
	r.seq++
	c := &fakeCommit{sha: objectID(fmt.Appendf(nil, "commit %d\x00%s\x00%s", r.seq, strings.Join(parents, ","), message)), message: message, parents: parents, files: files, seq: r.seq}
	r.commits[c.sha] = c
	return c
}

func (r *Repo) resolveLocked(ref string) (*fakeCommit, bool) {
	if sha, ok := r.branches[ref]; ok {
		ref = sha
	}
	c, ok := r.commits[ref]
	return c, ok
}

func (r *Repo) ancestorsLocked(start *fakeCommit) []*fakeCommit {
	seen := map[string]bool{start.sha: true}
	queue := []*fakeCommit{start}
	var out []*fakeCommit
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		out = append(out, c)
		for _, parent := range c.parents {
			if !seen[parent] {
				seen[parent] = true
				queue = append(queue, r.commits[parent])
			}
		}
	}
	slices.SortFunc(out, func(a, b *fakeCommit) int { return b.seq - a.seq })
	return out
}

func (r *Repo) isAncestorLocked(ancestor, descendant string) bool {
	c, ok := r.commits[descendant]
	if !ok {
		return false
	}
	return slices.ContainsFunc(r.ancestorsLocked(c), func(a *fakeCommit) bool { return a.sha == ancestor })
}

func (r *Repo) mergeBaseLocked(a, b string) *fakeCommit {
	ca, okA := r.commits[a]
	cb, okB := r.commits[b]
	if !okA || !okB {
		return nil
	}
	inB := map[string]bool{}
	for _, c := range r.ancestorsLocked(cb) {
		inB[c.sha] = true
	}
	for _, c := range r.ancestorsLocked(ca) {
		if inB[c.sha] {
			return c
		}
	}
	return nil
}

func (r *Repo) touchesLocked(c *fakeCommit, path string) bool {
	content, present := c.files[path]
	if len(c.parents) == 0 {
		return present
	}
	for _, parent := range c.parents {
		before, had := r.commits[parent].files[path]
		if had == present && bytes.Equal(before, content) {
			return false
		}
	}
	return true
}

func (r *Repo) changedFilesLocked(base, head string) []string {
	mb := r.mergeBaseLocked(base, head)
	hc, ok := r.commits[head]
	if mb == nil || !ok {
		return nil
	}
	var changed []string
	for path := range unionKeys(mb.files, hc.files) {
		if !sameFile(mb.files, hc.files, path) {
			changed = append(changed, path)
		}
	}
	slices.Sort(changed)
	return changed
}

func (r *Repo) mergeLocked(base, head string) (map[string][]byte, bool) {
	mb := r.mergeBaseLocked(base, head)
	bc, okB := r.commits[base]
	hc, okH := r.commits[head]
	if mb == nil || !okB || !okH {
		return nil, false
	}
	tree := map[string][]byte{}
	for path := range unionKeys(mb.files, bc.files, hc.files) {
		var from map[string][]byte
		switch {
		case sameFile(hc.files, mb.files, path), sameFile(bc.files, hc.files, path):
			from = bc.files
		case sameFile(bc.files, mb.files, path):
			from = hc.files
		default:
			return nil, false
		}
		if content, ok := from[path]; ok {
			tree[path] = content
		}
	}
	return tree, true
}

func unionKeys(trees ...map[string][]byte) map[string]bool {
	out := map[string]bool{}
	for _, tree := range trees {
		for path := range tree {
			out[path] = true
		}
	}
	return out
}

func sameFile(a, b map[string][]byte, path string) bool {
	ca, okA := a[path]
	cb, okB := b[path]
	return okA == okB && bytes.Equal(ca, cb)
}

func blobSHA(content []byte) string {
	return objectID(append(fmt.Appendf(nil, "blob %d\x00", len(content)), content...))
}

func objectID(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:40]
}

func wrapBase64(content []byte) string {
	encoded := base64.StdEncoding.EncodeToString(content)
	var b strings.Builder
	for len(encoded) > base64LineLength {
		b.WriteString(encoded[:base64LineLength])
		b.WriteByte('\n')
		encoded = encoded[base64LineLength:]
	}
	b.WriteString(encoded)
	b.WriteByte('\n')
	return b.String()
}
