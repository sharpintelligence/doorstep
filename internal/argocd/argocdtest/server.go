package argocdtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type RequestKind string

const (
	Get   RequestKind = "get"
	Watch RequestKind = "watch"

	eventBuffer = 64
	awaitLimit  = 5 * time.Second
)

type failure struct {
	code  int
	times int
}

type event struct {
	Type   string `json:"type"`
	Object any    `json:"object"`
}

type watcher struct {
	namespace string
	name      string
	events    chan event
	done      chan struct{}
	once      sync.Once
}

func (w *watcher) stop() {
	w.once.Do(func() { close(w.done) })
}

type Server struct {
	URL string

	mu       sync.Mutex
	apps     map[string]Application
	versions map[string]int
	version  int
	watchers map[*watcher]struct{}
	failures map[RequestKind]*failure
	counts   map[RequestKind]int
	queries  []url.Values
	token    string
}

func NewServer(tb testing.TB) *Server {
	tb.Helper()
	s := &Server{
		apps:     make(map[string]Application),
		versions: make(map[string]int),
		version:  100,
		watchers: make(map[*watcher]struct{}),
		failures: make(map[RequestKind]*failure),
		counts:   make(map[RequestKind]int),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /apis/argoproj.io/v1alpha1/namespaces/{namespace}/applications/{name}", s.handleGet)
	mux.HandleFunc("GET /apis/argoproj.io/v1alpha1/namespaces/{namespace}/applications", s.handleWatch)
	srv := httptest.NewServer(mux)
	tb.Cleanup(func() {
		s.CloseWatches()
		srv.Close()
	})
	s.URL = srv.URL
	return s
}

func (s *Server) RequireToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
}

func (s *Server) Kubeconfig(tb testing.TB) string {
	tb.Helper()
	s.mu.Lock()
	token := s.token
	s.mu.Unlock()
	user := "  user: {}\n"
	if token != "" {
		user = "  user:\n    token: " + token + "\n"
	}
	content := "apiVersion: v1\nkind: Config\ncurrent-context: test\n" +
		"clusters:\n- name: test\n  cluster:\n    server: " + s.URL + "\n" +
		"contexts:\n- name: test\n  context:\n    cluster: test\n    user: test\n" +
		"users:\n- name: test\n" + user
	path := filepath.Join(tb.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		tb.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func (s *Server) Set(app Application) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := appKey(app.Namespace, app.Name)
	_, exists := s.apps[key]
	s.storeLocked(app.Clone())
	kind := "MODIFIED"
	if !exists {
		kind = "ADDED"
	}
	s.broadcastLocked(app.Namespace, app.Name, kind)
}

func (s *Server) Update(namespace, name string, fn func(*Application)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	app, ok := s.apps[appKey(namespace, name)]
	if !ok {
		return false
	}
	app = app.Clone()
	fn(&app)
	s.storeLocked(app)
	s.broadcastLocked(namespace, name, "MODIFIED")
	return true
}

func (s *Server) Delete(namespace, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := appKey(namespace, name)
	if _, ok := s.apps[key]; !ok {
		return
	}
	s.version++
	s.versions[key] = s.version
	s.broadcastLocked(namespace, name, "DELETED")
	delete(s.apps, key)
	delete(s.versions, key)
}

func (s *Server) FailNext(kind RequestKind, code, times int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if code == 0 || times == 0 {
		delete(s.failures, kind)
		return
	}
	s.failures[kind] = &failure{code: code, times: times}
}

func (s *Server) ExpireWatches() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for w := range s.watchers {
		select {
		case w.events <- event{Type: "ERROR", Object: status(http.StatusGone, "Expired", "too old resource version")}:
		default:
		}
		w.stop()
		delete(s.watchers, w)
	}
}

func (s *Server) CloseWatches() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for w := range s.watchers {
		w.stop()
		delete(s.watchers, w)
	}
}

func (s *Server) Requests(kind RequestKind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[kind]
}

func (s *Server) OpenWatches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.watchers)
}

func (s *Server) WatchQueries() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]url.Values, len(s.queries))
	for i, q := range s.queries {
		out[i] = cloneValues(q)
	}
	return out
}

func (s *Server) AwaitWatch(tb testing.TB, total int) {
	tb.Helper()
	deadline := time.Now().Add(awaitLimit)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		ready := s.counts[Watch] >= total && len(s.watchers) > 0
		s.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	tb.Fatalf("argocdtest: no open watch after %d watch requests within %s", total, awaitLimit)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	code, ok := s.admitLocked(Get, r)
	if !ok {
		s.mu.Unlock()
		writeStatus(w, code)
		return
	}
	key := appKey(r.PathValue("namespace"), r.PathValue("name"))
	app, exists := s.apps[key]
	version := s.versions[key]
	s.mu.Unlock()
	if !exists {
		writeJSON(w, http.StatusNotFound, status(http.StatusNotFound, "NotFound",
			fmt.Sprintf("applications.argoproj.io %q not found", r.PathValue("name"))))
		return
	}
	writeJSON(w, http.StatusOK, app.Object(version))
}

func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if v := query.Get("watch"); v != "1" && v != "true" {
		writeJSON(w, http.StatusMethodNotAllowed, status(http.StatusMethodNotAllowed, "MethodNotAllowed", "argocdtest serves only watches on the collection"))
		return
	}
	name, ok := strings.CutPrefix(query.Get("fieldSelector"), "metadata.name=")
	if !ok || name == "" || strings.Contains(name, ",") {
		writeJSON(w, http.StatusBadRequest, status(http.StatusBadRequest, "BadRequest", "argocdtest needs fieldSelector=metadata.name=<name>"))
		return
	}
	namespace := r.PathValue("namespace")
	key := appKey(namespace, name)

	s.mu.Lock()
	code, admitted := s.admitLocked(Watch, r)
	if !admitted {
		s.mu.Unlock()
		writeStatus(w, code)
		return
	}
	s.queries = append(s.queries, cloneValues(query))
	wt := &watcher{namespace: namespace, name: name, events: make(chan event, eventBuffer), done: make(chan struct{})}
	if app, exists := s.apps[key]; exists {
		if from, err := strconv.Atoi(query.Get("resourceVersion")); err == nil && from < s.versions[key] {
			wt.events <- event{Type: "MODIFIED", Object: app.Object(s.versions[key])}
		}
	}
	s.watchers[wt] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.watchers, wt)
		s.mu.Unlock()
	}()

	timeout := time.Hour
	if seconds, err := strconv.Atoi(query.Get("timeoutSeconds")); err == nil && seconds > 0 {
		timeout = time.Duration(seconds) * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	encoder := json.NewEncoder(w)
	for {
		select {
		case ev := <-wt.events:
			if err := encoder.Encode(ev); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case <-wt.done:
			for {
				select {
				case ev := <-wt.events:
					_ = encoder.Encode(ev)
				default:
					return
				}
			}
		case <-timer.C:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) admitLocked(kind RequestKind, r *http.Request) (int, bool) {
	s.counts[kind]++
	if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
		return http.StatusUnauthorized, false
	}
	f := s.failures[kind]
	if f == nil {
		return 0, true
	}
	if f.times > 0 {
		f.times--
		if f.times == 0 {
			delete(s.failures, kind)
		}
	}
	return f.code, false
}

func (s *Server) storeLocked(app Application) {
	key := appKey(app.Namespace, app.Name)
	s.version++
	s.apps[key] = app
	s.versions[key] = s.version
}

func (s *Server) broadcastLocked(namespace, name, kind string) {
	key := appKey(namespace, name)
	object := s.apps[key].Object(s.versions[key])
	for w := range s.watchers {
		if w.namespace != namespace || w.name != name {
			continue
		}
		select {
		case w.events <- event{Type: kind, Object: object}:
		default:
			w.stop()
			delete(s.watchers, w)
		}
	}
}

func appKey(namespace, name string) string {
	return namespace + "/" + name
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for key, values := range v {
		out[key] = append([]string(nil), values...)
	}
	return out
}

func status(code int, reason, message string) map[string]any {
	return map[string]any{
		"kind":       "Status",
		"apiVersion": "v1",
		"metadata":   map[string]any{},
		"status":     "Failure",
		"message":    message,
		"reason":     reason,
		"code":       code,
	}
}

func writeStatus(w http.ResponseWriter, code int) {
	reason := strings.ReplaceAll(http.StatusText(code), " ", "")
	writeJSON(w, code, status(code, reason, "argocdtest: injected "+strconv.Itoa(code)))
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
