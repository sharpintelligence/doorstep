package registrytest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const manifestMediaType = "application/vnd.oci.image.manifest.v1+json"

type manifest struct {
	body   []byte
	digest string
}

type Server struct {
	URL string

	mu        sync.Mutex
	manifests map[string]manifest
	requests  atomic.Int64
}

func NewServer(tb testing.TB) *Server {
	tb.Helper()
	s := &Server{manifests: map[string]manifest{}}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(s.serveHTTP))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Start()
	tb.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

func (s *Server) Host() string {
	return strings.TrimPrefix(s.URL, "http://")
}

func (s *Server) Repo(name string) string {
	return s.Host() + "/" + name
}

func (s *Server) Push(name, tag string) string {
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     manifestMediaType,
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    "sha256:" + hexSHA256([]byte(name+":"+tag)),
			"size":      2,
		},
		"layers": []any{},
	})
	if err != nil {
		panic(err)
	}
	m := manifest{body: body, digest: "sha256:" + hexSHA256(body)}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.manifests[name+":"+tag] = m
	return m.digest
}

func (s *Server) Requests() int64 {
	return s.requests.Load()
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	switch {
	case r.URL.Path == "/v2/" || r.URL.Path == "/v2":
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(r.URL.Path, "/v2/") && strings.Contains(r.URL.Path, "/manifests/"):
		s.serveManifest(w, r)
	default:
		writeError(w, http.StatusNotFound, "NOT_FOUND")
	}
}

func (s *Server) serveManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodHead && r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v2/")
	idx := strings.LastIndex(rest, "/manifests/")
	name, ref := rest[:idx], rest[idx+len("/manifests/"):]

	s.mu.Lock()
	m, ok := s.manifests[name+":"+ref]
	if !ok {
		for key, candidate := range s.manifests {
			if strings.HasPrefix(key, name+":") && candidate.digest == ref {
				m, ok = candidate, true
				break
			}
		}
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN")
		return
	}
	w.Header().Set("Content-Type", manifestMediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(m.body)))
	w.Header().Set("Docker-Content-Digest", m.digest)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(m.body)
	}
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": strings.ToLower(code)}}})
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
