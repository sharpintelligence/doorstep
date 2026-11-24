package githubtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type claims struct {
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	Issuer    string `json:"iss"`
}

func signJWT(t *testing.T, key *rsa.PrivateKey, alg string, c claims) string {
	t.Helper()
	enc := base64.RawURLEncoding
	header, err := json.Marshal(map[string]string{"alg": alg, "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	input := enc.EncodeToString(header) + "." + enc.EncodeToString(body)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return input + "." + enc.EncodeToString(sig)
}

func call(t *testing.T, s *Server, method, path, bearer string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, s.URL+path, http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func TestServerVerifiesJWT(t *testing.T) {
	t.Parallel()

	s := NewServer(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s.SetNow(func() time.Time { return now })
	s.Install("acme", "app", 5)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	valid := claims{IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(9 * time.Minute).Unix(), Issuer: s.AppID}

	tests := []struct {
		name       string
		token      string
		wantStatus int
	}{
		{name: "valid", token: signJWT(t, s.PrivateKey, "RS256", valid), wantStatus: http.StatusOK},
		{name: "missing", token: "", wantStatus: http.StatusUnauthorized},
		{name: "not a jwt", token: "ghs_abc", wantStatus: http.StatusUnauthorized},
		{name: "wrong key", token: signJWT(t, other, "RS256", valid), wantStatus: http.StatusUnauthorized},
		{name: "wrong alg", token: signJWT(t, s.PrivateKey, "HS256", valid), wantStatus: http.StatusUnauthorized},
		{
			name:       "wrong issuer",
			token:      signJWT(t, s.PrivateKey, "RS256", claims{IssuedAt: valid.IssuedAt, ExpiresAt: valid.ExpiresAt, Issuer: "999"}),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "issued in the future",
			token:      signJWT(t, s.PrivateKey, "RS256", claims{IssuedAt: now.Add(time.Minute).Unix(), ExpiresAt: valid.ExpiresAt, Issuer: s.AppID}),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "expired",
			token:      signJWT(t, s.PrivateKey, "RS256", claims{IssuedAt: now.Add(-10 * time.Minute).Unix(), ExpiresAt: now.Unix(), Issuer: s.AppID}),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "lifetime over ten minutes",
			token:      signJWT(t, s.PrivateKey, "RS256", claims{IssuedAt: valid.IssuedAt, ExpiresAt: now.Add(10 * time.Minute).Unix(), Issuer: s.AppID}),
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status, body := call(t, s, http.MethodGet, "/repos/acme/app/installation", tt.token)
			if status != tt.wantStatus {
				t.Errorf("status = %d, want %d (%v)", status, tt.wantStatus, body)
			}
		})
	}
}

func TestServerInstallations(t *testing.T) {
	t.Parallel()

	s := NewServer(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s.SetNow(func() time.Time { return now })
	s.SetTokenTTL(30 * time.Minute)
	jwt := signJWT(t, s.PrivateKey, "RS256", claims{IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix(), Issuer: s.AppID})
	s.Install("Acme", "App", 5)
	s.Install("alice", "dotfiles", 6)
	s.Handle("GET /repos/{owner}/{repo}/whoami", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := InstallationID(r)
		WriteJSON(w, http.StatusOK, map[string]any{"installation": id, "ok": ok})
	}))

	status, body := call(t, s, http.MethodGet, "/repos/acme/app/installation", jwt)
	if status != http.StatusOK || body["id"] != float64(5) || body["suspended_at"] != nil {
		t.Errorf("lookup = %d %v", status, body)
	}
	if account, _ := body["account"].(map[string]any); account["login"] != "Acme" {
		t.Errorf("account = %v", body["account"])
	}
	if status, _ := call(t, s, http.MethodGet, "/repos/acme/other/installation", jwt); status != http.StatusNotFound {
		t.Errorf("unknown repository lookup = %d, want 404", status)
	}

	status, body = call(t, s, http.MethodPost, "/app/installations/5/access_tokens", jwt)
	token, _ := body["token"].(string)
	if status != http.StatusCreated || !strings.HasPrefix(token, "ghs_") {
		t.Fatalf("mint = %d %v", status, body)
	}
	if body["expires_at"] != now.Add(30*time.Minute).Format(time.RFC3339) {
		t.Errorf("expires_at = %v", body["expires_at"])
	}
	if status, _ := call(t, s, http.MethodPost, "/app/installations/99/access_tokens", jwt); status != http.StatusNotFound {
		t.Errorf("unknown installation mint = %d, want 404", status)
	}
	if status, _ := call(t, s, http.MethodPost, "/app/installations/abc/access_tokens", jwt); status != http.StatusNotFound {
		t.Errorf("non-numeric installation mint = %d, want 404", status)
	}

	status, body = call(t, s, http.MethodGet, "/repos/acme/app/whoami", token)
	if status != http.StatusOK || body["installation"] != float64(5) || body["ok"] != true {
		t.Errorf("authenticated call = %d %v", status, body)
	}
	if status, _ := call(t, s, http.MethodGet, "/repos/acme/app/whoami", jwt); status != http.StatusUnauthorized {
		t.Errorf("call with a JWT = %d, want 401", status)
	}
	if status, _ := call(t, s, http.MethodGet, "/repos/acme/app/whoami", ""); status != http.StatusUnauthorized {
		t.Errorf("anonymous call = %d, want 401", status)
	}

	s.Suspend(5)
	if status, body := call(t, s, http.MethodGet, "/repos/acme/app/installation", jwt); status != http.StatusOK || body["suspended_at"] == nil {
		t.Errorf("suspended lookup = %d %v", status, body)
	}
	if status, _ := call(t, s, http.MethodPost, "/app/installations/5/access_tokens", jwt); status != http.StatusForbidden {
		t.Errorf("suspended mint = %d, want 403", status)
	}
	if status, _ := call(t, s, http.MethodGet, "/repos/acme/app/whoami", token); status != http.StatusUnauthorized {
		t.Errorf("token of suspended installation = %d, want 401", status)
	}

	s.Uninstall(6)
	if status, _ := call(t, s, http.MethodGet, "/repos/alice/dotfiles/installation", jwt); status != http.StatusNotFound {
		t.Errorf("uninstalled lookup = %d, want 404", status)
	}

	if got, want := s.LookupCalls(), int64(4); got != want {
		t.Errorf("lookup calls = %d, want %d", got, want)
	}
	if got, want := s.TokenCalls(), int64(4); got != want {
		t.Errorf("token calls = %d, want %d", got, want)
	}
}

func TestServerTokenExpiry(t *testing.T) {
	t.Parallel()

	s := NewServer(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	s.SetNow(clock)
	s.Install("acme", "app", 5)
	s.Handle("GET /ping", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	jwt := signJWT(t, s.PrivateKey, "RS256", claims{IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix(), Issuer: s.AppID})
	_, body := call(t, s, http.MethodPost, "/app/installations/5/access_tokens", jwt)
	token, _ := body["token"].(string)

	tests := []struct {
		advance time.Duration
		want    int
	}{
		{advance: DefaultTokenTTL - time.Second, want: http.StatusNoContent},
		{advance: DefaultTokenTTL, want: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		later := now.Add(tt.advance)
		s.SetNow(func() time.Time { return later })
		if status, _ := call(t, s, http.MethodGet, "/ping", token); status != tt.want {
			t.Errorf("after %s: status = %d, want %d", tt.advance, status, tt.want)
		}
	}
}

func TestPrivateKeyPEM(t *testing.T) {
	t.Parallel()

	s := NewServer(t)
	block, _ := pem.Decode(s.PrivateKeyPEM())
	if block == nil || block.Type != "RSA PRIVATE KEY" {
		t.Fatalf("PrivateKeyPEM block = %v", block)
	}
	if s.AppID != DefaultAppID {
		t.Errorf("AppID = %q", s.AppID)
	}
	if _, err := strconv.Atoi(s.AppID); err != nil {
		t.Errorf("AppID %q is not numeric", s.AppID)
	}
}
