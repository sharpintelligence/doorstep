package registry

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	mediaOCIIndex         = "application/vnd.oci.image.index.v1+json"
	mediaOCIManifest      = "application/vnd.oci.image.manifest.v1+json"
	mediaDockerList       = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaDockerManifest   = "application/vnd.docker.distribution.manifest.v2+json"
	fakeService           = "fake-registry"
	testUserAgent         = "doorstep-test/1.0"
	errCodeManifest       = "MANIFEST_UNKNOWN"
	errCodeName           = "NAME_UNKNOWN"
	errCodeDenied         = "DENIED"
	errCodeUnauthorized   = "UNAUTHORIZED"
	errCodeTooManyRequest = "TOOMANYREQUESTS"
)

type authMode int

const (
	authNone authMode = iota
	authBearer
	authBasic
)

type manifest struct {
	mediaType string
	body      []byte
}

func (m manifest) digest() string {
	sum := sha256.Sum256(m.body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type fakeConfig struct {
	auth            authMode
	accounts        map[string]string
	manifests       map[string]manifest
	omitHeadDigest  bool
	headStatus      int
	manifestStatus  int
	manifestErrCode string
	manifestDelay   time.Duration
}

type recordedRequest struct {
	method        string
	path          string
	query         string
	accept        string
	authorization string
	userAgent     string
}

type fakeRegistry struct {
	cfg    fakeConfig
	server *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
}

func newFakeRegistry(t *testing.T, cfg fakeConfig) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{cfg: cfg}
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(f.serveHTTP))
	f.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.server.Start()
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeRegistry) host() string {
	return strings.TrimPrefix(f.server.URL, "http://")
}

func (f *fakeRegistry) repo(name string) string {
	return f.host() + "/" + name
}

func (f *fakeRegistry) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

func (f *fakeRegistry) manifestRequests() []recordedRequest {
	var out []recordedRequest
	for _, r := range f.recorded() {
		if strings.Contains(r.path, "/manifests/") {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeRegistry) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		method:        r.Method,
		path:          r.URL.Path,
		query:         r.URL.RawQuery,
		accept:        r.Header.Get("Accept"),
		authorization: r.Header.Get("Authorization"),
		userAgent:     r.Header.Get("User-Agent"),
	})
	f.mu.Unlock()

	switch {
	case r.URL.Path == "/v2/" || r.URL.Path == "/v2":
		f.servePing(w, r)
	case r.URL.Path == "/token":
		f.serveToken(w, r)
	case strings.HasPrefix(r.URL.Path, "/v2/") && strings.Contains(r.URL.Path, "/manifests/"):
		f.serveManifest(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeRegistry) servePing(w http.ResponseWriter, r *http.Request) {
	switch f.cfg.auth {
	case authNone:
		w.WriteHeader(http.StatusOK)
	case authBearer:
		f.challengeBearer(w, "")
	case authBasic:
		if f.basicAuthorized(r, "") {
			w.WriteHeader(http.StatusOK)
			return
		}
		f.challengeBasic(w)
	}
}

func (f *fakeRegistry) challengeBearer(w http.ResponseWriter, scope string) {
	challenge := fmt.Sprintf(`Bearer realm="%s/token",service="%s"`, f.server.URL, fakeService)
	if scope != "" {
		challenge += fmt.Sprintf(`,scope="%s"`, scope)
	}
	w.Header().Set("WWW-Authenticate", challenge)
	writeRegistryError(w, http.StatusUnauthorized, errCodeUnauthorized)
}

func (f *fakeRegistry) challengeBasic(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
	writeRegistryError(w, http.StatusUnauthorized, errCodeUnauthorized)
}

func (f *fakeRegistry) account(repo string) (string, bool) {
	best, found := "", false
	var account string
	for prefix, userPass := range f.cfg.accounts {
		if prefix != "" && repo != prefix && !strings.HasPrefix(repo, prefix+"/") {
			continue
		}
		if !found || len(prefix) > len(best) {
			best, account, found = prefix, userPass, true
		}
	}
	return account, found
}

func (f *fakeRegistry) basicAuthorized(r *http.Request, repo string) bool {
	want, required := f.account(repo)
	if !required {
		return true
	}
	user, password, ok := r.BasicAuth()
	return ok && user+":"+password == want
}

func (f *fakeRegistry) serveToken(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("service") != fakeService {
		writeRegistryError(w, http.StatusBadRequest, "UNSUPPORTED")
		return
	}
	scope := r.URL.Query().Get("scope")
	repo, ok := strings.CutPrefix(scope, "repository:")
	if !ok || !strings.HasSuffix(repo, ":pull") {
		writeRegistryError(w, http.StatusBadRequest, "UNSUPPORTED")
		return
	}
	repo = strings.TrimSuffix(repo, ":pull")
	if !f.basicAuthorized(r, repo) {
		writeRegistryError(w, http.StatusUnauthorized, errCodeUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": tokenFor(repo)})
}

func tokenFor(repo string) string {
	return "token-" + base64.RawURLEncoding.EncodeToString([]byte(repo))
}

func (f *fakeRegistry) serveManifest(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v2/")
	idx := strings.LastIndex(rest, "/manifests/")
	repo, ref := rest[:idx], rest[idx+len("/manifests/"):]

	switch f.cfg.auth {
	case authNone:
	case authBearer:
		if r.Header.Get("Authorization") != "Bearer "+tokenFor(repo) {
			f.challengeBearer(w, "repository:"+repo+":pull")
			return
		}
	case authBasic:
		if !f.basicAuthorized(r, repo) {
			f.challengeBasic(w)
			return
		}
	}

	if f.cfg.manifestDelay > 0 {
		select {
		case <-time.After(f.cfg.manifestDelay):
		case <-r.Context().Done():
			return
		}
	}
	if r.Method == http.MethodHead && f.cfg.headStatus != 0 {
		w.WriteHeader(f.cfg.headStatus)
		return
	}
	if f.cfg.manifestStatus != 0 {
		writeRegistryError(w, f.cfg.manifestStatus, f.cfg.manifestErrCode)
		return
	}

	m, found := f.lookupManifest(repo, ref)
	if !found {
		code := errCodeManifest
		if !f.knownRepo(repo) {
			code = errCodeName
		}
		writeRegistryError(w, http.StatusNotFound, code)
		return
	}

	w.Header().Set("Content-Type", m.mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(m.body)))
	if r.Method != http.MethodHead || !f.cfg.omitHeadDigest {
		w.Header().Set("Docker-Content-Digest", m.digest())
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(m.body)
	}
}

func (f *fakeRegistry) lookupManifest(repo, ref string) (manifest, bool) {
	if m, ok := f.cfg.manifests[repo+":"+ref]; ok {
		return m, true
	}
	for key, m := range f.cfg.manifests {
		if strings.HasPrefix(key, repo+":") && m.digest() == ref {
			return m, true
		}
	}
	return manifest{}, false
}

func (f *fakeRegistry) knownRepo(repo string) bool {
	for key := range f.cfg.manifests {
		if strings.HasPrefix(key, repo+":") {
			return true
		}
	}
	return false
}

func writeRegistryError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if code == "" {
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"code": code, "message": strings.ToLower(code)}},
	})
}

const commitSHA = "0123456789abcdef0123456789abcdef01234567"

var (
	childManifest = manifest{
		mediaType: mediaOCIManifest,
		body:      []byte(`{"schemaVersion":2,"mediaType":"` + mediaOCIManifest + `","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`),
	}
	ociIndex = manifest{
		mediaType: mediaOCIIndex,
		body:      []byte(`{"schemaVersion":2,"mediaType":"` + mediaOCIIndex + `","manifests":[{"mediaType":"` + mediaOCIManifest + `","digest":"` + childManifest.digest() + `","size":` + strconv.Itoa(len(childManifest.body)) + `,"platform":{"architecture":"amd64","os":"linux"}}]}`),
	}
	dockerList = manifest{
		mediaType: mediaDockerList,
		body:      []byte(`{"schemaVersion":2,"mediaType":"` + mediaDockerList + `","manifests":[]}`),
	}
	dockerManifest = manifest{
		mediaType: mediaDockerManifest,
		body:      []byte(`{"schemaVersion":2,"mediaType":"` + mediaDockerManifest + `","config":{"mediaType":"application/vnd.docker.container.image.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`),
	}
)

func basicPair(user, password string) string {
	return user + ":" + password
}

func TestResolveManifestKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		manifest manifest
	}{
		{name: "oci index", manifest: ociIndex},
		{name: "oci manifest", manifest: childManifest},
		{name: "docker manifest list", manifest: dockerList},
		{name: "docker v2 manifest", manifest: dockerManifest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeRegistry(t, fakeConfig{
				manifests: map[string]manifest{
					"team/app:" + commitSHA:              tt.manifest,
					"team/app:" + childManifest.digest(): childManifest,
				},
			})
			resolver := New(Options{UserAgent: testUserAgent})

			got, err := resolver.Resolve(t.Context(), fake.repo("team/app"), commitSHA)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if want := Digest(tt.manifest.digest()); got != want {
				t.Errorf("Resolve() = %s, want %s", got, want)
			}

			reqs := fake.manifestRequests()
			if len(reqs) != 1 || reqs[0].method != http.MethodHead {
				t.Fatalf("manifest requests = %+v, want a single HEAD", reqs)
			}
			for _, mt := range []string{mediaOCIIndex, mediaOCIManifest, mediaDockerList, mediaDockerManifest} {
				if !strings.Contains(reqs[0].accept, mt) {
					t.Errorf("Accept %q does not contain %q", reqs[0].accept, mt)
				}
			}
			if reqs[0].path != "/v2/team/app/manifests/"+commitSHA {
				t.Errorf("path = %q", reqs[0].path)
			}
			for _, r := range fake.recorded() {
				if !strings.Contains(r.userAgent, testUserAgent) {
					t.Errorf("%s %s User-Agent = %q, want it to contain %q", r.method, r.path, r.userAgent, testUserAgent)
				}
			}
		})
	}
}

func TestResolveIndexIsNotChildDigest(t *testing.T) {
	t.Parallel()

	fake := newFakeRegistry(t, fakeConfig{
		manifests: map[string]manifest{"app:v1": ociIndex, "app:child": childManifest},
	})
	got, err := New(Options{}).Resolve(t.Context(), fake.repo("app"), "v1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got == Digest(childManifest.digest()) {
		t.Fatalf("Resolve() returned the child manifest digest %s", got)
	}
	if got != Digest(ociIndex.digest()) {
		t.Errorf("Resolve() = %s, want index digest %s", got, ociIndex.digest())
	}
}

func TestResolveAuth(t *testing.T) {
	t.Parallel()

	manifests := map[string]manifest{
		"owner-a/app:" + commitSHA: dockerManifest,
		"owner-b/app:" + commitSHA: childManifest,
	}
	tests := []struct {
		name        string
		auth        authMode
		accounts    map[string]string
		credentials func(host string) Credentials
		repo        string
		want        manifest
		wantErr     error
		wantBasic   string
	}{
		{
			name: "anonymous bearer",
			auth: authBearer,
			repo: "owner-a/app",
			want: dockerManifest,
		},
		{
			name:     "bearer with basic credentials",
			auth:     authBearer,
			accounts: map[string]string{"": basicPair("robot", "s3cret")},
			credentials: func(host string) Credentials {
				return mustCredentials(t, map[string]Credential{host: {Username: "robot", Password: "s3cret"}})
			},
			repo:      "owner-a/app",
			want:      dockerManifest,
			wantBasic: basicPair("robot", "s3cret"),
		},
		{
			name:     "bearer with registry token skips the token endpoint",
			auth:     authBearer,
			accounts: map[string]string{"": basicPair("robot", "s3cret")},
			credentials: func(host string) Credentials {
				return mustCredentials(t, map[string]Credential{host: {RegistryToken: tokenFor("owner-a/app")}})
			},
			repo: "owner-a/app",
			want: dockerManifest,
		},
		{
			name:     "bearer with wrong credentials is denied",
			auth:     authBearer,
			accounts: map[string]string{"": basicPair("robot", "s3cret")},
			credentials: func(host string) Credentials {
				return mustCredentials(t, map[string]Credential{host: {Username: "robot", Password: "wrong"}})
			},
			repo:    "owner-a/app",
			wantErr: ErrDenied,
		},
		{
			name:     "bearer without credentials when required is denied",
			auth:     authBearer,
			accounts: map[string]string{"": basicPair("robot", "s3cret")},
			repo:     "owner-a/app",
			wantErr:  ErrDenied,
		},
		{
			name:     "basic auth",
			auth:     authBasic,
			accounts: map[string]string{"": basicPair("robot", "s3cret")},
			credentials: func(host string) Credentials {
				return mustCredentials(t, map[string]Credential{host: {Username: "robot", Password: "s3cret"}})
			},
			repo:      "owner-b/app",
			want:      childManifest,
			wantBasic: basicPair("robot", "s3cret"),
		},
		{
			name:     "basic auth with wrong credentials is denied",
			auth:     authBasic,
			accounts: map[string]string{"": basicPair("robot", "s3cret")},
			credentials: func(host string) Credentials {
				return mustCredentials(t, map[string]Credential{host: {Username: "robot", Password: "nope"}})
			},
			repo:    "owner-b/app",
			wantErr: ErrDenied,
		},
		{
			name: "path scoped credential for owner a",
			auth: authBearer,
			accounts: map[string]string{
				"owner-a": basicPair("alice", "token-a"),
				"owner-b": basicPair("bob", "token-b"),
			},
			credentials: func(host string) Credentials {
				return mustCredentials(t, map[string]Credential{
					host:              {Username: "fallback", Password: "fallback"},
					host + "/owner-a": {Username: "alice", Password: "token-a"},
					host + "/owner-b": {Username: "bob", Password: "token-b"},
				})
			},
			repo:      "owner-a/app",
			want:      dockerManifest,
			wantBasic: basicPair("alice", "token-a"),
		},
		{
			name: "path scoped credential for owner b",
			auth: authBearer,
			accounts: map[string]string{
				"owner-a": basicPair("alice", "token-a"),
				"owner-b": basicPair("bob", "token-b"),
			},
			credentials: func(host string) Credentials {
				return mustCredentials(t, map[string]Credential{
					host:              {Username: "fallback", Password: "fallback"},
					host + "/owner-a": {Username: "alice", Password: "token-a"},
					host + "/owner-b": {Username: "bob", Password: "token-b"},
				})
			},
			repo:      "owner-b/app",
			want:      childManifest,
			wantBasic: basicPair("bob", "token-b"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeRegistry(t, fakeConfig{auth: tt.auth, accounts: tt.accounts, manifests: manifests})
			opts := Options{}
			if tt.credentials != nil {
				opts.Credentials = tt.credentials(fake.host())
			}

			got, err := New(opts).Resolve(t.Context(), fake.repo(tt.repo), commitSHA)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Resolve() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got != Digest(tt.want.digest()) {
				t.Errorf("Resolve() = %s, want %s", got, tt.want.digest())
			}
			if tt.wantBasic != "" {
				wantHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte(tt.wantBasic))
				if !sentAuthorization(fake.recorded(), wantHeader) {
					t.Errorf("no request carried %q: %+v", wantHeader, fake.recorded())
				}
			}
			if tt.auth == authBearer {
				assertTokenScope(t, fake.recorded(), "repository:"+tt.repo+":pull")
			}
		})
	}
}

func assertTokenScope(t *testing.T, reqs []recordedRequest, scope string) {
	t.Helper()
	for _, r := range reqs {
		if r.path != "/token" {
			continue
		}
		if !strings.Contains(r.query, "scope="+strings.ReplaceAll(strings.ReplaceAll(scope, ":", "%3A"), "/", "%2F")) {
			t.Errorf("token request query %q does not ask for scope %q", r.query, scope)
		}
	}
}

func sentAuthorization(reqs []recordedRequest, header string) bool {
	for _, r := range reqs {
		if r.authorization == header {
			return true
		}
	}
	return false
}

func mustCredentials(t *testing.T, creds map[string]Credential) Credentials {
	t.Helper()
	auths := make(map[string]map[string]string, len(creds))
	for key, c := range creds {
		auths[key] = map[string]string{
			"username":      c.Username,
			"password":      c.Password,
			"identitytoken": c.IdentityToken,
			"registrytoken": c.RegistryToken,
		}
	}
	data, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parsed, err := ParseDockerConfig(data)
	if err != nil {
		t.Fatalf("ParseDockerConfig: %v", err)
	}
	return parsed
}

func TestResolveCredentialsNotSentToOtherHosts(t *testing.T) {
	t.Parallel()

	private := newFakeRegistry(t, fakeConfig{
		auth:      authBearer,
		accounts:  map[string]string{"": basicPair("robot", "s3cret")},
		manifests: map[string]manifest{"app:v1": dockerManifest},
	})
	public := newFakeRegistry(t, fakeConfig{
		auth:      authBearer,
		manifests: map[string]manifest{"app:v1": childManifest},
	})
	creds := mustCredentials(t, map[string]Credential{private.host(): {Username: "robot", Password: "s3cret"}})
	resolver := New(Options{Credentials: creds})

	if _, err := resolver.Resolve(t.Context(), private.repo("app"), "v1"); err != nil {
		t.Fatalf("Resolve private: %v", err)
	}
	got, err := resolver.Resolve(t.Context(), public.repo("app"), "v1")
	if err != nil {
		t.Fatalf("Resolve public: %v", err)
	}
	if got != Digest(childManifest.digest()) {
		t.Errorf("Resolve public = %s, want %s", got, childManifest.digest())
	}
	for _, r := range public.recorded() {
		if strings.HasPrefix(r.authorization, "Basic ") {
			t.Errorf("public registry received basic credentials on %s %s", r.method, r.path)
		}
	}
}

func TestResolveErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		cfg           fakeConfig
		repo          string
		tag           string
		wantIs        error
		wantNotIs     []error
		wantManifests int
	}{
		{
			name:          "unknown tag",
			cfg:           fakeConfig{manifests: map[string]manifest{"app:v1": dockerManifest}},
			repo:          "app",
			tag:           "v2",
			wantIs:        ErrNotFound,
			wantManifests: 1,
		},
		{
			name:          "unknown repository",
			cfg:           fakeConfig{manifests: map[string]manifest{"app:v1": dockerManifest}},
			repo:          "other",
			tag:           "v1",
			wantIs:        ErrNotFound,
			wantManifests: 1,
		},
		{
			name:          "unknown tag behind bearer auth",
			cfg:           fakeConfig{auth: authBearer, manifests: map[string]manifest{"app:v1": dockerManifest}},
			repo:          "app",
			tag:           "v2",
			wantIs:        ErrNotFound,
			wantManifests: 1,
		},
		{
			name:          "forbidden",
			cfg:           fakeConfig{manifestStatus: http.StatusForbidden, manifestErrCode: errCodeDenied},
			repo:          "app",
			tag:           "v1",
			wantIs:        ErrDenied,
			wantManifests: 1,
		},
		{
			name:          "server error is transient",
			cfg:           fakeConfig{manifestStatus: http.StatusInternalServerError},
			repo:          "app",
			tag:           "v1",
			wantNotIs:     []error{ErrNotFound, ErrDenied, ErrInvalidReference},
			wantManifests: 1,
		},
		{
			name:          "bad gateway is transient",
			cfg:           fakeConfig{manifestStatus: http.StatusBadGateway},
			repo:          "app",
			tag:           "v1",
			wantNotIs:     []error{ErrNotFound, ErrDenied, ErrInvalidReference},
			wantManifests: 1,
		},
		{
			name:          "rate limited is transient",
			cfg:           fakeConfig{manifestStatus: http.StatusTooManyRequests, manifestErrCode: errCodeTooManyRequest},
			repo:          "app",
			tag:           "v1",
			wantNotIs:     []error{ErrNotFound, ErrDenied, ErrInvalidReference},
			wantManifests: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeRegistry(t, tt.cfg)

			got, err := New(Options{}).Resolve(t.Context(), fake.repo(tt.repo), tt.tag)
			if err == nil {
				t.Fatalf("Resolve() = %s, want error", got)
			}
			if got != "" {
				t.Errorf("Resolve() digest = %q on error", got)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("Resolve() error = %v, want %v", err, tt.wantIs)
			}
			for _, target := range tt.wantNotIs {
				if errors.Is(err, target) {
					t.Errorf("Resolve() error = %v, must not be %v", err, target)
				}
			}
			if !strings.Contains(err.Error(), fake.repo(tt.repo)+":"+tt.tag) {
				t.Errorf("error %q does not name the reference", err)
			}
			if n := len(fake.manifestRequests()); n != tt.wantManifests {
				t.Errorf("manifest requests = %d, want %d (no retries, no GET fallback)", n, tt.wantManifests)
			}
		})
	}
}

func TestResolveFallsBackToGet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  fakeConfig
	}{
		{
			name: "missing digest header on HEAD",
			cfg:  fakeConfig{omitHeadDigest: true},
		},
		{
			name: "HEAD not allowed",
			cfg:  fakeConfig{headStatus: http.StatusMethodNotAllowed},
		},
		{
			name: "missing digest header behind bearer auth",
			cfg:  fakeConfig{auth: authBearer, omitHeadDigest: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.cfg
			cfg.manifests = map[string]manifest{"app:" + commitSHA: ociIndex}
			fake := newFakeRegistry(t, cfg)

			got, err := New(Options{}).Resolve(t.Context(), fake.repo("app"), commitSHA)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			sum := sha256.Sum256(ociIndex.body)
			if want := Digest("sha256:" + hex.EncodeToString(sum[:])); got != want {
				t.Errorf("Resolve() = %s, want %s", got, want)
			}
			reqs := fake.manifestRequests()
			if len(reqs) != 2 || reqs[0].method != http.MethodHead || reqs[1].method != http.MethodGet {
				t.Errorf("manifest requests = %+v, want HEAD then GET", reqs)
			}
		})
	}
}

func TestResolveInvalidReference(t *testing.T) {
	t.Parallel()

	fake := newFakeRegistry(t, fakeConfig{manifests: map[string]manifest{"app:v1": dockerManifest}})
	tests := []struct {
		name string
		repo string
		tag  string
	}{
		{name: "empty tag", repo: fake.repo("app"), tag: ""},
		{name: "repository with tag", repo: fake.repo("app:v1"), tag: "v1"},
		{name: "repository with digest", repo: fake.repo("app@" + dockerManifest.digest()), tag: "v1"},
		{name: "uppercase repository", repo: fake.repo("App"), tag: "v1"},
		{name: "empty repository", repo: "", tag: "v1"},
		{name: "tag with slash", repo: fake.repo("app"), tag: "feature/x"},
		{name: "tag with colon", repo: fake.repo("app"), tag: "a:b"},
		{name: "tag starting with dot", repo: fake.repo("app"), tag: ".v1"},
		{name: "tag too long", repo: fake.repo("app"), tag: strings.Repeat("a", 129)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(Options{}).Resolve(t.Context(), tt.repo, tt.tag)
			if !errors.Is(err, ErrInvalidReference) {
				t.Fatalf("Resolve() error = %v, want ErrInvalidReference", err)
			}
		})
	}
	t.Cleanup(func() {
		if reqs := fake.recorded(); len(reqs) != 0 {
			t.Errorf("invalid references reached the registry: %+v", reqs)
		}
	})
}

func TestResolveTransportFailures(t *testing.T) {
	t.Parallel()

	closed := httptest.NewServer(http.NotFoundHandler())
	closedRepo := strings.TrimPrefix(closed.URL, "http://") + "/app"
	closed.Close()

	tests := []struct {
		name    string
		run     func(t *testing.T) error
		wantIs  error
		maxTime time.Duration
	}{
		{
			name: "connection refused",
			run: func(t *testing.T) error {
				t.Helper()
				_, err := New(Options{}).Resolve(t.Context(), closedRepo, "v1")
				return err
			},
		},
		{
			name: "context cancelled",
			run: func(t *testing.T) error {
				t.Helper()
				fake := newFakeRegistry(t, fakeConfig{manifests: map[string]manifest{"app:v1": dockerManifest}})
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				_, err := New(Options{}).Resolve(ctx, fake.repo("app"), "v1")
				return err
			},
			wantIs: context.Canceled,
		},
		{
			name: "context cancelled mid request",
			run: func(t *testing.T) error {
				t.Helper()
				fake := newFakeRegistry(t, fakeConfig{
					manifests:     map[string]manifest{"app:v1": dockerManifest},
					manifestDelay: 10 * time.Second,
				})
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()
				_, err := New(Options{}).Resolve(ctx, fake.repo("app"), "v1")
				return err
			},
			wantIs:  context.DeadlineExceeded,
			maxTime: 5 * time.Second,
		},
		{
			name: "resolver timeout",
			run: func(t *testing.T) error {
				t.Helper()
				fake := newFakeRegistry(t, fakeConfig{
					manifests:     map[string]manifest{"app:v1": dockerManifest},
					manifestDelay: 10 * time.Second,
				})
				_, err := New(Options{Timeout: 100 * time.Millisecond}).Resolve(t.Context(), fake.repo("app"), "v1")
				return err
			},
			wantIs:  context.DeadlineExceeded,
			maxTime: 5 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			err := tt.run(t)
			if err == nil {
				t.Fatal("Resolve() succeeded, want error")
			}
			for _, sentinel := range []error{ErrNotFound, ErrDenied, ErrInvalidReference} {
				if errors.Is(err, sentinel) {
					t.Errorf("Resolve() error = %v, must not be %v", err, sentinel)
				}
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("Resolve() error = %v, want %v", err, tt.wantIs)
			}
			if tt.maxTime > 0 && time.Since(start) > tt.maxTime {
				t.Errorf("Resolve() took %s, want under %s", time.Since(start), tt.maxTime)
			}
		})
	}
}

type failingSource struct{}

func (failingSource) Lookup(string, string) (Credential, bool, error) {
	return Credential{}, false, errors.New("credentials unavailable")
}

func TestResolveCredentialSourceError(t *testing.T) {
	t.Parallel()

	fake := newFakeRegistry(t, fakeConfig{manifests: map[string]manifest{"app:v1": dockerManifest}})
	_, err := New(Options{Credentials: failingSource{}}).Resolve(t.Context(), fake.repo("app"), "v1")
	if err == nil || !strings.Contains(err.Error(), "credentials unavailable") {
		t.Fatalf("Resolve() error = %v, want the credential source error", err)
	}
	if reqs := fake.recorded(); len(reqs) != 0 {
		t.Errorf("registry was contacted despite the credential error: %+v", reqs)
	}
}

func TestResolveWithFileCredentials(t *testing.T) {
	t.Parallel()

	fake := newFakeRegistry(t, fakeConfig{
		auth:      authBearer,
		accounts:  map[string]string{"": basicPair("robot", "new-token")},
		manifests: map[string]manifest{"app:v1": dockerManifest},
	})
	path := writeFile(t, dockerConfigJSON(fake.host(), "robot", "old-token"))
	resolver := New(Options{Credentials: FileCredentials{Path: path}})

	if _, err := resolver.Resolve(t.Context(), fake.repo("app"), "v1"); !errors.Is(err, ErrDenied) {
		t.Fatalf("Resolve() with stale credentials error = %v, want ErrDenied", err)
	}
	rewriteFile(t, path, dockerConfigJSON(fake.host(), "robot", "new-token"))
	got, err := resolver.Resolve(t.Context(), fake.repo("app"), "v1")
	if err != nil {
		t.Fatalf("Resolve() after rotation: %v", err)
	}
	if got != Digest(dockerManifest.digest()) {
		t.Errorf("Resolve() = %s, want %s", got, dockerManifest.digest())
	}
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()

	r := New(Options{})
	if r.timeout != DefaultTimeout {
		t.Errorf("timeout = %s, want %s", r.timeout, DefaultTimeout)
	}
	if r.userAgent != DefaultUserAgent {
		t.Errorf("userAgent = %q, want %q", r.userAgent, DefaultUserAgent)
	}
	if r.transport == nil || r.transport == http.DefaultTransport {
		t.Errorf("transport = %v, want a private clone of http.DefaultTransport", r.transport)
	}
	if r.credentials != nil {
		t.Errorf("credentials = %v, want nil (anonymous)", r.credentials)
	}
}
