package auth

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

func seedSessions(t *testing.T, repo *fakeRepo) {
	t.Helper()
	for i, s := range []store.NewSession{
		{Provider: ProviderOIDC, Subject: "sub-a", Username: "alice"},
		{Provider: ProviderOIDC, Subject: "sub-a", Username: "alice"},
		{Provider: ProviderOIDC, Subject: "sub-b", Username: "Bob"},
		{Provider: ProviderGitHub, Subject: "583231", Username: "octocat"},
	} {
		s.Hash = hashSecret(string(rune('a' + i)))
		s.Role = core.RoleViewer
		s.TTL = time.Hour
		if _, err := repo.CreateSession(t.Context(), s); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}
}

func TestSessionsRevoke(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		in          RevokeSessionsInput
		wantRevoked int64
		wantDetail  map[string]any
		wantLeft    []string
	}{
		{
			name:        "by username",
			in:          RevokeSessionsInput{User: "alice", By: cliRoot},
			wantRevoked: 2,
			wantDetail:  map[string]any{"user": "alice", "sessions": float64(2), "origin": map[string]any{"os_user": "root"}},
			wantLeft:    []string{"sub-b", "583231"},
		},
		{
			name:        "username case-insensitively",
			in:          RevokeSessionsInput{User: "bob", By: cliRoot},
			wantRevoked: 1,
			wantDetail:  map[string]any{"user": "bob", "sessions": float64(1), "origin": map[string]any{"os_user": "root"}},
			wantLeft:    []string{"sub-a", "sub-a", "583231"},
		},
		{
			name:        "by actor id",
			in:          RevokeSessionsInput{User: "github:583231", By: Requester{Actor: cliRoot.Actor}},
			wantRevoked: 1,
			wantDetail:  map[string]any{"user": "github:583231", "sessions": float64(1)},
			wantLeft:    []string{"sub-a", "sub-a", "sub-b"},
		},
		{
			name:        "by subject",
			in:          RevokeSessionsInput{User: " sub-b ", By: Requester{Actor: cliRoot.Actor}},
			wantRevoked: 1,
			wantDetail:  map[string]any{"user": "sub-b", "sessions": float64(1)},
			wantLeft:    []string{"sub-a", "sub-a", "583231"},
		},
		{
			name:       "nobody matches",
			in:         RevokeSessionsInput{User: "mallory", By: Requester{Actor: cliRoot.Actor}},
			wantDetail: map[string]any{"user": "mallory", "sessions": float64(0)},
			wantLeft:   []string{"sub-a", "sub-a", "sub-b", "583231"},
		},
		{
			name:        "all",
			in:          RevokeSessionsInput{All: true, By: Requester{Actor: cliRoot.Actor}},
			wantRevoked: 4,
			wantDetail:  map[string]any{"all": true, "sessions": float64(4)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			seedSessions(t, repo)
			n, err := NewSessions(repo).Revoke(t.Context(), tt.in)
			if err != nil || n != tt.wantRevoked {
				t.Fatalf("Revoke = %d, %v, want %d", n, err, tt.wantRevoked)
			}
			st := repo.snapshot()
			var left []string
			for _, s := range st.sessions {
				left = append(left, s.Subject)
			}
			if !slices.Equal(left, tt.wantLeft) {
				t.Errorf("sessions left = %v, want %v", left, tt.wantLeft)
			}
			if len(st.audit) != 1 || st.audit[0].Action != core.AuditSessionRevoke || st.audit[0].Actor != cliRoot.Actor {
				t.Fatalf("audit = %+v", st.audit)
			}
			var detail map[string]any
			if err := json.Unmarshal(st.audit[0].Detail, &detail); err != nil || !reflect.DeepEqual(detail, tt.wantDetail) {
				t.Errorf("audit detail = %s, want %v", st.audit[0].Detail, tt.wantDetail)
			}
		})
	}
}

func TestSessionsRevokeRejects(t *testing.T) {
	t.Parallel()

	errDown := errors.New("database down")
	tests := []struct {
		name    string
		in      RevokeSessionsInput
		fail    error
		wantErr string
	}{
		{name: "neither user nor all", in: RevokeSessionsInput{By: cliRoot}, wantErr: "exactly one"},
		{name: "blank user", in: RevokeSessionsInput{User: "  ", By: cliRoot}, wantErr: "exactly one"},
		{name: "both", in: RevokeSessionsInput{User: "alice", All: true, By: cliRoot}, wantErr: "exactly one"},
		{name: "no actor", in: RevokeSessionsInput{All: true}, wantErr: "revoker"},
		{name: "store failure", in: RevokeSessionsInput{All: true, By: cliRoot}, fail: errDown, wantErr: errDown.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			seedSessions(t, repo)
			repo.fail(tt.fail)
			if _, err := NewSessions(repo).Revoke(t.Context(), tt.in); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Revoke error = %v, want %q", err, tt.wantErr)
			}
			repo.fail(nil)
			if st := repo.snapshot(); len(st.sessions) != 4 || len(st.audit) != 0 {
				t.Errorf("rejected revoke changed state: %d sessions, audit %v", len(st.sessions), st.audit)
			}
		})
	}
}

func TestSessionsList(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	seedSessions(t, repo)
	list, err := NewSessions(repo).List(t.Context())
	if err != nil || len(list) != 4 {
		t.Fatalf("List = %v, %v", list, err)
	}
	repo.expireSessions()
	if list, err := NewSessions(repo).List(t.Context()); err != nil || len(list) != 0 {
		t.Errorf("List after expiry = %v, %v", list, err)
	}
}

func TestUserActor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider, subject, username string
		want                        core.Actor
	}{
		{provider: "oidc", subject: "sub-1", username: "alice", want: core.Actor{Kind: core.ActorUser, ID: "oidc:sub-1", Name: "alice"}},
		{provider: "github", subject: "583231", username: "OctoCat", want: core.Actor{Kind: core.ActorUser, ID: "github:583231", Name: "OctoCat"}},
		{provider: "oidc", subject: "sub-1", want: core.Actor{Kind: core.ActorUser, ID: "oidc:sub-1", Name: "sub-1"}},
		{provider: "oidc", subject: "sub-1", username: " padded", want: core.Actor{Kind: core.ActorUser, ID: "oidc:sub-1", Name: "sub-1"}},
		{provider: "oidc", subject: "sub-1", username: "bad\nname", want: core.Actor{Kind: core.ActorUser, ID: "oidc:sub-1", Name: "sub-1"}},
	}
	for _, tt := range tests {
		if got := UserActor(tt.provider, tt.subject, tt.username); got != tt.want {
			t.Errorf("UserActor(%q, %q, %q) = %+v, want %+v", tt.provider, tt.subject, tt.username, got, tt.want)
		}
	}
}
