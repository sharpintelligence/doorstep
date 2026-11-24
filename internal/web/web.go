package web

import (
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/sharpintelligence/doorstep/internal/buildinfo"
	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	pollInterval  = "every 5s"
	historyLimit  = 25
	auditLimit    = 50
	lockEventsMax = 50
)

type App struct {
	Name              string
	Target            core.Target
	Repository        string
	Branch            string
	GitOpsRepository  string
	ArgoCDApplication string
}

type Options struct {
	Logger       *slog.Logger
	Now          func() time.Time
	Apps         []App
	History      History
	Queue        Queue
	Locks        Locks
	LockEvents   LockEvents
	Rollbacks    Rollbacks
	Blocks       Blocks
	Deploys      Deploys
	Audit        Audit
	Tokens       Tokens
	RequireLogin func(http.Handler) http.Handler
	SignIn       bool
	CommitURL    func(repository, sha string) string
	Build        buildinfo.Info
}

type Handler struct {
	logger       *slog.Logger
	now          func() time.Time
	apps         []App
	byName       map[string]App
	history      History
	queue        Queue
	locks        Locks
	lockEvents   LockEvents
	rollbacks    Rollbacks
	blocks       Blocks
	deploys      Deploys
	audit        Audit
	tokens       Tokens
	requireLogin func(http.Handler) http.Handler
	signIn       bool
	commitURL    func(repository, sha string) string
	build        buildinfo.Info
	static       staticFiles
	pages        map[string]*template.Template
}

func New(opts Options) (*Handler, error) {
	var errs []error
	required := []struct {
		name    string
		present bool
	}{
		{"history", opts.History != nil},
		{"queue", opts.Queue != nil},
		{"locks", opts.Locks != nil},
		{"lock events", opts.LockEvents != nil},
		{"rollbacks", opts.Rollbacks != nil},
		{"blocks", opts.Blocks != nil},
		{"audit", opts.Audit != nil},
		{"tokens", opts.Tokens != nil},
		{"require login", opts.RequireLogin != nil},
	}
	for _, dep := range required {
		if !dep.present {
			errs = append(errs, fmt.Errorf("web: %s is required", dep.name))
		}
	}
	h := &Handler{
		logger:       opts.Logger,
		now:          opts.Now,
		byName:       make(map[string]App, len(opts.Apps)),
		history:      opts.History,
		queue:        opts.Queue,
		locks:        opts.Locks,
		lockEvents:   opts.LockEvents,
		rollbacks:    opts.Rollbacks,
		blocks:       opts.Blocks,
		deploys:      opts.Deploys,
		audit:        opts.Audit,
		tokens:       opts.Tokens,
		requireLogin: opts.RequireLogin,
		signIn:       opts.SignIn,
		commitURL:    opts.CommitURL,
		build:        opts.Build,
	}
	for i, app := range opts.Apps {
		if err := app.Target.Validate(); err != nil || app.Name != app.Target.App {
			errs = append(errs, fmt.Errorf("web: apps[%d] %q: name must match a valid target", i, app.Name))
			continue
		}
		if _, dup := h.byName[app.Name]; dup {
			errs = append(errs, fmt.Errorf("web: apps[%d]: duplicate app %q", i, app.Name))
			continue
		}
		h.byName[app.Name] = app
		h.apps = append(h.apps, app)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if h.logger == nil {
		h.logger = slog.New(slog.DiscardHandler)
	}
	if h.now == nil {
		h.now = time.Now
	}
	static, err := loadStatic()
	if err != nil {
		return nil, err
	}
	h.static = static
	if h.pages, err = h.parseTemplates(); err != nil {
		return nil, err
	}
	return h, nil
}

type route struct {
	pattern string
	role    core.Role
	asset   bool
	handler http.HandlerFunc
}

func (h *Handler) routes() []route {
	routes := []route{
		{pattern: "GET /{$}", role: core.RoleViewer, handler: h.overview},
		{pattern: "GET /apps/{app}", role: core.RoleViewer, handler: h.appPage},
		{pattern: "GET /deployments/{id}", role: core.RoleViewer, handler: h.deploymentPage},
		{pattern: "GET /locks", role: core.RoleViewer, handler: h.locksPage},
		{pattern: "GET /audit", role: core.RoleViewer, handler: h.auditPage},
		{pattern: "GET /tokens", role: core.RoleAdmin, handler: h.tokensPage},
		{pattern: "POST /apps/{app}/rollback", role: core.RoleDeployer, handler: h.rollback},
		{pattern: "POST /apps/{app}/unblock", role: core.RoleDeployer, handler: h.unblock},
		{pattern: "POST /apps/{app}/lock", role: core.RoleDeployer, handler: h.lockApp},
		{pattern: "POST /apps/{app}/unlock", role: core.RoleDeployer, handler: h.unlockApp},
		{pattern: "POST /locks/global", role: core.RoleDeployer, handler: h.lockGlobal},
		{pattern: "POST /locks/global/release", role: core.RoleDeployer, handler: h.unlockGlobal},
		{pattern: "POST /tokens", role: core.RoleAdmin, handler: h.createToken},
		{pattern: "POST /tokens/{name}/revoke", role: core.RoleAdmin, handler: h.revokeToken},
		{pattern: "GET /static/{file}", asset: true, handler: h.serveStatic},
		{pattern: "/", handler: h.notFound},
	}
	if h.deploys != nil {
		routes = append(routes,
			route{pattern: "POST /apps/{app}/deployments", role: core.RoleDeployer, handler: h.deploy},
			route{pattern: "POST /deployments/{id}/cancel", role: core.RoleDeployer, handler: h.cancel},
		)
	}
	return routes
}

func (h *Handler) Routes() map[string]http.Handler {
	routes := h.routes()
	out := make(map[string]http.Handler, len(routes))
	for _, rt := range routes {
		out[rt.pattern] = h.guard(rt)
	}
	return out
}
