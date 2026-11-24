package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const applicationPath = "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/blog"

func TestClientGetSendsCredentialsAndDecodes(t *testing.T) {
	t.Parallel()

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"metadata":{"name":"blog"}}`)
	}, nil)
	files := newMemFiles(map[string]string{"/token": "first\n"})
	client, err := New(Config{Server: srv.URL, CAData: srv.ca.pem, Token: TokenFile("/token", files.read)}, Options{UserAgent: "doorstep/test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var out struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := client.Get(t.Context(), applicationPath, url.Values{"resourceVersion": {"7"}}, &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out.Metadata.Name != "blog" {
		t.Fatalf("name = %q", out.Metadata.Name)
	}
	files.set("/token", "second")
	if err := client.Get(t.Context(), applicationPath, nil, &out); err != nil {
		t.Fatalf("Get after rotation: %v", err)
	}

	reqs := srv.received()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	for i, want := range []string{"Bearer first", "Bearer second"} {
		if got := reqs[i].Header.Get("Authorization"); got != want {
			t.Errorf("request %d Authorization = %q, want %q", i, got, want)
		}
		if got := reqs[i].Header.Get("User-Agent"); got != "doorstep/test" {
			t.Errorf("request %d User-Agent = %q", i, got)
		}
		if got := reqs[i].Header.Get("Accept"); got != "application/json" {
			t.Errorf("request %d Accept = %q", i, got)
		}
		if reqs[i].URL.Path != applicationPath {
			t.Errorf("request %d path = %q", i, reqs[i].URL.Path)
		}
	}
	if got := reqs[0].URL.Query().Get("resourceVersion"); got != "7" {
		t.Errorf("resourceVersion = %q", got)
	}
}

func TestClientServerPathPrefix(t *testing.T) {
	t.Parallel()

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{}`)
	}, nil)
	client, err := New(Config{Server: srv.URL + "/k8s/clusters/c-1/", CAData: srv.ca.pem}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var out struct{}
	if err := client.Get(t.Context(), applicationPath, nil, &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := srv.received()[0].URL.Path; got != "/k8s/clusters/c-1"+applicationPath {
		t.Fatalf("path = %q", got)
	}
	if got := srv.received()[0].Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want none without a token", got)
	}
}

func TestClientRejectsUntrustedServer(t *testing.T) {
	t.Parallel()

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{}`)
	}, nil)
	other := newAuthority(t, "someone-else")
	tests := []struct {
		name string
		ca   []byte
	}{
		{name: "different CA", ca: other.pem},
		{name: "system roots", ca: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, err := New(Config{Server: srv.URL, CAData: tt.ca, Token: StaticToken("secret")}, Options{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			var out struct{}
			err = client.Get(t.Context(), applicationPath, nil, &out)
			if _, ok := errors.AsType[*tls.CertificateVerificationError](err); !ok {
				t.Fatalf("Get error = %v, want a certificate verification error", err)
			}
		})
	}
	if n := len(srv.received()); n != 0 {
		t.Fatalf("server saw %d requests", n)
	}
}

func TestClientCertificateAuth(t *testing.T) {
	t.Parallel()

	clientCA := newAuthority(t, "client-ca")
	pool := x509.NewCertPool()
	pool.AddCert(clientCA.cert)
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) == 0 || r.TLS.PeerCertificates[0].Subject.CommonName != "doorstep" {
			writeJSON(w, http.StatusUnauthorized, `{"kind":"Status","code":401,"message":"Unauthorized"}`)
			return
		}
		writeJSON(w, http.StatusOK, `{}`)
	}, pool)
	certPEM, keyPEM := clientCA.issue(t, "doorstep", x509.ExtKeyUsageClientAuth)
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	client, err := New(Config{Server: srv.URL, CAData: srv.ca.pem, ClientCert: &pair}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var out struct{}
	if err := client.Get(t.Context(), applicationPath, nil, &out); err != nil {
		t.Fatalf("Get with client certificate: %v", err)
	}

	anonymous, err := New(Config{Server: srv.URL, CAData: srv.ca.pem}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := anonymous.Get(t.Context(), applicationPath, nil, &out); err == nil {
		t.Fatal("Get without client certificate succeeded")
	}
}

func TestClientRefusesRedirects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		location string
	}{
		{name: "same host", location: "/elsewhere"},
		{name: "foreign host", location: "https://evil.example.com/steal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/elsewhere" {
					writeJSON(w, http.StatusOK, `{}`)
					return
				}
				w.Header().Set("Location", tt.location)
				w.WriteHeader(http.StatusFound)
			}, nil)
			client, err := New(Config{Server: srv.URL, CAData: srv.ca.pem, Token: StaticToken("secret")}, Options{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			var out struct{}
			if err := client.Get(t.Context(), applicationPath, nil, &out); !errors.Is(err, ErrRedirect) {
				t.Fatalf("Get error = %v, want ErrRedirect", err)
			}
			if n := len(srv.received()); n != 1 {
				t.Fatalf("server saw %d requests, want 1", n)
			}
		})
	}
}

func TestPinnedTransportRefusesForeignHosts(t *testing.T) {
	t.Parallel()

	var called atomic.Bool
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		writeJSON(w, http.StatusOK, `{}`)
	}, nil)
	client, err := New(Config{Server: srv.URL, CAData: srv.ca.pem, Token: StaticToken("secret")}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	origin, _ := url.Parse(srv.URL)
	tests := []struct {
		name string
		url  string
	}{
		{name: "other host", url: "https://evil.example.com" + applicationPath},
		{name: "other port", url: "https://" + origin.Hostname() + ":1" + applicationPath},
		{name: "plain http", url: "http://" + origin.Host + applicationPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tt.url, http.NoBody)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := client.http.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			if !errors.Is(err, ErrForeignHost) {
				t.Fatalf("Do error = %v, want ErrForeignHost", err)
			}
		})
	}
	if called.Load() {
		t.Fatal("request reached the server")
	}
}

func TestClientStatusErrors(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 2000)
	tests := []struct {
		name        string
		status      int
		body        string
		wantIs      error
		wantMessage string
		wantReason  string
	}{
		{
			name:        "not found",
			status:      http.StatusNotFound,
			body:        `{"kind":"Status","status":"Failure","reason":"NotFound","code":404,"message":"applications.argoproj.io \"blog\" not found"}`,
			wantIs:      ErrNotFound,
			wantMessage: `applications.argoproj.io "blog" not found`,
			wantReason:  "NotFound",
		},
		{
			name:        "forbidden",
			status:      http.StatusForbidden,
			body:        `{"kind":"Status","reason":"Forbidden","code":403,"message":"cannot get resource"}`,
			wantIs:      ErrForbidden,
			wantMessage: "cannot get resource",
			wantReason:  "Forbidden",
		},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"kind":"Status","code":401,"message":"Unauthorized"}`, wantIs: ErrForbidden, wantMessage: "Unauthorized"},
		{name: "gone", status: http.StatusGone, body: `{"kind":"Status","reason":"Expired","code":410,"message":"too old"}`, wantIs: ErrGone, wantMessage: "too old", wantReason: "Expired"},
		{name: "plain text server error", status: http.StatusInternalServerError, body: "boom"},
		{name: "long message is cut", status: http.StatusBadRequest, body: `{"message":"` + long + `"}`, wantMessage: long[:maxErrorMessageBytes] + "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, tt.status, tt.body)
			}, nil)
			client, err := New(Config{Server: srv.URL, CAData: srv.ca.pem}, Options{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			var out struct{}
			err = client.Get(t.Context(), applicationPath, nil, &out)
			statusErr, ok := errors.AsType[*StatusError](err)
			if !ok {
				t.Fatalf("Get error = %v, want *StatusError", err)
			}
			if statusErr.Code != tt.status || statusErr.Message != tt.wantMessage || statusErr.Reason != tt.wantReason {
				t.Fatalf("StatusError = %+v", statusErr)
			}
			for _, sentinel := range []error{ErrNotFound, ErrForbidden, ErrGone} {
				if got, want := errors.Is(err, sentinel), tt.wantIs != nil && errors.Is(sentinel, tt.wantIs); got != want {
					t.Errorf("errors.Is(%v) = %v, want %v", sentinel, got, want)
				}
			}
			if want := fmt.Sprintf("kube api: %d %s", tt.status, http.StatusText(tt.status)); !strings.HasPrefix(err.Error(), want) {
				t.Errorf("Error() = %q, want prefix %q", err.Error(), want)
			}
			if _, err := client.Stream(t.Context(), applicationPath, nil); !sameStatus(err, statusErr) {
				t.Errorf("Stream error = %v, want %v", err, statusErr)
			}
		})
	}
}

func sameStatus(err error, want *StatusError) bool {
	got, ok := errors.AsType[*StatusError](err)
	return ok && *got == *want
}

func TestStatusHelpers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code                   int
		notFound, forbid, gone bool
	}{
		{code: http.StatusNotFound, notFound: true},
		{code: http.StatusUnauthorized, forbid: true},
		{code: http.StatusForbidden, forbid: true},
		{code: http.StatusGone, gone: true},
		{code: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.code), func(t *testing.T) {
			t.Parallel()
			err := error(&StatusError{Code: tt.code})
			if IsNotFound(err) != tt.notFound || IsForbidden(err) != tt.forbid || IsGone(err) != tt.gone {
				t.Fatalf("helpers disagree for %d", tt.code)
			}
		})
	}
	if IsNotFound(errors.New("other")) {
		t.Fatal("IsNotFound(plain error) = true")
	}
	if status := ParseStatus(0, []byte(`{"code":410,"reason":"Expired","message":"gone"}`)); status.Code != http.StatusGone || !IsGone(status) {
		t.Fatalf("ParseStatus = %+v", status)
	}
}

func TestClientResponseTooLarge(t *testing.T) {
	t.Parallel()

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pad":"`))
		_, _ = w.Write([]byte(strings.Repeat("a", MaxResponseBytes)))
		_, _ = w.Write([]byte(`"}`))
	}, nil)
	client, err := New(Config{Server: srv.URL, CAData: srv.ca.pem}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var out struct{}
	if err := client.Get(t.Context(), applicationPath, nil, &out); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("Get error = %v, want ErrResponseTooLarge", err)
	}
}

func TestClientGetTimeout(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		writeJSON(w, http.StatusOK, `{}`)
	}, nil)
	defer close(release)
	client, err := New(Config{Server: srv.URL, CAData: srv.ca.pem}, Options{Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var out struct{}
	if err := client.Get(t.Context(), applicationPath, nil, &out); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get error = %v, want deadline exceeded", err)
	}
}

func TestClientStream(t *testing.T) {
	t.Parallel()

	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") != "1" {
			writeJSON(w, http.StatusBadRequest, `{}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"ADDED"}` + "\n"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(`{"type":"MODIFIED"}` + "\n"))
	}, nil)
	client, err := New(Config{Server: srv.URL, CAData: srv.ca.pem, Token: StaticToken("secret")}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body, err := client.Stream(t.Context(), "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications", url.Values{"watch": {"1"}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "{\"type\":\"ADDED\"}\n{\"type\":\"MODIFIED\"}\n" {
		t.Fatalf("stream = %q", data)
	}
	if got := srv.received()[0].Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestClientTokenErrorStopsRequest(t *testing.T) {
	t.Parallel()

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{}`)
	}, nil)
	files := newMemFiles(nil)
	client, err := New(Config{Server: srv.URL, CAData: srv.ca.pem, Token: TokenFile("/missing", files.read)}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var out struct{}
	err = client.Get(t.Context(), applicationPath, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "read kubernetes token file /missing") {
		t.Fatalf("Get error = %v", err)
	}
	if n := len(srv.received()); n != 0 {
		t.Fatalf("server saw %d requests", n)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "https", cfg: Config{Server: "https://10.0.0.1:6443"}},
		{name: "https with path", cfg: Config{Server: "https://rancher.example.com/k8s/clusters/c-1"}},
		{name: "http loopback v4", cfg: Config{Server: "http://127.0.0.1:8001"}},
		{name: "http localhost", cfg: Config{Server: "http://localhost:8001"}},
		{name: "http loopback v6", cfg: Config{Server: "http://[::1]:8001"}},
		{name: "http remote", cfg: Config{Server: "http://10.0.0.1:8080"}, wantErr: "only allowed for loopback"},
		{name: "other scheme", cfg: Config{Server: "ftp://10.0.0.1"}, wantErr: "must use https"},
		{name: "no host", cfg: Config{Server: "https:///path"}, wantErr: "absolute URL"},
		{name: "relative", cfg: Config{Server: "kube.local"}, wantErr: "absolute URL"},
		{name: "user info", cfg: Config{Server: "https://u:p@10.0.0.1"}, wantErr: "user information"},
		{name: "query", cfg: Config{Server: "https://10.0.0.1/?x=1"}, wantErr: "query string"},
		{name: "unparseable", cfg: Config{Server: "https://[::1"}, wantErr: "not a valid URL"},
		{name: "bad CA", cfg: Config{Server: "https://10.0.0.1", CAData: []byte("not pem")}, wantErr: "no PEM certificates"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, err := New(tt.cfg, Options{})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				if strings.HasSuffix(client.Server(), "/") {
					t.Fatalf("Server() = %q keeps a trailing slash", client.Server())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New error = %v, want ErrInvalidConfig containing %q", err, tt.wantErr)
			}
		})
	}
}
