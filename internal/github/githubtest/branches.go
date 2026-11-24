package githubtest

import (
	"net/http"
	"strings"
)

func (s *Server) SetBranch(owner, repo, branch, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.branches[branchKey(owner, repo, branch)] = sha
}

func (s *Server) DeleteBranch(owner, repo, branch string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.branches, branchKey(owner, repo, branch))
}

func (s *Server) BranchCalls() int64 {
	return s.branchCalls.Load()
}

func (s *Server) registerBranches() {
	s.Handle("GET /repos/{owner}/{repo}/git/ref/heads/{branch...}", http.HandlerFunc(s.handleBranchRef))
}

func (s *Server) handleBranchRef(w http.ResponseWriter, r *http.Request) {
	s.branchCalls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	owner, repo, branch := r.PathValue("owner"), r.PathValue("repo"), r.PathValue("branch")
	installationID, _ := InstallationID(r)
	if id, ok := s.repos[repoKey(owner, repo)]; !ok || id != installationID {
		WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	sha, ok := s.branches[branchKey(owner, repo, branch)]
	if !ok {
		WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"ref":    "refs/heads/" + branch,
		"object": map[string]string{"type": "commit", "sha": sha},
	})
}

func branchKey(owner, repo, branch string) string {
	return strings.ToLower(owner+"/"+repo) + ":" + branch
}
