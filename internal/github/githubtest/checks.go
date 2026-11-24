package githubtest

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
)

const (
	defaultPerPage = 30
	maxPerPage     = 100
)

type CheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	HTMLURL    string `json:"html_url,omitempty"`
	AppSlug    string `json:"-"`
}

type Status struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description,omitempty"`
	TargetURL   string `json:"target_url,omitempty"`
}

type CheckSuite struct {
	ID                   int64  `json:"id"`
	Status               string `json:"status"`
	Conclusion           string `json:"conclusion,omitempty"`
	LatestCheckRunsCount int    `json:"latest_check_runs_count"`
	AppSlug              string `json:"-"`
}

type commitChecks struct {
	runs     []CheckRun
	statuses []Status
	suites   []CheckSuite
}

func (s *Server) SetCheckRuns(owner, repo, sha string, runs ...CheckRun) {
	s.updateCommit(owner, repo, sha, func(c *commitChecks) { c.runs = slices.Clone(runs) })
}

func (s *Server) SetStatuses(owner, repo, sha string, statuses ...Status) {
	s.updateCommit(owner, repo, sha, func(c *commitChecks) { c.statuses = slices.Clone(statuses) })
}

func (s *Server) SetCheckSuites(owner, repo, sha string, suites ...CheckSuite) {
	s.updateCommit(owner, repo, sha, func(c *commitChecks) { c.suites = slices.Clone(suites) })
}

func (s *Server) ChecksCalls() int64 {
	return s.checksCalls.Load()
}

func (s *Server) updateCommit(owner, repo, sha string, update func(*commitChecks)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := commitKey(owner, repo, sha)
	c := s.commits[key]
	if c == nil {
		c = &commitChecks{}
		s.commits[key] = c
	}
	update(c)
}

func (s *Server) registerChecks() {
	s.Handle("GET /repos/{owner}/{repo}/commits/{sha}/check-runs", http.HandlerFunc(s.handleCheckRuns))
	s.Handle("GET /repos/{owner}/{repo}/commits/{sha}/status", http.HandlerFunc(s.handleCombinedStatus))
	s.Handle("GET /repos/{owner}/{repo}/commits/{sha}/check-suites", http.HandlerFunc(s.handleCheckSuites))
}

func (s *Server) handleCheckRuns(w http.ResponseWriter, r *http.Request) {
	c, ok := s.commitFor(w, r)
	if !ok {
		return
	}
	runs := c.runs
	if r.URL.Query().Get("filter") != "all" {
		runs = latestRuns(runs)
	}
	items := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		items = append(items, map[string]any{
			"id": run.ID, "name": run.Name, "status": run.Status, "conclusion": nullable(run.Conclusion),
			"html_url": run.HTMLURL, "app": map[string]string{"slug": run.AppSlug},
		})
	}
	page, ok := paginate(w, r, items)
	if ok {
		WriteJSON(w, http.StatusOK, map[string]any{"total_count": len(items), "check_runs": page})
	}
}

func (s *Server) handleCombinedStatus(w http.ResponseWriter, r *http.Request) {
	c, ok := s.commitFor(w, r)
	if !ok {
		return
	}
	page, ok := paginate(w, r, c.statuses)
	if ok {
		WriteJSON(w, http.StatusOK, map[string]any{
			"state":       combinedState(c.statuses),
			"sha":         r.PathValue("sha"),
			"total_count": len(c.statuses),
			"statuses":    page,
		})
	}
}

func (s *Server) handleCheckSuites(w http.ResponseWriter, r *http.Request) {
	c, ok := s.commitFor(w, r)
	if !ok {
		return
	}
	items := make([]map[string]any, 0, len(c.suites))
	for _, suite := range c.suites {
		items = append(items, map[string]any{
			"id": suite.ID, "status": suite.Status, "conclusion": nullable(suite.Conclusion),
			"latest_check_runs_count": suite.LatestCheckRunsCount, "app": map[string]string{"slug": suite.AppSlug},
		})
	}
	page, ok := paginate(w, r, items)
	if ok {
		WriteJSON(w, http.StatusOK, map[string]any{"total_count": len(items), "check_suites": page})
	}
}

func (s *Server) commitFor(w http.ResponseWriter, r *http.Request) (commitChecks, bool) {
	s.checksCalls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	installationID, _ := InstallationID(r)
	if id, ok := s.repos[repoKey(owner, repo)]; !ok || id != installationID {
		WriteError(w, http.StatusNotFound, "Not Found")
		return commitChecks{}, false
	}
	c, ok := s.commits[commitKey(owner, repo, r.PathValue("sha"))]
	if !ok {
		WriteError(w, http.StatusUnprocessableEntity, "No commit found for SHA: "+r.PathValue("sha"))
		return commitChecks{}, false
	}
	return commitChecks{
		runs:     slices.Clone(c.runs),
		statuses: slices.Clone(c.statuses),
		suites:   slices.Clone(c.suites),
	}, true
}

func paginate[T any](w http.ResponseWriter, r *http.Request, items []T) ([]T, bool) {
	perPage, err := queryInt(r, "per_page", defaultPerPage)
	if err != nil || perPage < 1 {
		WriteError(w, http.StatusUnprocessableEntity, "Invalid per_page")
		return nil, false
	}
	perPage = min(perPage, maxPerPage)
	page, err := queryInt(r, "page", 1)
	if err != nil || page < 1 {
		WriteError(w, http.StatusUnprocessableEntity, "Invalid page")
		return nil, false
	}
	start := min((page-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	return append([]T{}, items[start:end]...), true
}

func queryInt(r *http.Request, name string, fallback int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	return strconv.Atoi(raw)
}

func latestRuns(runs []CheckRun) []CheckRun {
	latest := map[string]CheckRun{}
	var order []string
	for _, run := range runs {
		current, seen := latest[run.Name]
		if !seen {
			order = append(order, run.Name)
		}
		if !seen || run.ID > current.ID {
			latest[run.Name] = run
		}
	}
	out := make([]CheckRun, 0, len(order))
	for _, name := range order {
		out = append(out, latest[name])
	}
	return out
}

func combinedState(statuses []Status) string {
	state := "success"
	if len(statuses) == 0 {
		return "pending"
	}
	for _, st := range statuses {
		switch st.State {
		case "error", "failure":
			return "failure"
		case "pending":
			state = "pending"
		}
	}
	return state
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func commitKey(owner, repo, sha string) string {
	return strings.ToLower(owner+"/"+repo) + "@" + sha
}
