package auth

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type fakeToken struct {
	store.APIToken
	hash [32]byte
}

type fakeLogin struct {
	store.Login
	expiresAt time.Time
}

type fakeSession struct {
	store.Session
	hash [32]byte
}

type fakeState struct {
	tokens   []fakeToken
	sessions []fakeSession
	logins   map[[32]byte]fakeLogin
	audit    []core.AuditEntry
	nextID   int64
	touched  int
	purged   int
}

func (s fakeState) clone() fakeState {
	s.tokens = slices.Clone(s.tokens)
	s.sessions = slices.Clone(s.sessions)
	s.logins = maps.Clone(s.logins)
	s.audit = slices.Clone(s.audit)
	return s
}

type fakeRepo struct {
	mu    *sync.Mutex
	state *fakeState
	err   *error
}

func newFakeRepo() *fakeRepo {
	var err error
	return &fakeRepo{mu: &sync.Mutex{}, state: &fakeState{logins: map[[32]byte]fakeLogin{}}, err: &err}
}

func (r *fakeRepo) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	*r.err = err
}

func (r *fakeRepo) snapshot() fakeState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.clone()
}

func (r *fakeRepo) do(fn func(s *fakeState) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if *r.err != nil {
		return *r.err
	}
	return fn(r.state)
}

func (r *fakeRepo) InTx(_ context.Context, fn func(Repository) error) error {
	before := r.snapshot()
	if err := fn(r); err != nil {
		r.mu.Lock()
		*r.state = before
		r.mu.Unlock()
		return err
	}
	return nil
}

func (r *fakeRepo) CreateAPIToken(_ context.Context, n store.NewAPIToken) (store.APIToken, error) {
	var created store.APIToken
	err := r.do(func(s *fakeState) error {
		for _, t := range s.tokens {
			if t.Name == n.Name {
				return fmt.Errorf("create api token %s: %w", n.Name, store.ErrTokenExists)
			}
		}
		s.nextID++
		created = store.APIToken{ID: s.nextID, Name: n.Name, Role: n.Role, CreatedBy: n.CreatedBy, CreatedAt: time.Now()}
		if n.TTL > 0 {
			at := time.Now().Add(n.TTL)
			created.ExpiresAt = &at
		}
		s.tokens = append(s.tokens, fakeToken{APIToken: created, hash: n.Hash})
		return nil
	})
	return created, err
}

func (r *fakeRepo) FindAPITokenByHash(_ context.Context, hash [32]byte) (store.APIToken, error) {
	var found store.APIToken
	err := r.do(func(s *fakeState) error {
		for _, t := range s.tokens {
			if t.hash == hash && t.RevokedAt == nil && (t.ExpiresAt == nil || t.ExpiresAt.After(time.Now())) {
				found = t.APIToken
				return nil
			}
		}
		return core.ErrNotFound
	})
	return found, err
}

func (r *fakeRepo) TouchAPIToken(context.Context, int64) error {
	return r.do(func(s *fakeState) error {
		s.touched++
		return nil
	})
}

func (r *fakeRepo) ListAPITokens(context.Context) ([]store.APIToken, error) {
	var out []store.APIToken
	err := r.do(func(s *fakeState) error {
		for _, t := range s.tokens {
			out = append(out, t.APIToken)
		}
		return nil
	})
	return out, err
}

func (r *fakeRepo) RevokeAPIToken(_ context.Context, name string) (store.APIToken, error) {
	var revoked store.APIToken
	err := r.do(func(s *fakeState) error {
		for i, t := range s.tokens {
			if t.Name == name && t.RevokedAt == nil {
				now := time.Now()
				s.tokens[i].RevokedAt = &now
				revoked = s.tokens[i].APIToken
				return nil
			}
		}
		return core.ErrNotFound
	})
	return revoked, err
}

func (r *fakeRepo) expireToken(name string) {
	_ = r.do(func(s *fakeState) error {
		for i, t := range s.tokens {
			if t.Name == name {
				past := time.Now().Add(-time.Second)
				s.tokens[i].ExpiresAt = &past
			}
		}
		return nil
	})
}

func (r *fakeRepo) CreateSession(_ context.Context, n store.NewSession) (store.Session, error) {
	var created store.Session
	err := r.do(func(s *fakeState) error {
		s.nextID++
		created = store.Session{
			ID: s.nextID, Provider: n.Provider, Subject: n.Subject, Username: n.Username, Name: n.Name,
			Email: n.Email, Role: n.Role, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(n.TTL),
		}
		s.sessions = append(s.sessions, fakeSession{Session: created, hash: n.Hash})
		return nil
	})
	return created, err
}

func (r *fakeRepo) FindSessionByHash(_ context.Context, hash [32]byte) (store.Session, error) {
	var found store.Session
	err := r.do(func(s *fakeState) error {
		for _, session := range s.sessions {
			if session.hash == hash && session.ExpiresAt.After(time.Now()) {
				found = session.Session
				return nil
			}
		}
		return core.ErrNotFound
	})
	return found, err
}

func (r *fakeRepo) DeleteSessionByHash(_ context.Context, hash [32]byte) (store.Session, error) {
	var deleted store.Session
	err := r.do(func(s *fakeState) error {
		for i, session := range s.sessions {
			if session.hash == hash {
				deleted = session.Session
				s.sessions = slices.Delete(s.sessions, i, i+1)
				return nil
			}
		}
		return core.ErrNotFound
	})
	return deleted, err
}

func (r *fakeRepo) ListSessions(context.Context) ([]store.Session, error) {
	var out []store.Session
	err := r.do(func(s *fakeState) error {
		for _, session := range s.sessions {
			if session.ExpiresAt.After(time.Now()) {
				out = append(out, session.Session)
			}
		}
		return nil
	})
	return out, err
}

func (r *fakeRepo) DeleteUserSessions(_ context.Context, user string) (int64, error) {
	return r.deleteSessions(func(s store.Session) bool {
		return s.Subject == user || s.Provider+":"+s.Subject == user || strings.EqualFold(s.Username, user)
	})
}

func (r *fakeRepo) DeleteAllSessions(context.Context) (int64, error) {
	return r.deleteSessions(func(store.Session) bool { return true })
}

func (r *fakeRepo) deleteSessions(match func(store.Session) bool) (int64, error) {
	var n int64
	err := r.do(func(s *fakeState) error {
		before := len(s.sessions)
		s.sessions = slices.DeleteFunc(s.sessions, func(f fakeSession) bool { return match(f.Session) })
		n = int64(before - len(s.sessions))
		return nil
	})
	return n, err
}

func (r *fakeRepo) expireSessions() {
	_ = r.do(func(s *fakeState) error {
		for i := range s.sessions {
			s.sessions[i].ExpiresAt = time.Now().Add(-time.Second)
		}
		return nil
	})
}

func (r *fakeRepo) DeleteExpiredAuthRows(context.Context) (int64, error) {
	var n int64
	err := r.do(func(s *fakeState) error {
		s.purged++
		return nil
	})
	return n, err
}

func (r *fakeRepo) CreateLogin(_ context.Context, n store.NewLogin) error {
	return r.do(func(s *fakeState) error {
		pending := 0
		for _, l := range s.logins {
			if l.expiresAt.After(time.Now()) {
				pending++
			}
		}
		if n.MaxPending > 0 && pending >= n.MaxPending {
			return store.ErrTooManyLogins
		}
		s.logins[n.StateHash] = fakeLogin{
			Login:     store.Login{Nonce: n.Nonce, PKCEVerifier: n.PKCEVerifier, ReturnTo: n.ReturnTo},
			expiresAt: time.Now().Add(n.TTL),
		}
		return nil
	})
}

func (r *fakeRepo) ConsumeLogin(_ context.Context, stateHash [32]byte) (store.Login, error) {
	var found store.Login
	err := r.do(func(s *fakeState) error {
		l, ok := s.logins[stateHash]
		delete(s.logins, stateHash)
		if !ok || !l.expiresAt.After(time.Now()) {
			return core.ErrNotFound
		}
		found = l.Login
		return nil
	})
	return found, err
}

func (r *fakeRepo) expireLogins() {
	_ = r.do(func(s *fakeState) error {
		for k, l := range s.logins {
			l.expiresAt = time.Now().Add(-time.Second)
			s.logins[k] = l
		}
		return nil
	})
}

func (r *fakeRepo) RecordAudit(_ context.Context, e core.AuditEntry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	return r.do(func(s *fakeState) error {
		s.nextID++
		e.ID = s.nextID
		s.audit = append(s.audit, e)
		return nil
	})
}

func (r *fakeRepo) actions() []core.AuditAction {
	var out []core.AuditAction
	for _, e := range r.snapshot().audit {
		out = append(out, e.Action)
	}
	return out
}

type fakeProvider struct {
	name      string
	mu        sync.Mutex
	identity  Identity
	err       error
	authErr   error
	exchanges []exchange
}

type exchange struct {
	code, nonce, verifier, redirectURL string
}

func (p *fakeProvider) Name() string {
	if p.name == "" {
		return ProviderOIDC
	}
	return p.name
}

func (p *fakeProvider) AuthCodeURL(_ context.Context, state, nonce, verifier, redirectURL string) (string, error) {
	if p.authErr != nil {
		return "", p.authErr
	}
	q := url.Values{"state": {state}, "nonce": {nonce}, "verifier": {verifier}, "redirect_uri": {redirectURL}}
	return "https://idp.example.com/authorize?" + q.Encode(), nil
}

func (p *fakeProvider) Exchange(_ context.Context, code, nonce, verifier, redirectURL string) (Identity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exchanges = append(p.exchanges, exchange{code, nonce, verifier, redirectURL})
	return p.identity, p.err
}

func (p *fakeProvider) lastExchange(t *testing.T) exchange {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.exchanges) == 0 {
		t.Fatal("provider was never asked to exchange a code")
	}
	return p.exchanges[len(p.exchanges)-1]
}

func serve(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func cookieFrom(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range (&http.Response{Header: rec.Header()}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}
