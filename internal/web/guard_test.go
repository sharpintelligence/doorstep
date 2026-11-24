package web

import (
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/core"
)

var wantRoutes = map[string]core.Role{
	"GET /{$}":                      core.RoleViewer,
	"GET /apps/{app}":               core.RoleViewer,
	"GET /deployments/{id}":         core.RoleViewer,
	"GET /locks":                    core.RoleViewer,
	"GET /audit":                    core.RoleViewer,
	"GET /tokens":                   core.RoleAdmin,
	"POST /apps/{app}/deployments":  core.RoleDeployer,
	"POST /deployments/{id}/cancel": core.RoleDeployer,
	"POST /apps/{app}/rollback":     core.RoleDeployer,
	"POST /apps/{app}/block":        core.RoleDeployer,
	"POST /apps/{app}/unblock":      core.RoleDeployer,
	"POST /apps/{app}/lock":         core.RoleDeployer,
	"POST /apps/{app}/unlock":       core.RoleDeployer,
	"POST /locks/global":            core.RoleDeployer,
	"POST /locks/global/release":    core.RoleDeployer,
	"POST /tokens":                  core.RoleAdmin,
	"POST /tokens/{name}/revoke":    core.RoleAdmin,
	"GET /static/{file}":            "",
	"/":                             "",
}

var routePaths = map[string]string{
	"GET /{$}":                      "/",
	"GET /apps/{app}":               "/apps/blog",
	"GET /deployments/{id}":         "/deployments/5",
	"GET /locks":                    "/locks",
	"GET /audit":                    "/audit",
	"GET /tokens":                   "/tokens",
	"POST /apps/{app}/deployments":  "/apps/blog/deployments",
	"POST /deployments/{id}/cancel": "/deployments/5/cancel",
	"POST /apps/{app}/rollback":     "/apps/blog/rollback",
	"POST /apps/{app}/block":        "/apps/blog/block",
	"POST /apps/{app}/unblock":      "/apps/blog/unblock",
	"POST /apps/{app}/lock":         "/apps/blog/lock",
	"POST /apps/{app}/unlock":       "/apps/blog/unlock",
	"POST /locks/global":            "/locks/global",
	"POST /locks/global/release":    "/locks/global/release",
	"POST /tokens":                  "/tokens",
	"POST /tokens/{name}/revoke":    "/tokens/ci/revoke",
	"GET /static/{file}":            "/static/app.css",
	"/":                             "/no/such/page",
}

func TestRouteTableRoles(t *testing.T) {
	t.Parallel()

	h, err := New(newFakes().options(io.Discard))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := map[string]core.Role{}
	for _, rt := range h.routes() {
		if _, dup := got[rt.pattern]; dup {
			t.Errorf("route %s registered twice", rt.pattern)
		}
		got[rt.pattern] = rt.role
	}
	if !maps.Equal(got, wantRoutes) {
		t.Fatalf("routes = %v\nwant %v", got, wantRoutes)
	}
	if routes := h.Routes(); !slices.Equal(slices.Sorted(maps.Keys(routes)), slices.Sorted(maps.Keys(wantRoutes))) {
		t.Fatalf("Routes() patterns = %v", slices.Sorted(maps.Keys(routes)))
	}

	opts := newFakes().options(io.Discard)
	opts.Deploys = nil
	h, err = New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, rt := range h.routes() {
		if strings.HasSuffix(rt.pattern, "/deployments") || strings.HasSuffix(rt.pattern, "/cancel") {
			t.Errorf("route %s registered without a deploy service", rt.pattern)
		}
	}
}

func TestEveryRouteEnforcesItsRole(t *testing.T) {
	t.Parallel()

	principals := map[string]*auth.Principal{"anonymous": nil, "viewer": &viewer, "reporter": &reporter, "deployer": &deployer, "admin": &admin}
	for pattern, role := range wantRoutes {
		method, _, hasMethod := strings.Cut(pattern, " ")
		if !hasMethod {
			method = http.MethodGet
		}
		for name, principal := range principals {
			t.Run(pattern+" as "+name, func(t *testing.T) {
				t.Parallel()
				f := newFakes()
				mux := newMux(t, f.options(io.Discard))
				c := call{method: method, path: routePaths[pattern], principal: principal}
				if method == http.MethodPost {
					c.form = url.Values{"reason": {"x"}}
				}
				rec := serve(t, mux, c)
				switch {
				case role == "":
					if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden || rec.Code == http.StatusFound {
						t.Fatalf("public route answered %d", rec.Code)
					}
				case principal == nil && method == http.MethodGet:
					if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), auth.LoginPath+"?return_to=") {
						t.Fatalf("anonymous GET = %d %q, want a redirect to sign-in", rec.Code, rec.Header().Get("Location"))
					}
				case principal == nil:
					if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
						t.Fatalf("anonymous POST = %d %s, want an HTML 401", rec.Code, rec.Header().Get("Content-Type"))
					}
				case !principal.Allows(role):
					if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "needs the "+string(role)+" role") {
						t.Fatalf("%s = %d, want an HTML 403: %s", name, rec.Code, rec.Body.String())
					}
				default:
					if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden || rec.Code == http.StatusFound {
						t.Fatalf("%s = %d, want access", name, rec.Code)
					}
				}
				if called := len(f.deploys.deploys) + len(f.deploys.cancels) + len(f.rollbacks.requests) + len(f.rollbacks.unblocks) + len(f.rollbacks.blockReqs) +
					len(f.locks.requests) + len(f.locks.unlocks) + len(f.tokens.created) + len(f.tokens.revoked); called > 0 && (principal == nil || !principal.Allows(role)) {
					t.Fatalf("a denied request reached a service")
				}
			})
		}
	}
}

func TestAnonymousRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		signIn       bool
		c            call
		wantStatus   int
		wantRedirect string
		wantBody     string
	}{
		{
			name:   "htmx poll after the session expired",
			signIn: true,
			c: call{path: "/apps/blog", header: map[string]string{
				"HX-Request": "true", "HX-Current-URL": "https://doorstep.example/apps/blog?before=9",
			}},
			wantStatus:   http.StatusUnauthorized,
			wantRedirect: "/auth/login?return_to=%2Fapps%2Fblog%3Fbefore%3D9",
			wantBody:     "Your session has ended",
		},
		{
			name:   "htmx poll with a hostile current url",
			signIn: true,
			c: call{path: "/locks", header: map[string]string{
				"HX-Request": "true", "HX-Current-URL": "https://evil.example//evil.example/x",
			}},
			wantStatus:   http.StatusUnauthorized,
			wantRedirect: "/auth/login?return_to=%2F",
		},
		{
			name:         "htmx poll without sign-in configured",
			c:            call{path: "/locks", header: map[string]string{"HX-Request": "true"}},
			wantStatus:   http.StatusUnauthorized,
			wantRedirect: "/",
		},
		{
			name:       "page without sign-in configured",
			c:          call{path: "/"},
			wantStatus: http.StatusUnauthorized,
			wantBody:   "needs a sign-in provider",
		},
		{
			name:       "post without sign-in configured",
			c:          call{method: http.MethodPost, path: "/apps/blog/lock", form: url.Values{"reason": {"x"}}},
			wantStatus: http.StatusUnauthorized,
			wantBody:   "needs a sign-in provider",
		},
		{
			name:       "post after the session expired",
			signIn:     true,
			c:          call{method: http.MethodPost, path: "/apps/blog/lock", form: url.Values{"reason": {"x"}}},
			wantStatus: http.StatusUnauthorized,
			wantBody:   "nothing was changed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakes()
			opts := f.options(io.Discard)
			opts.SignIn = tt.signIn
			rec := serve(t, newMux(t, opts), tt.c)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("HX-Redirect"); got != tt.wantRedirect {
				t.Errorf("HX-Redirect = %q, want %q", got, tt.wantRedirect)
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body does not contain %q:\n%s", tt.wantBody, rec.Body.String())
			}
			if len(f.locks.requests) != 0 {
				t.Errorf("anonymous request locked an app")
			}
			assertSecurityHeaders(t, rec)
		})
	}
}

func assertSecurityHeaders(t *testing.T, rec interface{ Header() http.Header }) {
	t.Helper()
	want := map[string]string{
		"Content-Security-Policy": contentSecurityPolicy,
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "same-origin",
		"Cache-Control":           "no-store",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestSecurityHeadersOnEveryHTMLResponse(t *testing.T) {
	t.Parallel()

	f := newFakes()
	mux := newMux(t, f.options(io.Discard))
	for _, c := range []call{
		{path: "/", principal: &viewer},
		{path: "/apps/blog", principal: &viewer},
		{path: "/no/such/page"},
		{path: "/tokens", principal: &viewer},
		{method: http.MethodPost, path: "/apps/blog/unlock", form: url.Values{}, principal: &deployer},
	} {
		rec := serve(t, mux, c)
		assertSecurityHeaders(t, rec)
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'self'") {
			t.Errorf("%s: missing CSP", c.path)
		}
	}
}

func TestNewValidatesOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(o *Options)
		want   string
	}{
		{name: "no history", mutate: func(o *Options) { o.History = nil }, want: "history is required"},
		{name: "no tokens", mutate: func(o *Options) { o.Tokens = nil }, want: "tokens is required"},
		{name: "no login wrapper", mutate: func(o *Options) { o.RequireLogin = nil }, want: "require login is required"},
		{name: "duplicate app", mutate: func(o *Options) { o.Apps = []App{blogApp, blogApp} }, want: "duplicate app"},
		{name: "app name mismatch", mutate: func(o *Options) { o.Apps = []App{{Name: "shop", Target: blog}} }, want: "must match"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := newFakes().options(io.Discard)
			tt.mutate(&opts)
			if _, err := New(opts); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New error = %v, want %q", err, tt.want)
			}
		})
	}
}
