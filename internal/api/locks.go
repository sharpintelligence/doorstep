package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

type lockRequest struct {
	Reason    string     `json:"reason"`
	ExpiresIn *string    `json:"expires_in"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type lockResponse struct {
	Scope     string     `json:"scope"`
	App       string     `json:"app,omitempty"`
	Owner     string     `json:"owner"`
	Reason    string     `json:"reason"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type lockListResponse struct {
	Locks []lockResponse `json:"locks"`
}

type lockConflictResponse struct {
	Error errorBody    `json:"error"`
	Lock  lockResponse `json:"lock"`
}

const (
	lockPath    = Prefix + "lock"
	locksPath   = Prefix + "locks"
	appLockPath = Prefix + "apps/{app}/lock"
)

const (
	scopeApp    = "app"
	scopeGlobal = "global"
)

func (a *api) registerLocks(mux *http.ServeMux) {
	mux.Handle("POST "+appLockPath, a.authenticated(core.RoleDeployer, a.lockApp))
	mux.Handle("DELETE "+appLockPath, a.authenticated(core.RoleDeployer, a.unlockApp))
	mux.HandleFunc(appLockPath, methodNotAllowed(http.MethodPost, http.MethodDelete))
	mux.Handle("POST "+lockPath, a.authenticated(core.RoleDeployer, a.lockGlobal))
	mux.Handle("DELETE "+lockPath, a.authenticated(core.RoleDeployer, a.unlockGlobal))
	mux.HandleFunc(lockPath, methodNotAllowed(http.MethodPost, http.MethodDelete))
	mux.Handle("GET "+locksPath, a.authenticated(core.RoleViewer, a.listLocks))
	mux.HandleFunc(locksPath, methodNotAllowed(http.MethodGet, http.MethodHead))
}

func toLockResponse(l core.Lock) lockResponse {
	resp := lockResponse{
		Scope:     scopeApp,
		App:       l.Target.App,
		Owner:     l.Owner,
		Reason:    l.Reason,
		CreatedAt: l.CreatedAt.UTC(),
	}
	if l.Global {
		resp.Scope = scopeGlobal
		resp.App = ""
	}
	if l.ExpiresAt != nil {
		at := l.ExpiresAt.UTC()
		resp.ExpiresAt = &at
	}
	return resp
}

func (a *api) lockApp(w http.ResponseWriter, r *http.Request, actor string) {
	app := r.PathValue("app")
	if !a.known(app) {
		writeError(w, http.StatusNotFound, codeNotFound, "unknown app "+app)
		return
	}
	target := core.AppTarget(app)
	a.lock(w, r, actor, &target)
}

func (a *api) lockGlobal(w http.ResponseWriter, r *http.Request, actor string) {
	a.lock(w, r, actor, nil)
}

func (a *api) unlockApp(w http.ResponseWriter, r *http.Request, actor string) {
	target := core.AppTarget(r.PathValue("app"))
	a.unlock(w, r, actor, &target)
}

func (a *api) unlockGlobal(w http.ResponseWriter, r *http.Request, actor string) {
	a.unlock(w, r, actor, nil)
}

func (a *api) lock(w http.ResponseWriter, r *http.Request, actor string, target *core.Target) {
	var body lockRequest
	if reqErr := decodeJSON(w, r, &body); reqErr != nil {
		writeError(w, reqErr.status, reqErr.code, reqErr.message)
		return
	}
	req := core.LockRequest{Target: target, Owner: actor, Reason: body.Reason, ExpiresAt: body.ExpiresAt}
	if body.ExpiresIn != nil {
		if body.ExpiresAt != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "set at most one of expires_in and expires_at")
			return
		}
		d, err := time.ParseDuration(*body.ExpiresIn)
		if err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, `expires_in must be a duration with a unit, such as "2h" or "30m"`)
			return
		}
		req.ExpiresIn = &d
	}

	lock, err := a.locks.Lock(r.Context(), req)
	if held, ok := errors.AsType[*core.LockHeldError](err); ok {
		writeJSON(w, http.StatusConflict, lockConflictResponse{
			Error: errorBody{Code: codeLocked, Message: held.Error()},
			Lock:  toLockResponse(held.Lock),
		})
		return
	}
	if errors.Is(err, core.ErrInvalidLock) {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	if errors.Is(err, core.ErrLockContended) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, codeContended, "the lock changed while it was being taken; retry")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toLockResponse(lock))
}

func (a *api) unlock(w http.ResponseWriter, r *http.Request, actor string, target *core.Target) {
	_, err := a.locks.Unlock(r.Context(), target, actor)
	if errors.Is(err, core.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeNotFound, core.LockRequest{Target: target}.Scope()+" is not locked")
		return
	}
	if errors.Is(err, core.ErrInvalidLock) {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) listLocks(w http.ResponseWriter, r *http.Request, _ string) {
	locks, err := a.locks.List(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	resp := lockListResponse{Locks: make([]lockResponse, 0, len(locks))}
	for _, l := range locks {
		resp.Locks = append(resp.Locks, toLockResponse(l))
	}
	writeJSON(w, http.StatusOK, resp)
}
