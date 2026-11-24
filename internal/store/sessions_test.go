package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func newSession(token string) store.NewSession {
	return store.NewSession{
		Hash:     hashOf(token),
		Provider: "oidc",
		Subject:  "sub-1",
		Username: "octocat",
		Name:     "The Octocat",
		Email:    "octocat@example.com",
		Role:     core.RoleViewer,
		TTL:      time.Hour,
	}
}

func TestSessionLifecycle(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	before := time.Now()
	created, err := st.CreateSession(t.Context(), newSession("s1"))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if created.Username != "octocat" || created.Role != core.RoleViewer || created.Provider != "oidc" || created.Email != "octocat@example.com" {
		t.Errorf("created = %+v", created)
	}
	if d := created.ExpiresAt.Sub(before); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("ExpiresAt is %v from now, want about an hour", d)
	}

	found, err := st.FindSessionByHash(t.Context(), hashOf("s1"))
	if err != nil || found != created {
		t.Errorf("FindSessionByHash = %+v, %v, want %+v", found, err, created)
	}
	if _, err := st.FindSessionByHash(t.Context(), hashOf("s2")); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("unknown session error = %v, want ErrNotFound", err)
	}

	deleted, err := st.DeleteSessionByHash(t.Context(), hashOf("s1"))
	if err != nil || deleted.ID != created.ID {
		t.Errorf("DeleteSessionByHash = %+v, %v", deleted, err)
	}
	if _, err := st.FindSessionByHash(t.Context(), hashOf("s1")); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("deleted session error = %v, want ErrNotFound", err)
	}
	if _, err := st.DeleteSessionByHash(t.Context(), hashOf("s1")); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("second delete error = %v, want ErrNotFound", err)
	}
}

func TestCreateSessionRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mutate     func(*store.NewSession)
		constraint string
	}{
		{name: "unknown role", mutate: func(s *store.NewSession) { s.Role = "root" }},
		{name: "zero ttl", mutate: func(s *store.NewSession) { s.TTL = 0 }},
		{name: "unknown provider", mutate: func(s *store.NewSession) { s.Provider = "saml" }, constraint: "auth_sessions_provider_check"},
		{name: "blank subject", mutate: func(s *store.NewSession) { s.Subject = " " }, constraint: "auth_sessions_subject_check"},
		{name: "blank username", mutate: func(s *store.NewSession) { s.Username = "" }, constraint: "auth_sessions_username_check"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			n := newSession("s1")
			tt.mutate(&n)
			_, err := st.CreateSession(t.Context(), n)
			if tt.constraint != "" {
				assertConstraint(t, err, tt.constraint)
				return
			}
			if err == nil {
				t.Error("CreateSession accepted an invalid session")
			}
		})
	}
}

func TestExpiredSessionIsNotFoundAndPurged(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	for _, token := range []string{"live", "expired"} {
		if _, err := st.CreateSession(t.Context(), newSession(token)); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}
	mustExec(t, st, "UPDATE auth_sessions SET expires_at = now() - interval '1 second' WHERE token_hash = sha256($1::bytea)", "expired")
	if _, err := st.FindSessionByHash(t.Context(), hashOf("expired")); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("expired session error = %v, want ErrNotFound", err)
	}
	if err := st.CreateLogin(t.Context(), newLogin("old")); err != nil {
		t.Fatalf("CreateLogin: %v", err)
	}
	if err := st.CreateLogin(t.Context(), newLogin("fresh")); err != nil {
		t.Fatalf("CreateLogin: %v", err)
	}
	mustExec(t, st, "UPDATE auth_logins SET expires_at = now() - interval '1 second' WHERE state_hash = sha256($1::bytea)", "old")

	purged, err := st.DeleteExpiredAuthRows(t.Context())
	if err != nil || purged != 2 {
		t.Errorf("DeleteExpiredAuthRows = %d, %v, want 2", purged, err)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM auth_sessions"); n != 1 {
		t.Errorf("%d sessions left, want 1", n)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM auth_logins"); n != 1 {
		t.Errorf("%d logins left, want 1", n)
	}
	if _, err := st.FindSessionByHash(t.Context(), hashOf("live")); err != nil {
		t.Errorf("live session: %v", err)
	}
}

func newLogin(state string) store.NewLogin {
	return store.NewLogin{StateHash: hashOf(state), Nonce: "nonce-" + state, PKCEVerifier: "verifier-" + state, ReturnTo: "/apps", TTL: 10 * time.Minute}
}

func TestConsumeLoginIsSingleUse(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	if err := st.CreateLogin(t.Context(), newLogin("state")); err != nil {
		t.Fatalf("CreateLogin: %v", err)
	}
	got, err := st.ConsumeLogin(t.Context(), hashOf("state"))
	if err != nil {
		t.Fatalf("ConsumeLogin: %v", err)
	}
	if got != (store.Login{Nonce: "nonce-state", PKCEVerifier: "verifier-state", ReturnTo: "/apps"}) {
		t.Errorf("ConsumeLogin = %+v", got)
	}
	if _, err := st.ConsumeLogin(t.Context(), hashOf("state")); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("replayed login error = %v, want ErrNotFound", err)
	}
}

func TestConsumeExpiredLoginDeletesIt(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	if err := st.CreateLogin(t.Context(), newLogin("state")); err != nil {
		t.Fatalf("CreateLogin: %v", err)
	}
	mustExec(t, st, "UPDATE auth_logins SET expires_at = now() - interval '1 second'")
	if _, err := st.ConsumeLogin(t.Context(), hashOf("state")); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("expired login error = %v, want ErrNotFound", err)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM auth_logins"); n != 0 {
		t.Errorf("%d logins left, want the expired one consumed", n)
	}
}

func TestCreateLoginRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mutate     func(*store.NewLogin)
		constraint string
	}{
		{name: "zero ttl", mutate: func(l *store.NewLogin) { l.TTL = 0 }},
		{name: "absolute return path", mutate: func(l *store.NewLogin) { l.ReturnTo = "https://evil.example" }, constraint: "auth_logins_return_to_check"},
		{name: "empty nonce", mutate: func(l *store.NewLogin) { l.Nonce = "" }, constraint: "auth_logins_nonce_check"},
		{name: "empty verifier", mutate: func(l *store.NewLogin) { l.PKCEVerifier = "" }, constraint: "auth_logins_pkce_verifier_check"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			l := newLogin("state")
			tt.mutate(&l)
			err := st.CreateLogin(t.Context(), l)
			if tt.constraint != "" {
				assertConstraint(t, err, tt.constraint)
				return
			}
			if err == nil {
				t.Error("CreateLogin accepted an invalid login")
			}
		})
	}
}
