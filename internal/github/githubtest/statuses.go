package githubtest

import (
	"encoding/json"
	"net/http"
	"slices"
)

type StatusReply struct {
	Code    int
	Message string
	Header  http.Header
}

type StatusHook func(r *http.Request, st Status) *StatusReply

func (s *Server) OnCreateStatus(hook StatusHook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusHook = hook
}

func (s *Server) PostedStatuses(owner, repo, sha string) []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.posted[commitKey(owner, repo, sha)])
}

func (s *Server) StatusCalls() int64 {
	return s.statusCalls.Load()
}

func (s *Server) registerStatuses() {
	s.Handle("POST /repos/{owner}/{repo}/statuses/{sha}", http.HandlerFunc(s.handleCreateStatus))
}

func (s *Server) handleCreateStatus(w http.ResponseWriter, r *http.Request) {
	s.statusCalls.Add(1)
	owner, repo, sha := r.PathValue("owner"), r.PathValue("repo"), r.PathValue("sha")
	var st Status
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		WriteError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	s.mu.Lock()
	hook := s.statusHook
	installationID, _ := InstallationID(r)
	id, installed := s.repos[repoKey(owner, repo)]
	s.mu.Unlock()
	if !installed || id != installationID {
		WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	if hook != nil {
		if reply := hook(r, st); reply != nil {
			for key, values := range reply.Header {
				w.Header()[key] = values
			}
			WriteError(w, reply.Code, reply.Message)
			return
		}
	}
	switch st.State {
	case "pending", "success", "failure", "error":
	default:
		WriteError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	s.mu.Lock()
	key := commitKey(owner, repo, sha)
	s.posted[key] = append(s.posted[key], st)
	c := s.commits[key]
	if c == nil {
		c = &commitChecks{}
		s.commits[key] = c
	}
	i := slices.IndexFunc(c.statuses, func(existing Status) bool { return existing.Context == st.Context })
	if i >= 0 {
		c.statuses[i] = st
	} else {
		c.statuses = append(c.statuses, st)
	}
	s.mu.Unlock()
	WriteJSON(w, http.StatusCreated, st)
}
