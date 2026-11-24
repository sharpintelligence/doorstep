package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
)

func TestNewSecrets(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for range 100 {
		raw, hash := newSecret()
		if !wellFormedSecret(raw) || hash != hashSecret(raw) || seen[raw] {
			t.Fatalf("newSecret() = %q, well formed %v, unique %v", raw, wellFormedSecret(raw), !seen[raw])
		}
		seen[raw] = true

		token, tokenHash := newAPIToken()
		if !strings.HasPrefix(token, TokenPrefix) || !wellFormedToken(token) || tokenHash != hashSecret(token) || seen[token] {
			t.Fatalf("newAPIToken() = %q", token)
		}
		seen[token] = true
	}
}

func TestWellFormedToken(t *testing.T) {
	t.Parallel()

	valid, _ := newAPIToken()
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "valid", raw: valid, want: true},
		{name: "empty"},
		{name: "prefix only", raw: TokenPrefix},
		{name: "missing prefix", raw: strings.TrimPrefix(valid, TokenPrefix)},
		{name: "wrong prefix", raw: "ghp_" + strings.TrimPrefix(valid, TokenPrefix)},
		{name: "short", raw: valid[:len(valid)-1]},
		{name: "long", raw: valid + "A"},
		{name: "padding", raw: valid[:len(valid)-1] + "="},
		{name: "standard base64", raw: valid[:len(valid)-1] + "+"},
		{name: "whitespace", raw: valid[:len(valid)-1] + " "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := wellFormedToken(tt.raw); got != tt.want {
				t.Errorf("wellFormedToken(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestCSRFToken(t *testing.T) {
	t.Parallel()

	a, hashA := newSecret()
	b, _ := newSecret()
	if first, second := csrfTokenFor(a), csrfTokenFor(a); first != second {
		t.Error("csrf token is not deterministic")
	}
	if csrfTokenFor(a) == csrfTokenFor(b) {
		t.Error("two sessions share a csrf token")
	}
	if csrfTokenFor(a) == string(hashA[:]) || strings.Contains(csrfTokenFor(a), a) {
		t.Error("csrf token reveals the session token or its stored hash")
	}
	if !wellFormedSecret(csrfTokenFor(a)) {
		t.Errorf("csrf token %q is not 32 base64url bytes", csrfTokenFor(a))
	}
}

func TestPrincipalContext(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	if _, ok := PrincipalFrom(ctx); ok {
		t.Error("empty context has a principal")
	}
	if CSRFToken(ctx) != "" {
		t.Error("empty context has a csrf token")
	}
	p := Principal{Actor: core.Actor{Kind: core.ActorUser, ID: "alice"}, Role: core.RoleDeployer, Method: MethodSession}
	got, ok := PrincipalFrom(withCSRFToken(WithPrincipal(ctx, p), "csrf"))
	if !ok || got != p {
		t.Errorf("PrincipalFrom = %+v, %v", got, ok)
	}
	if CSRFToken(withCSRFToken(ctx, "csrf")) != "csrf" {
		t.Error("csrf token not stored")
	}
	if !p.Allows(core.RoleViewer) || !p.Allows(core.RoleDeployer) || p.Allows(core.RoleAdmin) {
		t.Error("Principal.Allows does not follow the role order")
	}
}
