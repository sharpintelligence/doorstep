package auth

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

var cliRoot = core.Actor{Kind: core.ActorCLI, ID: "root"}

func TestValidateTokenName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ok   bool
	}{
		{name: "ci", ok: true},
		{name: "deploy-bot.prod_1", ok: true},
		{name: "0", ok: true},
		{name: strings.Repeat("a", 63), ok: true},
		{name: ""},
		{name: strings.Repeat("a", 64)},
		{name: "CI"},
		{name: "-ci"},
		{name: ".ci"},
		{name: "c i"},
		{name: "ci/prod"},
		{name: "ci\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateTokenName(tt.name)
			if tt.ok != (err == nil) {
				t.Errorf("ValidateTokenName(%q) = %v, want ok %v", tt.name, err, tt.ok)
			}
			if err != nil && !errors.Is(err, ErrInvalidTokenName) {
				t.Errorf("error %v does not wrap ErrInvalidTokenName", err)
			}
		})
	}
}

func TestTokensCreateAndAuthenticate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		role      core.Role
		expiresIn time.Duration
	}{
		{name: "viewer", role: core.RoleViewer},
		{name: "deployer", role: core.RoleDeployer, expiresIn: time.Hour},
		{name: "admin", role: core.RoleAdmin},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			tokens := NewTokens(repo, nil)
			raw, created, err := tokens.Create(t.Context(), CreateTokenInput{Name: tt.name, Role: tt.role, ExpiresIn: tt.expiresIn, By: cliRoot})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if !wellFormedToken(raw) || created.Name != tt.name || created.Role != tt.role || created.CreatedBy != "cli:root" {
				t.Errorf("Create = %q, %+v", raw, created)
			}
			if (created.ExpiresAt != nil) != (tt.expiresIn > 0) {
				t.Errorf("ExpiresAt = %v for expiry %v", created.ExpiresAt, tt.expiresIn)
			}
			state := repo.snapshot()
			if len(state.tokens) != 1 || state.tokens[0].hash != hashSecret(raw) {
				t.Error("stored hash does not match the token")
			}

			audit := state.audit
			if len(audit) != 1 || audit[0].Action != core.AuditTokenCreate || audit[0].Actor != cliRoot {
				t.Fatalf("audit = %+v", audit)
			}
			var detail map[string]string
			if err := json.Unmarshal(audit[0].Detail, &detail); err != nil || detail["token"] != tt.name || detail["role"] != string(tt.role) {
				t.Errorf("audit detail = %s", audit[0].Detail)
			}
			if _, hasExpiry := detail["expires_at"]; hasExpiry != (tt.expiresIn > 0) {
				t.Errorf("audit detail expiry = %s", audit[0].Detail)
			}
			if strings.Contains(string(audit[0].Detail), raw) {
				t.Error("audit detail contains the token")
			}

			p, err := tokens.Authenticate(t.Context(), raw)
			want := Principal{Actor: core.Actor{Kind: core.ActorToken, ID: tt.name, Name: tt.name}, Role: tt.role, Method: MethodToken}
			if err != nil || p != want {
				t.Errorf("Authenticate = %+v, %v, want %+v", p, err, want)
			}
			if repo.snapshot().touched != 1 {
				t.Error("token use was not recorded")
			}
		})
	}
}

func TestTokensCreateRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      CreateTokenInput
		wantErr []string
		wantIs  error
	}{
		{name: "bad name", in: CreateTokenInput{Name: "CI", Role: core.RoleViewer, By: cliRoot}, wantIs: ErrInvalidTokenName},
		{name: "bad role", in: CreateTokenInput{Name: "ci", Role: "root", By: cliRoot}, wantIs: core.ErrUnknownRole},
		{name: "negative expiry", in: CreateTokenInput{Name: "ci", Role: core.RoleViewer, ExpiresIn: -time.Second, By: cliRoot}, wantErr: []string{"must not be negative"}},
		{name: "no creator", in: CreateTokenInput{Name: "ci", Role: core.RoleViewer}, wantErr: []string{"creator"}},
		{name: "everything", in: CreateTokenInput{}, wantErr: []string{"invalid token name", "unknown role", "creator"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			_, _, err := NewTokens(repo, nil).Create(t.Context(), tt.in)
			if err == nil {
				t.Fatal("Create succeeded")
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error %v does not wrap %v", err, tt.wantIs)
			}
			for _, s := range tt.wantErr {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not contain %q", err, s)
				}
			}
			if state := repo.snapshot(); len(state.tokens) != 0 || len(state.audit) != 0 {
				t.Errorf("invalid input wrote %+v", state)
			}
		})
	}
}

func TestTokensCreateDuplicateWritesNoAudit(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	tokens := NewTokens(repo, nil)
	if _, _, err := tokens.Create(t.Context(), CreateTokenInput{Name: "ci", Role: core.RoleViewer, By: cliRoot}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := tokens.Create(t.Context(), CreateTokenInput{Name: "ci", Role: core.RoleAdmin, By: cliRoot}); !errors.Is(err, store.ErrTokenExists) {
		t.Fatalf("duplicate Create error = %v, want ErrTokenExists", err)
	}
	if got := repo.actions(); !slices.Equal(got, []core.AuditAction{core.AuditTokenCreate}) {
		t.Errorf("audit actions = %v", got)
	}
}

func TestTokensAuthenticateRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		token func(t *testing.T, repo *fakeRepo, tokens *Tokens) string
	}{
		{name: "malformed", token: func(*testing.T, *fakeRepo, *Tokens) string { return "not-a-token" }},
		{name: "unknown", token: func(*testing.T, *fakeRepo, *Tokens) string {
			raw, _ := newAPIToken()
			return raw
		}},
		{name: "revoked", token: func(t *testing.T, _ *fakeRepo, tokens *Tokens) string {
			t.Helper()
			raw := mustCreateToken(t, tokens, "ci", core.RoleAdmin)
			if err := tokens.Revoke(t.Context(), "ci", cliRoot); err != nil {
				t.Fatalf("Revoke: %v", err)
			}
			return raw
		}},
		{name: "expired", token: func(t *testing.T, repo *fakeRepo, tokens *Tokens) string {
			t.Helper()
			raw := mustCreateToken(t, tokens, "ci", core.RoleAdmin)
			repo.expireToken("ci")
			return raw
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			tokens := NewTokens(repo, nil)
			raw := tt.token(t, repo, tokens)
			if _, err := tokens.Authenticate(t.Context(), raw); !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("Authenticate error = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestTokensAuthenticateStoreFailure(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	tokens := NewTokens(repo, nil)
	raw := mustCreateToken(t, tokens, "ci", core.RoleViewer)
	errDown := errors.New("database down")
	repo.fail(errDown)
	if _, err := tokens.Authenticate(t.Context(), raw); !errors.Is(err, errDown) || errors.Is(err, ErrUnauthenticated) {
		t.Errorf("Authenticate error = %v, want the store error", err)
	}
}

func TestTokensListAndRevoke(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	tokens := NewTokens(repo, nil)
	mustCreateToken(t, tokens, "a", core.RoleViewer)
	mustCreateToken(t, tokens, "b", core.RoleAdmin)

	admin := core.Actor{Kind: core.ActorUser, ID: "alice"}
	if err := tokens.Revoke(t.Context(), "b", admin); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := tokens.Revoke(t.Context(), "b", admin); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("second Revoke error = %v, want ErrNotFound", err)
	}
	if err := tokens.Revoke(t.Context(), "a", core.Actor{}); err == nil {
		t.Error("Revoke without an actor succeeded")
	}
	list, err := tokens.List(t.Context())
	if err != nil || len(list) != 2 || list[1].RevokedAt == nil || list[0].RevokedAt != nil {
		t.Errorf("List = %+v, %v", list, err)
	}
	audit := repo.snapshot().audit
	last := audit[len(audit)-1]
	if len(audit) != 3 || last.Action != core.AuditTokenRevoke || last.Actor != admin || !strings.Contains(string(last.Detail), `"token":"b"`) {
		t.Errorf("audit = %+v", audit)
	}
}

func mustCreateToken(t *testing.T, tokens *Tokens, name string, role core.Role) string {
	t.Helper()
	raw, _, err := tokens.Create(t.Context(), CreateTokenInput{Name: name, Role: role, By: cliRoot})
	if err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
	return raw
}
