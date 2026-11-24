package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/buildinfo"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/deploys"
	"github.com/sharpintelligence/doorstep/internal/rollback"
	"github.com/sharpintelligence/doorstep/internal/store"
)

var (
	testNow  = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	blog     = core.AppTarget("blog")
	blogApp  = App{Name: "blog", Target: blog, Repository: "acme/blog", Branch: "main", GitOpsRepository: "acme/homelab", ArgoCDApplication: "argocd/blog"}
	shaApp   = strings.Repeat("a", 40)
	shaPin   = strings.Repeat("b", 40)
	digestA  = core.Digest("sha256:" + strings.Repeat("1", 64))
	digestB  = core.Digest("sha256:" + strings.Repeat("2", 64))
	alice    = core.Actor{Kind: core.ActorUser, ID: "oidc:alice", Name: "alice"}
	viewer   = auth.Principal{Actor: alice, Role: core.RoleViewer, Method: auth.MethodSession}
	reporter = auth.Principal{Actor: alice, Role: core.RoleReporter, Method: auth.MethodSession}
	deployer = auth.Principal{Actor: alice, Role: core.RoleDeployer, Method: auth.MethodSession}
	admin    = auth.Principal{Actor: alice, Role: core.RoleAdmin, Method: auth.MethodSession}
)

func deployment(id core.DeploymentID, state core.State) core.Deployment {
	created := testNow.Add(-time.Hour)
	return core.Deployment{
		ID: id, Target: blog, Kind: core.KindDeploy, State: state, Source: core.SourceWebhook, RequestedBy: "octocat",
		Commit: core.Commit{Repository: "acme/blog", SHA: shaApp}, Image: core.ImageRef{Repository: "ghcr.io/acme/blog", Tag: shaApp},
		CreatedAt: created, UpdatedAt: created,
	}
}

type fakeHistory struct {
	mu        sync.Mutex
	pages     map[core.DeploymentID]store.DeploymentPage
	histories map[core.DeploymentID]store.DeploymentHistory
	current   *core.Deployment
	filters   []store.DeploymentFilter
	bulk      int
	err       error
}

func (f *fakeHistory) ListDeployments(_ context.Context, filter store.DeploymentFilter) (store.DeploymentPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filters = append(f.filters, filter)
	if f.err != nil {
		return store.DeploymentPage{}, f.err
	}
	page := f.pages[filter.Before]
	if filter.Limit > 0 && len(page.Items) > filter.Limit {
		page.Items = page.Items[:filter.Limit]
	}
	return page, nil
}

func (f *fakeHistory) GetDeploymentHistory(_ context.Context, id core.DeploymentID) (store.DeploymentHistory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.histories[id]
	if !ok {
		return store.DeploymentHistory{}, fmt.Errorf("deployment %d: %w", id, core.ErrNotFound)
	}
	return h, nil
}

func (f *fakeHistory) CurrentDeployment(context.Context, core.Target) (core.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.current == nil {
		return core.Deployment{}, core.ErrNotFound
	}
	return *f.current, nil
}

func (f *fakeHistory) CurrentDeployments(context.Context) ([]core.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bulk++
	if f.current == nil {
		return nil, f.err
	}
	return []core.Deployment{*f.current}, f.err
}

func (f *fakeHistory) LatestDeployments(context.Context) ([]core.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bulk++
	newest := f.pages[0].Items
	if len(newest) == 0 {
		return nil, f.err
	}
	return newest[:1], f.err
}

type fakeQueue struct {
	mu      sync.Mutex
	entries []core.QueueEntry
	apps    []string
}

func (f *fakeQueue) Entries(_ context.Context, app string) ([]core.QueueEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apps = append(f.apps, app)
	return f.entries, nil
}

type fakeLocks struct {
	mu        sync.Mutex
	locks     []core.Lock
	events    []core.LockEvent
	requests  []core.LockRequest
	unlocks   []string
	lockErr   error
	unlockErr error
}

func (f *fakeLocks) Lock(_ context.Context, req core.LockRequest) (core.Lock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	return core.Lock{}, f.lockErr
}

func (f *fakeLocks) Unlock(_ context.Context, target *core.Target, actor string) (core.Lock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unlocks = append(f.unlocks, core.LockRequest{Target: target}.Scope()+" by "+actor)
	return core.Lock{}, f.unlockErr
}

func (f *fakeLocks) List(context.Context) ([]core.Lock, error) {
	return f.locks, nil
}

func (f *fakeLocks) ListLockEvents(_ context.Context, limit int) ([]core.LockEvent, error) {
	return f.events[:min(limit, len(f.events))], nil
}

type fakeRollbacks struct {
	mu          sync.Mutex
	requests    []rollback.Request
	rollbackErr func(req rollback.Request) error
	blocks      []core.Block
	unblocks    []core.BlockRef
	unblockErr  error
}

func (f *fakeRollbacks) Rollback(_ context.Context, req rollback.Request) (rollback.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.rollbackErr != nil {
		if err := f.rollbackErr(req); err != nil {
			return rollback.Result{}, err
		}
	}
	return rollback.Result{Rollback: deployment(31, core.StateQueued)}, nil
}

func (f *fakeRollbacks) ListBlocks(context.Context, core.Target) ([]core.Block, error) {
	return f.blocks, nil
}

func (f *fakeRollbacks) Unblock(_ context.Context, _ core.Target, ref core.BlockRef, _ core.Actor, _ core.Role) ([]core.Block, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unblocks = append(f.unblocks, ref)
	return nil, f.unblockErr
}

type fakeDeploys struct {
	mu        sync.Mutex
	deploys   []deploys.Request
	cancels   []deploys.CancelRequest
	created   bool
	deployErr error
	cancelErr error
}

func (f *fakeDeploys) Deploy(_ context.Context, req deploys.Request) (deploys.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deploys = append(f.deploys, req)
	if f.deployErr != nil {
		return deploys.Result{}, f.deployErr
	}
	return deploys.Result{Deployment: deployment(41, core.StateQueued), Created: f.created}, nil
}

func (f *fakeDeploys) Cancel(_ context.Context, req deploys.CancelRequest) (core.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, req)
	if f.cancelErr != nil {
		return core.Deployment{}, f.cancelErr
	}
	return deployment(req.ID, core.StateCancelled), nil
}

type fakeAudit struct {
	mu      sync.Mutex
	entries []core.AuditEntry
	filters []store.AuditFilter
}

func (f *fakeAudit) ListAudit(_ context.Context, filter store.AuditFilter) ([]core.AuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filters = append(f.filters, filter)
	return f.entries, nil
}

type fakeTokens struct {
	mu        sync.Mutex
	tokens    []store.APIToken
	created   []auth.CreateTokenInput
	revoked   []string
	createErr error
	revokeErr error
}

func (f *fakeTokens) Create(_ context.Context, in auth.CreateTokenInput) (string, store.APIToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, in)
	if f.createErr != nil {
		return "", store.APIToken{}, f.createErr
	}
	return "dsp_secretvalue", store.APIToken{ID: 1, Name: in.Name, Role: in.Role, CreatedBy: in.By.Actor.String(), CreatedAt: testNow}, nil
}

func (f *fakeTokens) List(context.Context) ([]store.APIToken, error) {
	return f.tokens, nil
}

func (f *fakeTokens) Revoke(_ context.Context, name string, _ auth.Requester) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, name)
	return f.revokeErr
}

type fakes struct {
	history   *fakeHistory
	queue     *fakeQueue
	locks     *fakeLocks
	rollbacks *fakeRollbacks
	deploys   *fakeDeploys
	audit     *fakeAudit
	tokens    *fakeTokens
}

func newFakes() *fakes {
	return &fakes{
		history:   &fakeHistory{pages: map[core.DeploymentID]store.DeploymentPage{}, histories: map[core.DeploymentID]store.DeploymentHistory{}},
		queue:     &fakeQueue{},
		locks:     &fakeLocks{},
		rollbacks: &fakeRollbacks{},
		deploys:   &fakeDeploys{created: true},
		audit:     &fakeAudit{},
		tokens:    &fakeTokens{},
	}
}

func fakeRequireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.PrincipalFrom(r.Context()); !ok {
			http.Redirect(w, r, auth.LoginPath+"?return_to="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (f *fakes) options(logs io.Writer) Options {
	return Options{
		Logger:       slog.New(slog.NewTextHandler(logs, nil)),
		Now:          func() time.Time { return testNow },
		Apps:         []App{blogApp},
		History:      f.history,
		Queue:        f.queue,
		Locks:        f.locks,
		LockEvents:   f.locks,
		Rollbacks:    f.rollbacks,
		Blocks:       f.rollbacks,
		Deploys:      f.deploys,
		Audit:        f.audit,
		Tokens:       f.tokens,
		RequireLogin: fakeRequireLogin,
		SignIn:       true,
		CommitURL: func(repository, sha string) string {
			return "https://github.example/" + repository + "/commit/" + sha
		},
		Build: buildinfo.Info{Version: "v1.2.3"},
	}
}

func newMux(t *testing.T, opts Options) *http.ServeMux {
	t.Helper()
	h, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	for pattern, handler := range h.Routes() {
		mux.Handle(pattern, handler)
	}
	return mux
}

type call struct {
	method    string
	path      string
	form      url.Values
	principal *auth.Principal
	header    map[string]string
}

func serve(t *testing.T, mux http.Handler, c call) *httptest.ResponseRecorder {
	t.Helper()
	method := c.method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader = http.NoBody
	if c.form != nil {
		body = strings.NewReader(c.form.Encode())
	}
	r := httptest.NewRequestWithContext(t.Context(), method, c.path, body)
	if c.form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range c.header {
		r.Header.Set(k, v)
	}
	if c.principal != nil {
		r = r.WithContext(auth.WithPrincipal(r.Context(), *c.principal))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}
