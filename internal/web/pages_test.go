package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const hostile = `<script>alert(1)</script>`

func richFakes() *fakes {
	f := newFakes()
	current := deployment(7, core.StateSucceeded)
	current.Digest, current.PinCommitSHA, current.GitOpsRepository, current.ArgoCDRevision = digestA, shaPin, "acme/homelab", shaPin
	f.history.current = &current
	running := deployment(9, core.StateSyncing)
	running.Digest, running.PreviousDigest, running.PinCommitSHA, running.GitOpsRepository, running.ArgoCDRevision = digestB, digestA, shaPin, "acme/homelab", "HEAD"
	running.Migration, running.MigrationReason = true, "label db-migration on #4"
	rolledBack := deployment(8, core.StateSucceeded)
	rb := core.DeploymentID(6)
	rolledBack.Kind, rolledBack.RollbackOfID, rolledBack.Digest = core.KindRollback, &rb, digestA
	f.history.pages[0] = store.DeploymentPage{Items: []core.Deployment{running, rolledBack, current}, Next: 7}
	f.history.pages[7] = store.DeploymentPage{Items: []core.Deployment{deployment(3, core.StateFailed)}}
	started := testNow.Add(-10 * time.Minute)
	finished := testNow.Add(-5 * time.Minute)
	f.history.histories[9] = store.DeploymentHistory{
		Deployment: running,
		Events: []core.Event{
			{ID: 1, Type: core.EventTransition, To: core.StateQueued, Actor: "octocat", OccurredAt: started},
			{ID: 2, Type: core.EventTransition, From: core.StateQueued, To: core.StateResolving, Actor: "worker", Message: hostile, OccurredAt: started},
		},
		EventsTruncated: true,
		Gates: []core.GateResult{
			{
				DeploymentID: 9, Name: "smoke", Type: "external", Phase: core.GatePhaseBefore, Status: core.GateFailed, Message: hostile,
				Evidence:  json.RawMessage(`{"logs_url":"https://ci.example/run/1","url":"javascript:alert(1)","note":"<b>x</b>"}`),
				StartedAt: started, FinishedAt: &finished,
			},
			{DeploymentID: 9, Name: "soak", Type: "soak", Phase: core.GatePhaseAfter, Status: core.GatePending, StartedAt: started},
		},
		Rollbacks: []core.Deployment{deployment(12, core.StateQueued)},
	}
	f.history.histories[7] = store.DeploymentHistory{Deployment: current}
	f.queue.entries = []core.QueueEntry{
		{Position: 1, Deployment: running, Priority: core.PriorityNormal, EnqueuedAt: started, AvailableAt: started},
		{Position: 2, Deployment: deployment(10, core.StateQueued), Priority: core.PriorityUrgent, EnqueuedAt: started, AvailableAt: started},
		{Position: 1, Deployment: core.Deployment{ID: 99, Target: core.AppTarget("other"), State: core.StateQueued}},
	}
	expires := testNow.Add(2 * time.Hour)
	f.locks.locks = []core.Lock{
		{Global: true, Kind: core.LockFreeze, Owner: "user:oidc:bob", Reason: "release " + hostile, CreatedAt: started},
		{Target: blog, Kind: core.LockFreeze, Owner: "user:oidc:carol", Reason: "db work", CreatedAt: started, ExpiresAt: &expires},
	}
	f.locks.events = []core.LockEvent{{Action: core.LockActionLocked, Lock: f.locks.locks[1], Actor: "user:oidc:carol", OccurredAt: started}}
	f.rollbacks.blocks = []core.Block{{ID: 2, Target: blog, Digest: digestB, Reason: "bad build", CreatedBy: "user:oidc:bob", CreatedAt: started}}
	dep := core.DeploymentID(9)
	f.audit.entries = []core.AuditEntry{{ID: 77, OccurredAt: started, Actor: alice, Role: core.RoleDeployer, Action: core.AuditDeployRequest, Target: &blog, DeploymentID: &dep, Detail: json.RawMessage(`{"commit":"` + shaApp + `"}`)}}
	f.tokens.tokens = []store.APIToken{{ID: 1, Name: "ci", Role: core.RoleReporter, CreatedBy: "cli:alice", CreatedAt: started}}
	return f
}

func get(t *testing.T, path string, principal auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	mux := newMux(t, richFakes().options(io.Discard))
	return serve(t, mux, call{path: path, principal: &principal})
}

func body(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return rec.Body.String()
}

func TestPagesRender(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		path      string
		principal auth.Principal
		want      []string
		reject    []string
	}{
		{
			name: "overview",
			path: "/",
			want: []string{
				`<a href="/apps/blog">blog</a>`,
				`href="https://github.example/acme/blog/commit/` + shaApp + `"`,
				"1 waiting, 1 in flight",
				"All apps are locked by user:oidc:bob",
				`hx-get="/" hx-select="#live" hx-swap="outerHTML" hx-trigger="every 5s"`,
				`<time datetime="2026-11-24T11:50:00Z">10 minutes ago</time>`,
				`<span class="badge ok">succeeded</span>`,
			},
			reject: []string{hostile},
		},
		{
			name:      "app as deployer",
			path:      "/apps/blog",
			principal: deployer,
			want: []string{
				`href="https://github.example/acme/homelab/commit/` + shaPin + `"`,
				`<code>HEAD</code>`,
				`<a href="/apps/blog?before=7">Older deployments</a>`,
				`rollback of <a href="/deployments/6">#6</a>`,
				`<span class="badge warn" title="label db-migration on #4">migration</span>`,
				`<span class="badge active">in flight</span>`,
				"urgent",
				`action="/apps/blog/deployments"`,
				`action="/apps/blog/unblock"`,
				`name="ref" value="` + string(digestB) + `"`,
				`action="/apps/blog/unlock"`,
				`name="to_deployment_id" value="8"`,
				"blog is locked by user:oidc:carol: db work",
				`hx-get="/apps/blog"`,
			},
			reject: []string{"/deployments/99", hostile},
		},
		{
			name: "app as viewer has no actions",
			path: "/apps/blog",
			want: []string{"Older deployments"},
			reject: []string{
				`action="/apps/blog/deployments"`, `action="/apps/blog/rollback"`, `action="/apps/blog/unblock"`, `action="/apps/blog/lock"`, `action="/apps/blog/unlock"`,
			},
		},
		{
			name: "second history page",
			path: "/apps/blog?before=7",
			want: []string{"#3", `<a href="/apps/blog">Newest deployments</a>`, `hx-get="/apps/blog?before=7"`},
		},
		{
			name:      "running deployment",
			path:      "/deployments/9",
			principal: deployer,
			want: []string{
				"&lt;script&gt;alert(1)&lt;/script&gt;",
				`<a href="https://ci.example/run/1" rel="noopener noreferrer">logs_url</a>`,
				"&lt;b&gt;x&lt;/b&gt;",
				"before gates", "after gates",
				`<span class="badge bad">failed</span>`,
				`<span class="badge active">pending</span>`,
				"Only the newest events are shown",
				`href="https://github.example/acme/homelab/commit/` + shaPin + `"`,
				`<dt>Argo CD revision</dt><dd><code>HEAD</code></dd>`,
				`<a href="/deployments/12">#12</a>`,
				`action="/deployments/9/cancel"`,
				"the version stays pinned until a rollback",
				`hx-trigger="every 5s"`,
				"label db-migration on #4",
			},
			reject: []string{hostile, `href="javascript:`, ">url</a>"},
		},
		{
			name:   "finished deployment does not poll",
			path:   "/deployments/7",
			want:   []string{`<span class="badge ok">succeeded</span>`, `<div id="live">`},
			reject: []string{"hx-get", "hx-trigger", `action="/deployments/7/cancel"`},
		},
		{
			name:      "locks",
			path:      "/locks",
			principal: deployer,
			want:      []string{"All apps are locked by user:oidc:bob: release &lt;script&gt;", `action="/locks/global/release"`, `action="/apps/blog/unlock"`, "db work", "locked"},
			reject:    []string{hostile, `action="/locks/global"`},
		},
		{
			name: "audit",
			path: "/audit?action=deploy.request&app=blog",
			want: []string{"deploy.request", `<option value="deploy.request" selected>`, `<option value="blog" selected>`, "&#34;commit&#34;", `<a href="/deployments/9">#9</a>`},
		},
		{
			name:      "tokens",
			path:      "/tokens",
			principal: admin,
			want:      []string{"ci", "reporter", `action="/tokens/ci/revoke"`, `action="/tokens"`, `<a href="/tokens" aria-current="page">Tokens</a>`},
		},
		{
			name:   "navigation hides tokens from non-admins",
			path:   "/locks",
			reject: []string{`href="/tokens"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			principal := tt.principal
			if principal.Role == "" {
				principal = viewer
			}
			resp := get(t, tt.path, principal)
			html := body(t, resp)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d:\n%s", resp.Code, html)
			}
			for _, want := range tt.want {
				if !strings.Contains(html, want) {
					t.Errorf("page does not contain %q", want)
				}
			}
			for _, reject := range tt.reject {
				if strings.Contains(html, reject) {
					t.Errorf("page contains %q", reject)
				}
			}
			if t.Failed() {
				t.Logf("page:\n%s", html)
			}
		})
	}
}

func TestPageErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path       string
		wantStatus int
		want       string
	}{
		{path: "/apps/shop", wantStatus: http.StatusNotFound, want: "No app with this name"},
		{path: "/apps/blog?before=x", wantStatus: http.StatusBadRequest, want: "page cursor"},
		{path: "/deployments/404", wantStatus: http.StatusNotFound, want: "Deployment 404 does not exist"},
		{path: "/deployments/abc", wantStatus: http.StatusNotFound, want: "positive whole numbers"},
		{path: "/audit?action=DROP", wantStatus: http.StatusBadRequest, want: "action filter"},
		{path: "/audit?app=shop", wantStatus: http.StatusBadRequest, want: "app filter"},
		{path: "/audit?before=-1", wantStatus: http.StatusBadRequest, want: "page cursor"},
		{path: "/nowhere", wantStatus: http.StatusNotFound, want: "There is no page at this address"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			resp := get(t, tt.path, viewer)
			html := body(t, resp)
			if resp.Code != tt.wantStatus || !strings.Contains(html, tt.want) {
				t.Fatalf("GET %s = %d, want %d containing %q:\n%s", tt.path, resp.Code, tt.wantStatus, tt.want, html)
			}
		})
	}
}

func TestNoticesAreFixedMessages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		query string
		want  string
	}{
		{query: "?notice=deploy_queued", want: `<p class="notice" role="status">Deploy queued.</p>`},
		{query: "?notice=" + hostile, want: ""},
		{query: "?notice=unknown", want: ""},
	}
	for _, tt := range tests {
		html := body(t, get(t, "/locks"+tt.query, viewer))
		switch {
		case tt.want != "" && !strings.Contains(html, tt.want):
			t.Errorf("%s: notice missing:\n%s", tt.query, html)
		case tt.want == "" && strings.Contains(html, `class="notice"`):
			t.Errorf("%s: unexpected notice", tt.query)
		}
		if strings.Contains(html, hostile) || strings.Contains(html, "unknown") {
			t.Errorf("%s: query text reflected into the page", tt.query)
		}
	}
}

var (
	formPattern = regexp.MustCompile(`(?s)<form\b[^>]*>.*?</form>`)
	postPattern = regexp.MustCompile(`<form\b[^>]*method="post"`)
)

func TestEveryPostFormCarriesTheCSRFToken(t *testing.T) {
	t.Parallel()

	paths := []string{"/", "/apps/blog", "/apps/blog?before=7", "/deployments/9", "/deployments/7", "/locks", "/audit", "/tokens"}
	forms := 0
	for _, path := range paths {
		html := body(t, get(t, path, admin))
		for _, form := range formPattern.FindAllString(html, -1) {
			if !postPattern.MatchString(form) {
				continue
			}
			forms++
			if !strings.Contains(form, `name="csrf_token"`) {
				t.Errorf("%s: POST form without csrf_token:\n%s", path, form)
			}
		}
		if strings.Count(html, "<form") != len(formPattern.FindAllString(html, -1)) {
			t.Errorf("%s: unbalanced forms", path)
		}
	}
	if forms < 10 {
		t.Fatalf("found only %d POST forms; the test is not looking at the pages", forms)
	}
}

func TestEvidenceLinks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want []link
	}{
		{raw: `{"logs_url":"https://ci.example/1","url":"http://gate.example/x"}`, want: []link{{URL: "https://ci.example/1", Text: "logs_url"}, {URL: "http://gate.example/x", Text: "url"}}},
		{raw: `{"url":"javascript:alert(1)"}`},
		{raw: `{"url":"data:text/html,hi"}`},
		{raw: `{"url":"//evil.example/x"}`},
		{raw: `{"url":"https://user:pw@evil.example/"}`},
		{raw: `{"url":42}`},
		{raw: `{"link":"https://ci.example/1"}`},
		{raw: `[1,2]`},
		{raw: ``},
	}
	for _, tt := range tests {
		got := evidenceLinks(json.RawMessage(tt.raw))
		if len(got) != len(tt.want) {
			t.Errorf("evidenceLinks(%s) = %v, want %v", tt.raw, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("evidenceLinks(%s) = %v, want %v", tt.raw, got, tt.want)
			}
		}
	}
}

func TestRelativeTimes(t *testing.T) {
	t.Parallel()

	h := &Handler{now: func() time.Time { return testNow }}
	future := testNow.Add(3 * time.Hour)
	tests := []struct {
		in   any
		want string
	}{
		{in: testNow.Add(-30 * time.Second), want: "just now"},
		{in: testNow.Add(-time.Minute), want: "1 minute ago"},
		{in: testNow.Add(-90 * time.Minute), want: "1 hour ago"},
		{in: testNow.Add(-72 * time.Hour), want: "3 days ago"},
		{in: &future, want: "in 3 hours"},
		{in: (*time.Time)(nil), want: ""},
		{in: time.Time{}, want: ""},
		{in: "yesterday", want: ""},
	}
	for _, tt := range tests {
		if got := h.relTime(tt.in); got != tt.want {
			t.Errorf("relTime(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestLinksWithoutCommitURL(t *testing.T) {
	t.Parallel()

	opts := richFakes().options(io.Discard)
	opts.CommitURL = nil
	rec := serve(t, newMux(t, opts), call{path: "/deployments/9", principal: &viewer})
	if html := rec.Body.String(); strings.Contains(html, "github.example") || !strings.Contains(html, "<code>"+shortSHA(shaApp)+"</code>") {
		t.Fatalf("commit links rendered without a commit URL:\n%s", html)
	}
}

var (
	detailsPattern     = regexp.MustCompile(`(?s)<details\b[^>]*>.*?</details>`)
	destructivePattern = regexp.MustCompile(`action="[^"]*/(rollback|unblock|unlock|release|revoke|cancel)"`)
	livePattern        = regexp.MustCompile(`(?s)<div id="live"[^>]*>.*?</div>`)
)

func pageAs(t *testing.T, f *fakes, path string, principal auth.Principal) string {
	t.Helper()
	rec := serve(t, newMux(t, f.options(io.Discard)), call{path: path, principal: &principal})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d:\n%s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func unlockedFakes() *fakes {
	f := richFakes()
	f.locks.locks = nil
	return f
}

func TestDestructiveActionsNeedASecondStep(t *testing.T) {
	t.Parallel()

	pages := []struct {
		fakes func() *fakes
		path  string
	}{
		{richFakes, "/apps/blog"},
		{unlockedFakes, "/apps/blog"},
		{richFakes, "/deployments/9"},
		{richFakes, "/locks"},
		{richFakes, "/tokens"},
	}
	seen := map[string]int{}
	rollbackForms := 0
	for _, page := range pages {
		html := pageAs(t, page.fakes(), page.path, admin)
		var disclosed []string
		for _, d := range detailsPattern.FindAllString(html, -1) {
			disclosed = append(disclosed, formPattern.FindAllString(d, -1)...)
		}
		for _, form := range formPattern.FindAllString(html, -1) {
			action := destructivePattern.FindStringSubmatch(form)
			if action == nil {
				continue
			}
			seen[action[1]]++
			if !slices.Contains(disclosed, form) {
				t.Errorf("%s: %s form is a single click, not inside <details>:\n%s", page.path, action[1], form)
			}
			if action[1] == "rollback" {
				rollbackForms++
				if !strings.Contains(form, `name="reason"`) {
					t.Errorf("%s: rollback form without a reason field:\n%s", page.path, form)
				}
			}
		}
	}
	for _, action := range []string{"rollback", "unblock", "unlock", "release", "revoke", "cancel"} {
		if seen[action] == 0 {
			t.Errorf("no %s form found; the test is not looking at the pages", action)
		}
	}
	if rollbackForms < 3 {
		t.Errorf("found %d rollback forms, want the app form and one per successful deployment", rollbackForms)
	}
}

func TestStateDependentActionsAreRefreshed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fakes   func() *fakes
		path    string
		inLive  []string
		outside []string
	}{
		{
			name: "locked app", fakes: richFakes, path: "/apps/blog",
			inLive:  []string{`action="/apps/blog/unlock"`, `action="/apps/blog/unblock"`, `name="to_deployment_id" value="8"`},
			outside: []string{`action="/apps/blog/deployments"`, `id="rollback_reason"`},
		},
		{
			name: "unlocked app", fakes: unlockedFakes, path: "/apps/blog",
			inLive: []string{`action="/apps/blog/lock"`},
		},
		{
			name: "running deployment", fakes: richFakes, path: "/deployments/9",
			inLive: []string{`action="/deployments/9/cancel"`},
		},
		{
			name: "global lock held", fakes: richFakes, path: "/locks",
			inLive: []string{`action="/locks/global/release"`, `action="/apps/blog/unlock"`},
		},
		{
			name: "no global lock", fakes: unlockedFakes, path: "/locks",
			inLive: []string{`action="/locks/global"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			html := pageAs(t, tt.fakes(), tt.path, deployer)
			live := livePattern.FindString(html)
			for _, want := range tt.inLive {
				if !strings.Contains(live, want) {
					t.Errorf("%s is not in the refreshed region", want)
				}
			}
			for _, want := range tt.outside {
				if !strings.Contains(html, want) || strings.Contains(live, want) {
					t.Errorf("%s should be on the page but outside the refreshed region", want)
				}
			}
		})
	}
}

func TestOverviewQueriesDoNotGrowWithApps(t *testing.T) {
	t.Parallel()

	f := richFakes()
	opts := f.options(io.Discard)
	for _, name := range []string{"shop", "wiki", "mail"} {
		opts.Apps = append(opts.Apps, App{Name: name, Target: core.AppTarget(name), Repository: "acme/" + name, Branch: "main"})
	}
	rec := serve(t, newMux(t, opts), call{path: "/", principal: &viewer})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !slices.Equal(f.queue.apps, []string{""}) {
		t.Errorf("queue reads = %q, want one read of every app", f.queue.apps)
	}
	if f.history.bulk != 2 || len(f.history.filters) != 0 {
		t.Errorf("history reads = %d bulk and %d per app, want 2 bulk and none per app", f.history.bulk, len(f.history.filters))
	}
	html := rec.Body.String()
	for _, name := range []string{"shop", "wiki", "mail"} {
		if !strings.Contains(html, `<a href="/apps/`+name+`">`+name+`</a>`) {
			t.Errorf("overview does not list %s", name)
		}
	}
	if strings.Count(html, "0 waiting") != 3 || !strings.Contains(html, "1 waiting, 1 in flight") {
		t.Errorf("queue counts are not per app:\n%s", html)
	}
}
