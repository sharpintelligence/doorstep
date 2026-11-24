package notify

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const secretPath = "/services/T000/B000/s3cr3t-t0k3n"

func TestHTTPSenderPost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		handler       http.HandlerFunc
		wantErr       bool
		wantStatus    int
		wantExcerpt   string
		wantRetry     time.Duration
		wantRetryable bool
	}{
		{
			name: "ok",
			handler: func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.Method != http.MethodPost || string(body) != `{"a":1}` || r.Header.Get("X-Test") != "yes" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_, _ = io.WriteString(w, "ok")
			},
		},
		{
			name:    "204",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
		},
		{
			name: "redirect is not followed and is permanent",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://elsewhere.example.com/", http.StatusFound)
			},
			wantErr:    true,
			wantStatus: http.StatusFound,
		},
		{
			name: "429 with retry after seconds",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "12")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, "rate_limited")
			},
			wantErr:       true,
			wantStatus:    http.StatusTooManyRequests,
			wantExcerpt:   "rate_limited",
			wantRetry:     12 * time.Second,
			wantRetryable: true,
		},
		{
			name: "500 body is cut to an excerpt",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, strings.Repeat("x", 10000))
			},
			wantErr:       true,
			wantStatus:    http.StatusInternalServerError,
			wantExcerpt:   strings.Repeat("x", 256) + "…",
			wantRetryable: true,
		},
		{
			name: "400 is permanent",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, "invalid_payload\n")
			},
			wantErr:     true,
			wantStatus:  http.StatusBadRequest,
			wantExcerpt: "invalid_payload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			sender := newHTTPSender(nil, func() time.Time { return testTime })
			err := sender.post(t.Context(), srv.URL+secretPath, http.Header{"X-Test": {"yes"}}, []byte(`{"a":1}`))
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("post() = %v", err)
				}
				return
			}
			statusErr, ok := errors.AsType[*StatusError](err)
			if !ok {
				t.Fatalf("post() = %v, want *StatusError", err)
			}
			if statusErr.StatusCode != tt.wantStatus || statusErr.Excerpt != tt.wantExcerpt || statusErr.RetryAfter != tt.wantRetry {
				t.Errorf("post() = %+v, want status %d excerpt %q retry after %s", statusErr, tt.wantStatus, tt.wantExcerpt, tt.wantRetry)
			}
			if retryable(err) != tt.wantRetryable {
				t.Errorf("retryable(%v) = %v, want %v", err, retryable(err), tt.wantRetryable)
			}
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Errorf("error %q leaks the URL", err)
			}
		})
	}
}

func TestHTTPSenderRedactsURLFromTransportErrors(t *testing.T) {
	t.Parallel()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hang.Close)
	slow := &http.Client{Timeout: 50 * time.Millisecond}

	tests := []struct {
		name   string
		client *http.Client
		url    string
	}{
		{name: "connection refused", url: "http://" + addr + secretPath},
		{name: "client timeout", client: slow, url: hang.URL + secretPath},
		{name: "unknown host", url: "http://doorstep-test.invalid" + secretPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := newHTTPSender(tt.client, nil).post(t.Context(), tt.url, http.Header{}, nil)
			if err == nil {
				t.Fatal("post() = nil, want an error")
			}
			if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "/services/") {
				t.Errorf("error %q leaks the URL", err)
			}
			if !retryable(err) {
				t.Errorf("transport error %v is not retryable", err)
			}
		})
	}
}

func TestHTTPSenderRejectsInvalidEndpoints(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "hooks.slack.com/services/s3cr3t", "ftp://example.com/s3cr3t", "https:///s3cr3t", "mailto:s3cr3t@example.com", "http://[::1/s3cr3t"} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			err := newHTTPSender(nil, nil).post(t.Context(), raw, http.Header{}, nil)
			if !errors.Is(err, errInvalidEndpoint) {
				t.Fatalf("post(%q) = %v, want errInvalidEndpoint", raw, err)
			}
			if retryable(err) {
				t.Errorf("invalid endpoint error is retryable")
			}
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Errorf("error %q leaks the URL", err)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		want  time.Duration
	}{
		{value: "", want: 0},
		{value: "5", want: 5 * time.Second},
		{value: " 30 ", want: 30 * time.Second},
		{value: "0", want: 0},
		{value: "-3", want: 0},
		{value: "99999999999", want: time.Hour},
		{value: testTime.Add(90 * time.Second).Format(http.TimeFormat), want: 90 * time.Second},
		{value: testTime.Add(-time.Minute).Format(http.TimeFormat), want: 0},
		{value: "soon", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Parallel()
			if got := parseRetryAfter(tt.value, testTime); got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %s, want %s", tt.value, got, tt.want)
			}
		})
	}
}

func TestNewHTTPClient(t *testing.T) {
	t.Parallel()

	c := NewHTTPClient()
	if c.Timeout != 10*time.Second {
		t.Errorf("Timeout = %s, want 10s", c.Timeout)
	}
	if c.Transport == http.DefaultTransport {
		t.Error("client shares http.DefaultTransport")
	}
	if err := c.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect() = %v, want ErrUseLastResponse", err)
	}
}
