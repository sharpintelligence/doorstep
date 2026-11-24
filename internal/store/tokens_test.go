package store_test

import (
	"crypto/sha256"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func hashOf(s string) [32]byte {
	return sha256.Sum256([]byte(s))
}

func newToken(name string) store.NewAPIToken {
	return store.NewAPIToken{Name: name, Hash: hashOf(name), Role: core.RoleDeployer, CreatedBy: "cli:root"}
}

func mustCreateToken(t *testing.T, st *store.Store, n store.NewAPIToken) store.APIToken {
	t.Helper()
	created, err := st.CreateAPIToken(t.Context(), n)
	if err != nil {
		t.Fatalf("CreateAPIToken(%s): %v", n.Name, err)
	}
	return created
}

func TestCreateAPIToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		token      store.NewAPIToken
		wantExpiry time.Duration
	}{
		{name: "no expiry", token: newToken("ci")},
		{name: "with expiry", token: func() store.NewAPIToken {
			n := newToken("deploy-bot")
			n.TTL = 24 * time.Hour
			n.Role = core.RoleAdmin
			return n
		}(), wantExpiry: 24 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			before := time.Now()
			created := mustCreateToken(t, st, tt.token)
			if created.ID == 0 || created.Name != tt.token.Name || created.Role != tt.token.Role || created.CreatedBy != "cli:root" {
				t.Errorf("created = %+v", created)
			}
			if created.LastUsedAt != nil || created.RevokedAt != nil || created.Expired {
				t.Errorf("new token has usage, revocation or expiry: %+v", created)
			}
			switch {
			case tt.wantExpiry == 0 && created.ExpiresAt != nil:
				t.Errorf("ExpiresAt = %v, want none", created.ExpiresAt)
			case tt.wantExpiry != 0 && created.ExpiresAt == nil:
				t.Error("ExpiresAt = nil")
			case tt.wantExpiry != 0:
				if d := created.ExpiresAt.Sub(before); d < tt.wantExpiry-time.Minute || d > tt.wantExpiry+time.Minute {
					t.Errorf("ExpiresAt is %v from now, want about %v", d, tt.wantExpiry)
				}
			}
			found, err := st.FindAPITokenByHash(t.Context(), tt.token.Hash)
			if err != nil || found.ID != created.ID {
				t.Errorf("FindAPITokenByHash = %+v, %v", found, err)
			}
		})
	}
}

func TestCreateAPITokenRejects(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	mustCreateToken(t, st, newToken("ci"))

	duplicate := newToken("ci")
	duplicate.Hash = hashOf("other")
	if _, err := st.CreateAPIToken(t.Context(), duplicate); !errors.Is(err, store.ErrTokenExists) {
		t.Errorf("duplicate name error = %v, want ErrTokenExists", err)
	}

	sameHash := newToken("other")
	sameHash.Hash = hashOf("ci")
	if _, err := st.CreateAPIToken(t.Context(), sameHash); err == nil || errors.Is(err, store.ErrTokenExists) {
		t.Errorf("duplicate hash error = %v, want a non-name constraint error", err)
	}

	badRole := newToken("bad-role")
	badRole.Role = "root"
	if _, err := st.CreateAPIToken(t.Context(), badRole); !errors.Is(err, core.ErrUnknownRole) {
		t.Errorf("bad role error = %v, want ErrUnknownRole", err)
	}

	negative := newToken("negative")
	negative.TTL = -time.Second
	if _, err := st.CreateAPIToken(t.Context(), negative); err == nil {
		t.Error("negative TTL accepted")
	}

	badName := newToken("Bad Name")
	_, err := st.CreateAPIToken(t.Context(), badName)
	assertConstraint(t, err, "api_tokens_name_check")
}

func TestFindAPITokenByHashSkipsUnusableTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup string
	}{
		{name: "expired", setup: "UPDATE api_tokens SET expires_at = now() - interval '1 second' WHERE name = $1"},
		{name: "revoked", setup: "UPDATE api_tokens SET revoked_at = now() WHERE name = $1"},
		{name: "unknown", setup: "UPDATE api_tokens SET token_hash = sha256('elsewhere') WHERE name = $1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			mustCreateToken(t, st, newToken("ci"))
			mustExec(t, st, tt.setup, "ci")
			if _, err := st.FindAPITokenByHash(t.Context(), hashOf("ci")); !errors.Is(err, core.ErrNotFound) {
				t.Errorf("FindAPITokenByHash error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestTouchAPITokenAtMostOncePerMinute(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	created := mustCreateToken(t, st, newToken("ci"))
	lastUsed := func() *time.Time {
		t.Helper()
		var at *time.Time
		if err := store.QueryRowSQL(t.Context(), st, "SELECT last_used_at FROM api_tokens WHERE id = $1", created.ID).Scan(&at); err != nil {
			t.Fatalf("read last_used_at: %v", err)
		}
		return at
	}

	if err := st.TouchAPIToken(t.Context(), created.ID); err != nil {
		t.Fatalf("TouchAPIToken: %v", err)
	}
	first := lastUsed()
	if first == nil {
		t.Fatal("last_used_at not set")
	}
	if err := st.TouchAPIToken(t.Context(), created.ID); err != nil {
		t.Fatalf("TouchAPIToken: %v", err)
	}
	if again := lastUsed(); !again.Equal(*first) {
		t.Errorf("last_used_at moved from %v to %v within a minute", first, again)
	}

	mustExec(t, st, "UPDATE api_tokens SET last_used_at = now() - interval '2 minutes' WHERE id = $1", created.ID)
	stale := lastUsed()
	if err := st.TouchAPIToken(t.Context(), created.ID); err != nil {
		t.Fatalf("TouchAPIToken: %v", err)
	}
	if fresh := lastUsed(); !fresh.After(*stale) {
		t.Errorf("last_used_at = %v, want after %v", fresh, stale)
	}
}

func TestListAndRevokeAPITokens(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	mustCreateToken(t, st, newToken("zeta"))
	mustCreateToken(t, st, newToken("alpha"))
	mustCreateToken(t, st, newToken("expired"))
	mustExec(t, st, "UPDATE api_tokens SET expires_at = now() - interval '1 second' WHERE name = 'expired'")

	revoked, err := st.RevokeAPIToken(t.Context(), "zeta")
	if err != nil || revoked.Name != "zeta" || revoked.RevokedAt == nil {
		t.Fatalf("RevokeAPIToken = %+v, %v", revoked, err)
	}
	if _, err := st.RevokeAPIToken(t.Context(), "zeta"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("second revoke error = %v, want ErrNotFound", err)
	}
	if _, err := st.RevokeAPIToken(t.Context(), "missing"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("revoke missing error = %v, want ErrNotFound", err)
	}

	tokens, err := st.ListAPITokens(t.Context())
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	var names []string
	for _, tok := range tokens {
		names = append(names, tok.Name)
		switch tok.Name {
		case "zeta":
			if tok.RevokedAt == nil {
				t.Error("zeta not revoked")
			}
		case "expired":
			if !tok.Expired {
				t.Error("expired token not reported expired")
			}
		case "alpha":
			if tok.Expired || tok.RevokedAt != nil {
				t.Errorf("alpha = %+v, want active", tok)
			}
		}
	}
	if want := []string{"alpha", "expired", "zeta"}; !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}

	reuse := newToken("zeta")
	reuse.Hash = hashOf("zeta-2")
	if _, err := st.CreateAPIToken(t.Context(), reuse); !errors.Is(err, store.ErrTokenExists) {
		t.Errorf("reusing a revoked name error = %v, want ErrTokenExists", err)
	}
}
