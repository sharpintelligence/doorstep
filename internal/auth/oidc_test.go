package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	testClientID     = "doorstep"
	testClientSecret = "oidc-client-secret"
)

type issuance struct {
	nonce     string
	challenge string
	redirect  string
}

type fakeIssuer struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu             sync.Mutex
	codes          map[string]issuance
	idClaims       map[string]any
	userInfo       map[string]any
	noUserInfo     bool
	audience       string
	expiresIn      time.Duration
	nonceOverride  string
	discoveryDown  bool
	discoveryCalls atomic.Int64
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	f := &fakeIssuer{
		t:         t,
		key:       key,
		codes:     map[string]issuance{},
		idClaims:  map[string]any{"sub": "sub-1", "preferred_username": "alice", "name": "Alice", "email": "alice@example.com", "email_verified": true, "groups": []string{"ops"}},
		userInfo:  map[string]any{},
		audience:  testClientID,
		expiresIn: 5 * time.Minute,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /jwks", f.jwks)
	mux.HandleFunc("POST /token", f.token)
	mux.HandleFunc("GET /userinfo", f.userinfo)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssuer) set(fn func(f *fakeIssuer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeIssuer) discovery(w http.ResponseWriter, _ *http.Request) {
	f.discoveryCalls.Add(1)
	f.mu.Lock()
	down, noUserInfo := f.discoveryDown, f.noUserInfo
	f.mu.Unlock()
	if down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	doc := map[string]any{
		"issuer":                                f.srv.URL,
		"authorization_endpoint":                f.srv.URL + "/authorize",
		"token_endpoint":                        f.srv.URL + "/token",
		"jwks_uri":                              f.srv.URL + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	if !noUserInfo {
		doc["userinfo_endpoint"] = f.srv.URL + "/userinfo"
	}
	writeTestJSON(w, http.StatusOK, doc)
}

func (f *fakeIssuer) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := f.key.PublicKey
	writeTestJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

func (f *fakeIssuer) authorize(t *testing.T, authURL string) (code string, state string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	q := u.Query()
	if u.Path != "/authorize" || q.Get("client_id") != testClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("unexpected authorization request %s", authURL)
	}
	if !strings.Contains(" "+q.Get("scope")+" ", " openid ") {
		t.Fatalf("scope %q lacks openid", q.Get("scope"))
	}
	code, _ = newSecret()
	f.set(func(f *fakeIssuer) {
		f.codes[code] = issuance{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
	})
	return code, q.Get("state")
}

func (f *fakeIssuer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != testClientID || secret != testClientSecret {
		writeTestJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	code := r.PostForm.Get("code")
	iss, found := f.codes[code]
	delete(f.codes, code)
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case !found:
		writeTestJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	case base64.RawURLEncoding.EncodeToString(sum[:]) != iss.challenge:
		writeTestJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "pkce"})
		return
	case r.PostForm.Get("redirect_uri") != iss.redirect:
		writeTestJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "redirect"})
		return
	}
	claims := map[string]any{}
	for k, v := range f.idClaims {
		claims[k] = v
	}
	nonce := iss.nonce
	if f.nonceOverride != "" {
		nonce = f.nonceOverride
	}
	now := time.Now()
	claims["iss"] = f.srv.URL
	claims["aud"] = f.audience
	claims["iat"] = now.Unix()
	claims["exp"] = now.Add(f.expiresIn).Unix()
	claims["nonce"] = nonce
	writeTestJSON(w, http.StatusOK, map[string]any{
		"access_token": "access-" + code,
		"token_type":   "Bearer",
		"expires_in":   300,
		"id_token":     f.sign(claims),
	})
}

func (f *fakeIssuer) userinfo(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer access-") {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	info := map[string]any{"sub": f.idClaims["sub"]}
	for k, v := range f.userInfo {
		info[k] = v
	}
	writeTestJSON(w, http.StatusOK, info)
}

func (f *fakeIssuer) sign(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"k1","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		f.t.Fatalf("marshal claims: %v", err)
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		f.t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func writeTestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeIssuer) provider(t *testing.T) *OIDCProvider {
	t.Helper()
	p, err := NewOIDCProvider(OIDCOptions{
		IssuerURL:     f.srv.URL,
		ClientID:      testClientID,
		ClientSecret:  func() (string, error) { return testClientSecret, nil },
		Scopes:        []string{"openid", "profile", "email", "groups"},
		GroupsClaim:   "groups",
		UsernameClaim: "preferred_username",
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	return p
}

const testRedirect = "https://doorstep.example.com/auth/callback"

func (f *fakeIssuer) login(t *testing.T, p *OIDCProvider) (Identity, error) {
	t.Helper()
	nonce, _ := newSecret()
	verifier := oauth2.GenerateVerifier()
	authURL, err := p.AuthCodeURL(t.Context(), "state-1", nonce, verifier, testRedirect)
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	code, state := f.authorize(t, authURL)
	if state != "state-1" {
		t.Fatalf("state = %q", state)
	}
	return p.Exchange(t.Context(), code, nonce, verifier, testRedirect)
}

func TestOIDCProviderExchange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(f *fakeIssuer)
		want    Identity
		wantErr string
	}{
		{
			name: "id token claims",
			want: Identity{Subject: "sub-1", Username: "alice", Name: "Alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"ops"}},
		},
		{
			name: "groups only in userinfo",
			setup: func(f *fakeIssuer) {
				delete(f.idClaims, "groups")
				f.userInfo["groups"] = []string{"admins", "ops"}
			},
			want: Identity{Subject: "sub-1", Username: "alice", Name: "Alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"admins", "ops"}},
		},
		{
			name: "id token claims win over userinfo",
			setup: func(f *fakeIssuer) {
				f.userInfo["groups"] = []string{"admins"}
				f.userInfo["email"] = "mallory@example.com"
			},
			want: Identity{Subject: "sub-1", Username: "alice", Name: "Alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"ops"}},
		},
		{
			name: "single group string and string email_verified",
			setup: func(f *fakeIssuer) {
				f.idClaims["groups"] = "ops"
				f.idClaims["email_verified"] = "true"
			},
			want: Identity{Subject: "sub-1", Username: "alice", Name: "Alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"ops"}},
		},
		{
			name: "no userinfo endpoint",
			setup: func(f *fakeIssuer) {
				f.noUserInfo = true
				delete(f.idClaims, "preferred_username")
				delete(f.idClaims, "name")
				f.idClaims["email_verified"] = false
				f.idClaims["groups"] = []any{"ops", 7}
			},
			want: Identity{Subject: "sub-1", Username: "sub-1", Name: "sub-1", Email: "alice@example.com"},
		},
		{
			name:    "wrong nonce",
			setup:   func(f *fakeIssuer) { f.nonceOverride = "attacker-nonce" },
			wantErr: "nonce does not match",
		},
		{
			name:    "wrong audience",
			setup:   func(f *fakeIssuer) { f.audience = "another-client" },
			wantErr: "expected audience",
		},
		{
			name:    "expired id token",
			setup:   func(f *fakeIssuer) { f.expiresIn = -time.Minute },
			wantErr: "token is expired",
		},
		{
			name:    "userinfo subject mismatch",
			setup:   func(f *fakeIssuer) { f.userInfo["sub"] = "someone-else" },
			wantErr: "userinfo subject does not match",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeIssuer(t)
			if tt.setup != nil {
				f.set(tt.setup)
			}
			got, err := f.login(t, f.provider(t))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Exchange error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Exchange: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Exchange = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestOIDCProviderRejectsWrongVerifierAndReplay(t *testing.T) {
	t.Parallel()

	f := newFakeIssuer(t)
	p := f.provider(t)
	nonce, _ := newSecret()
	verifier := oauth2.GenerateVerifier()
	authURL, err := p.AuthCodeURL(t.Context(), "s", nonce, verifier, testRedirect)
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	code, _ := f.authorize(t, authURL)
	if _, err := p.Exchange(t.Context(), code, nonce, oauth2.GenerateVerifier(), testRedirect); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("Exchange with another verifier = %v, want invalid_grant", err)
	}
	if _, err := p.Exchange(t.Context(), code, nonce, verifier, testRedirect); err == nil {
		t.Error("code was accepted twice")
	}
}

func TestOIDCProviderReadsSecretPerExchange(t *testing.T) {
	t.Parallel()

	f := newFakeIssuer(t)
	var reads atomic.Int64
	secret := testClientSecret
	p, err := NewOIDCProvider(OIDCOptions{
		IssuerURL: f.srv.URL, ClientID: testClientID, Scopes: []string{"openid"}, GroupsClaim: "groups", UsernameClaim: "preferred_username",
		ClientSecret: func() (string, error) {
			reads.Add(1)
			return secret, nil
		},
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	for range 2 {
		if _, err := f.login(t, p); err != nil {
			t.Fatalf("login: %v", err)
		}
	}
	if reads.Load() != 2 {
		t.Errorf("secret read %d times, want once per exchange", reads.Load())
	}

	errUnreadable := errors.New("secret file missing")
	p.opts.ClientSecret = func() (string, error) { return "", errUnreadable }
	if _, err := f.login(t, p); !errors.Is(err, errUnreadable) {
		t.Errorf("Exchange error = %v, want the secret error", err)
	}
}

func TestOIDCDiscoveryIsLazyCachedAndRetried(t *testing.T) {
	t.Parallel()

	f := newFakeIssuer(t)
	f.set(func(f *fakeIssuer) { f.discoveryDown = true })
	p := f.provider(t)
	if f.discoveryCalls.Load() != 0 {
		t.Fatal("NewOIDCProvider contacted the issuer")
	}
	if err := p.Probe(t.Context()); err == nil {
		t.Fatal("Probe succeeded while discovery is down")
	}
	if _, err := p.AuthCodeURL(t.Context(), "s", "n", oauth2.GenerateVerifier(), testRedirect); err == nil {
		t.Fatal("AuthCodeURL succeeded while discovery is down")
	}
	f.set(func(f *fakeIssuer) { f.discoveryDown = false })
	if err := p.Probe(t.Context()); err != nil {
		t.Fatalf("Probe after recovery: %v", err)
	}
	calls := f.discoveryCalls.Load()
	if _, err := f.login(t, p); err != nil {
		t.Fatalf("login: %v", err)
	}
	if f.discoveryCalls.Load() != calls {
		t.Errorf("discovery ran again after success: %d calls, want %d", f.discoveryCalls.Load(), calls)
	}
}

func TestOIDCIssuerMismatch(t *testing.T) {
	t.Parallel()

	f := newFakeIssuer(t)
	p, err := NewOIDCProvider(OIDCOptions{
		IssuerURL: f.srv.URL + "/", ClientID: testClientID, ClientSecret: func() (string, error) { return "", nil },
		GroupsClaim: "groups", UsernameClaim: "preferred_username",
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	if err := p.Probe(t.Context()); err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Errorf("Probe error = %v, want an issuer mismatch", err)
	}
}

func TestNewOIDCProviderValidates(t *testing.T) {
	t.Parallel()

	_, err := NewOIDCProvider(OIDCOptions{})
	for _, want := range []string{"issuer url", "client id", "client secret", "claims"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not mention %q", err, want)
		}
	}
}

func TestOIDCSignInEndToEnd(t *testing.T) {
	t.Parallel()

	f := newFakeIssuer(t)
	f.set(func(f *fakeIssuer) {
		delete(f.idClaims, "groups")
		f.userInfo["groups"] = []string{"platform"}
	})
	mapper := RoleMapper{Provider: ProviderOIDC, Bindings: map[core.Role]Binding{core.RoleAdmin: {Groups: []string{"platform"}}}}
	e := newEnv(t, testPublicURL, f.provider(t), mapper)
	fl := &flow{t: t, e: e, names: newCookies(true)}

	rec := fl.do(httptest.NewRequestWithContext(t.Context(), http.MethodGet, LoginPath+"?return_to=%2Fapps", http.NoBody))
	if rec.Code != http.StatusFound {
		t.Fatalf("login = %d %q", rec.Code, rec.Body.String())
	}
	code, state := f.authorize(t, rec.Header().Get("Location"))
	login := cookieFrom(t, rec, fl.names.login)
	rec = fl.callback(url.Values{"state": {state}, "code": {code}}, login)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/apps" {
		t.Fatalf("callback = %d %q", rec.Code, rec.Body.String())
	}
	session := cookieFrom(t, rec, fl.names.session)
	got := decodeSeen(t, fl.do(request{session: session.Value, path: "/admin"}.build(t)))
	if !got.Authenticated || got.Actor != "user:alice" || got.Role != "admin" {
		t.Errorf("signed in as %+v", got)
	}
}
