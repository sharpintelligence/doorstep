package githubtest

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	DefaultAppID    = "12345"
	DefaultTokenTTL = time.Hour
	maxJWTLifetime  = 10 * time.Minute
)

type installationKey struct{}

type installation struct {
	account   string
	suspended bool
}

type grant struct {
	installationID int64
	expiresAt      time.Time
}

type Server struct {
	URL        string
	AppID      string
	PrivateKey *rsa.PrivateKey

	mux *http.ServeMux

	mu            sync.Mutex
	repos         map[string]int64
	installations map[int64]*installation
	tokens        map[string]grant
	commits       map[string]*commitChecks
	branches      map[string]string
	tokenTTL      time.Duration
	now           func() time.Time

	lookupCalls atomic.Int64
	tokenCalls  atomic.Int64
	checksCalls atomic.Int64
	branchCalls atomic.Int64

	fakeRepos     map[string]*Repo
	fakeRepoRoute sync.Once
}

func NewServer(tb testing.TB) *Server {
	tb.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		tb.Fatalf("generate RSA key: %v", err)
	}
	s := &Server{
		AppID:         DefaultAppID,
		PrivateKey:    key,
		mux:           http.NewServeMux(),
		repos:         make(map[string]int64),
		installations: make(map[int64]*installation),
		tokens:        make(map[string]grant),
		commits:       make(map[string]*commitChecks),
		branches:      make(map[string]string),
		tokenTTL:      DefaultTokenTTL,
		now:           time.Now,
	}
	s.mux.HandleFunc("GET /repos/{owner}/{repo}/installation", s.handleLookup)
	s.mux.HandleFunc("POST /app/installations/{id}/access_tokens", s.handleMint)
	s.registerChecks()
	s.registerBranches()
	srv := httptest.NewServer(s)
	tb.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) PrivateKeyPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(s.PrivateKey)})
}

func (s *Server) Install(owner, repo string, installationID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.installations[installationID]; !ok {
		s.installations[installationID] = &installation{account: owner}
	}
	s.repos[repoKey(owner, repo)] = installationID
}

func (s *Server) Suspend(installationID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inst, ok := s.installations[installationID]; ok {
		inst.suspended = true
	}
	s.revokeLocked(installationID)
}

func (s *Server) Uninstall(installationID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.installations, installationID)
	for key, id := range s.repos {
		if id == installationID {
			delete(s.repos, key)
		}
	}
	s.revokeLocked(installationID)
}

func (s *Server) RevokeTokens(installationID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokeLocked(installationID)
}

func (s *Server) SetTokenTTL(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenTTL = d
}

func (s *Server) SetNow(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

func (s *Server) LookupCalls() int64 {
	return s.lookupCalls.Load()
}

func (s *Server) TokenCalls() int64 {
	return s.tokenCalls.Load()
}

func (s *Server) Handle(pattern string, h http.Handler) {
	s.mux.Handle(pattern, s.requireInstallationToken(h))
}

func InstallationID(r *http.Request) (int64, bool) {
	id, ok := r.Context().Value(installationKey{}).(int64)
	return id, ok
}

func WriteError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", "FAKE:"+strconv.Itoa(status))
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) requireInstallationToken(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			WriteError(w, http.StatusUnauthorized, "Requires authentication")
			return
		}
		s.mu.Lock()
		g, found := s.tokens[token]
		now := s.now()
		s.mu.Unlock()
		if !found || !now.Before(g.expiresAt) {
			WriteError(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), installationKey{}, g.installationID)))
	})
}

func (s *Server) handleLookup(w http.ResponseWriter, r *http.Request) {
	s.lookupCalls.Add(1)
	if err := s.verifyJWT(r); err != nil {
		WriteError(w, http.StatusUnauthorized, err.Error())
		return
	}
	s.mu.Lock()
	id, ok := s.repos[repoKey(r.PathValue("owner"), r.PathValue("repo"))]
	var inst installation
	if ok {
		inst = *s.installations[id]
	}
	now := s.now()
	s.mu.Unlock()
	if !ok {
		WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	var suspendedAt *string
	if inst.suspended {
		ts := now.UTC().Format(time.RFC3339)
		suspendedAt = &ts
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"id":           id,
		"account":      map[string]string{"login": inst.account},
		"suspended_at": suspendedAt,
	})
}

func (s *Server) handleMint(w http.ResponseWriter, r *http.Request) {
	s.tokenCalls.Add(1)
	if err := s.verifyJWT(r); err != nil {
		WriteError(w, http.StatusUnauthorized, err.Error())
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.installations[id]
	switch {
	case !ok:
		WriteError(w, http.StatusNotFound, "Not Found")
		return
	case inst.suspended:
		WriteError(w, http.StatusForbidden, "This installation has been suspended")
		return
	}
	token := "ghs_" + rand.Text()
	expiresAt := s.now().Add(s.tokenTTL).UTC().Truncate(time.Second)
	s.tokens[token] = grant{installationID: id, expiresAt: expiresAt}
	WriteJSON(w, http.StatusCreated, map[string]any{
		"token":      token,
		"expires_at": expiresAt.Format(time.RFC3339),
	})
}

func (s *Server) verifyJWT(r *http.Request) error {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return errors.New("a JSON web token could not be decoded")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("a JSON web token could not be decoded")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := decodeSegment(parts[0], &header); err != nil || header.Alg != "RS256" {
		return errors.New("'alg' must be RS256")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return errors.New("a JSON web token could not be decoded")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&s.PrivateKey.PublicKey, crypto.SHA256, sum[:], signature); err != nil {
		return errors.New("'Signature' could not be verified")
	}
	var claims struct {
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
		Issuer    string `json:"iss"`
	}
	if err := decodeSegment(parts[1], &claims); err != nil {
		return errors.New("a JSON web token could not be decoded")
	}
	s.mu.Lock()
	now := s.now()
	s.mu.Unlock()
	switch {
	case claims.Issuer != s.AppID:
		return errors.New("'iss' claim does not match the app")
	case claims.IssuedAt > now.Unix():
		return errors.New("'iat' claim is in the future")
	case claims.ExpiresAt <= now.Unix():
		return errors.New("'exp' claim is in the past")
	case time.Duration(claims.ExpiresAt-claims.IssuedAt)*time.Second > maxJWTLifetime:
		return errors.New("'exp' claim is too far in the future")
	}
	return nil
}

func (s *Server) revokeLocked(installationID int64) {
	for token, g := range s.tokens {
		if g.installationID == installationID {
			delete(s.tokens, token)
		}
	}
}

func decodeSegment(segment string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func repoKey(owner, repo string) string {
	return strings.ToLower(owner + "/" + repo)
}
