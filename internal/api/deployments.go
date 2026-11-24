package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const (
	paramEnvironment = "environment"
	paramState       = "state"
	paramKind        = "kind"
	paramCommit      = "commit"
	paramDigest      = "digest"
	paramLimit       = "limit"
	paramCursor      = "cursor"
)

const (
	appDeploymentsPath = Prefix + "apps/{app}/deployments"
	deploymentPath     = Prefix + "deployments/{id}"
)

func (a *api) registerHistory(mux *http.ServeMux) {
	mux.HandleFunc("GET "+appDeploymentsPath, a.listAppDeployments)
	mux.HandleFunc(appDeploymentsPath, methodNotAllowed(http.MethodGet, http.MethodHead))
	mux.HandleFunc("GET "+deploymentPath, a.getDeployment)
	mux.HandleFunc(deploymentPath, methodNotAllowed(http.MethodGet, http.MethodHead))
}

var listParams = []string{paramEnvironment, paramState, paramKind, paramCommit, paramDigest, paramLimit, paramCursor}

func (a *api) listAppDeployments(w http.ResponseWriter, r *http.Request) {
	filter, err := parseListFilter(r.PathValue("app"), r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	page, err := a.history.ListDeployments(r.Context(), filter)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newDeploymentList(page))
}

func (a *api) getDeployment(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "deployment id must be a positive integer")
		return
	}
	if len(r.URL.Query()) > 0 {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "this endpoint takes no query parameters")
		return
	}
	h, err := a.history.GetDeploymentHistory(r.Context(), id)
	if errors.Is(err, core.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeNotFound, fmt.Sprintf("deployment %d not found", id))
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newDeploymentDetail(h))
}

func parseListFilter(app string, q url.Values) (store.DeploymentFilter, error) {
	if strings.TrimSpace(app) == "" {
		return store.DeploymentFilter{}, errors.New("app must not be blank")
	}
	for name := range q {
		if !slices.Contains(listParams, name) {
			return store.DeploymentFilter{}, fmt.Errorf("unknown query parameter %q", name)
		}
	}
	for _, name := range []string{paramEnvironment, paramCommit, paramDigest, paramLimit, paramCursor} {
		if len(q[name]) > 1 {
			return store.DeploymentFilter{}, fmt.Errorf("query parameter %q may only be given once", name)
		}
	}

	f := store.DeploymentFilter{App: app}
	if q.Has(paramEnvironment) {
		f.Environment = q.Get(paramEnvironment)
		if strings.TrimSpace(f.Environment) == "" {
			return store.DeploymentFilter{}, errors.New("environment must not be blank")
		}
	}

	states, err := parseList(q, paramState, core.ParseState)
	if err != nil {
		return store.DeploymentFilter{}, err
	}
	f.States = states

	kinds, err := parseList(q, paramKind, func(s string) (core.Kind, error) {
		if k := core.Kind(s); k.Valid() {
			return k, nil
		}
		return "", fmt.Errorf("unknown deployment kind %q", s)
	})
	if err != nil {
		return store.DeploymentFilter{}, err
	}
	f.Kinds = kinds

	if q.Has(paramCommit) {
		f.CommitSHA = q.Get(paramCommit)
		if err := core.ValidateCommitSHA(f.CommitSHA); err != nil {
			return store.DeploymentFilter{}, err
		}
	}

	if q.Has(paramDigest) {
		f.Digest = core.Digest(q.Get(paramDigest))
		if err := f.Digest.Validate(); err != nil {
			return store.DeploymentFilter{}, err
		}
	}

	if q.Has(paramLimit) {
		limit, err := strconv.Atoi(q.Get(paramLimit))
		if err != nil || limit < 1 || limit > store.MaxPageSize {
			return store.DeploymentFilter{}, fmt.Errorf("limit must be an integer from 1 to %d", store.MaxPageSize)
		}
		f.Limit = limit
	}

	if q.Has(paramCursor) {
		before, err := parseID(q.Get(paramCursor))
		if err != nil {
			return store.DeploymentFilter{}, errors.New("cursor must be a next_cursor value from a previous page")
		}
		f.Before = before
	}
	return f, nil
}

func parseList[T comparable](q url.Values, name string, parse func(string) (T, error)) ([]T, error) {
	var out []T
	for _, value := range q[name] {
		for part := range strings.SplitSeq(value, ",") {
			if part == "" {
				return nil, fmt.Errorf("query parameter %q has an empty value", name)
			}
			v, err := parse(part)
			if err != nil {
				return nil, err
			}
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out, nil
}

func parseID(s string) (core.DeploymentID, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != s {
		return 0, fmt.Errorf("invalid id %q", s)
	}
	return core.DeploymentID(id), nil
}
