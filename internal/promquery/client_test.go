package promquery_test

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/promquery"
)

type captured struct {
	method string
	path   string
	form   url.Values
	header http.Header
}

type fakeServer struct {
	mu       sync.Mutex
	requests []captured
	status   int
	body     string
	handler  http.HandlerFunc
}

func newServer(t *testing.T, status int, body string) (*fakeServer, *httptest.Server) {
	t.Helper()
	f := &fakeServer{status: status, body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.requests = append(f.requests, captured{method: r.Method, path: r.URL.Path, form: r.PostForm, header: r.Header.Clone()})
		handler := f.handler
		f.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeServer) last(t *testing.T) captured {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no request reached the server")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeServer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func newClient(t *testing.T, base string, mutate ...func(*promquery.Options)) *promquery.Client {
	t.Helper()
	opts := promquery.Options{BaseURL: base, UserAgent: "doorstep/test"}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := promquery.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

var at = time.Date(2026, 11, 24, 12, 30, 0, 0, time.UTC)

func TestQueryDecodesResults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantType   promquery.ResultType
		wantSeries []promquery.Series
	}{
		{
			name:     "scalar",
			body:     `{"status":"success","data":{"resultType":"scalar","result":[1764000000.5,"0.25"]}}`,
			wantType: promquery.ResultScalar,
			wantSeries: []promquery.Series{{Labels: map[string]string{}, Points: []promquery.Point{
				{At: time.Unix(1764000000, 500000000).UTC(), Value: 0.25},
			}}},
		},
		{
			name:     "vector",
			body:     `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"job":"api"},"value":[1764000000,"3"]},{"metric":{},"value":[1764000000,"4"]}]}}`,
			wantType: promquery.ResultVector,
			wantSeries: []promquery.Series{
				{Labels: map[string]string{"job": "api"}, Points: []promquery.Point{{At: time.Unix(1764000000, 0).UTC(), Value: 3}}},
				{Labels: map[string]string{}, Points: []promquery.Point{{At: time.Unix(1764000000, 0).UTC(), Value: 4}}},
			},
		},
		{
			name:       "empty vector",
			body:       `{"status":"success","data":{"resultType":"vector","result":[]}}`,
			wantType:   promquery.ResultVector,
			wantSeries: []promquery.Series{},
		},
		{
			name:     "matrix",
			body:     `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"a":"b"},"values":[[1764000000,"1"],[1764000060,"2.5"]]}]}}`,
			wantType: promquery.ResultMatrix,
			wantSeries: []promquery.Series{{Labels: map[string]string{"a": "b"}, Points: []promquery.Point{
				{At: time.Unix(1764000000, 0).UTC(), Value: 1},
				{At: time.Unix(1764000060, 0).UTC(), Value: 2.5},
			}}},
		},
		{
			name:       "string",
			body:       `{"status":"success","data":{"resultType":"string","result":[1764000000,"hello"]}}`,
			wantType:   promquery.ResultString,
			wantSeries: nil,
		},
		{
			name:     "warnings are ignored",
			body:     `{"status":"success","warnings":["partial"],"data":{"resultType":"scalar","result":[1764000000,"1"]}}`,
			wantType: promquery.ResultScalar,
			wantSeries: []promquery.Series{{Labels: map[string]string{}, Points: []promquery.Point{
				{At: time.Unix(1764000000, 0).UTC(), Value: 1},
			}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, srv := newServer(t, http.StatusOK, tt.body)
			got, err := newClient(t, srv.URL).Query(t.Context(), "up", at)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if got.Type != tt.wantType {
				t.Errorf("type = %q, want %q", got.Type, tt.wantType)
			}
			if len(got.Series) != len(tt.wantSeries) {
				t.Fatalf("series = %+v, want %+v", got.Series, tt.wantSeries)
			}
			for i, s := range got.Series {
				want := tt.wantSeries[i]
				if len(s.Labels) != len(want.Labels) {
					t.Errorf("series %d labels = %v, want %v", i, s.Labels, want.Labels)
				}
				for k, v := range want.Labels {
					if s.Labels[k] != v {
						t.Errorf("series %d label %s = %q, want %q", i, k, s.Labels[k], v)
					}
				}
				if len(s.Points) != len(want.Points) {
					t.Fatalf("series %d points = %v, want %v", i, s.Points, want.Points)
				}
				for j, p := range s.Points {
					if !p.At.Equal(want.Points[j].At) || p.Value != want.Points[j].Value {
						t.Errorf("series %d point %d = %v, want %v", i, j, p, want.Points[j])
					}
				}
			}
		})
	}
}

func TestQuerySpecialValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		check func(float64) bool
	}{
		{"NaN", math.IsNaN},
		{"+Inf", func(v float64) bool { return math.IsInf(v, 1) }},
		{"-Inf", func(v float64) bool { return math.IsInf(v, -1) }},
		{"1e-3", func(v float64) bool { return v == 0.001 }},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Parallel()
			_, srv := newServer(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1764000000,"`+tt.value+`"]}]}}`)
			got, err := newClient(t, srv.URL).Query(t.Context(), "up", at)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if v := got.Series[0].Points[0].Value; !tt.check(v) {
				t.Errorf("value = %v", v)
			}
		})
	}
}

func TestQueryRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		basePath string
		wantPath string
	}{
		{name: "root", basePath: "", wantPath: "/api/v1/query"},
		{name: "root with slash", basePath: "/", wantPath: "/api/v1/query"},
		{name: "path prefix", basePath: "/prometheus", wantPath: "/prometheus/api/v1/query"},
		{name: "path prefix with slash", basePath: "/tenant/prom/", wantPath: "/tenant/prom/api/v1/query"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, srv := newServer(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
			if _, err := newClient(t, srv.URL+tt.basePath).Query(t.Context(), `sum(rate(x{a="b"}[5m]))`, at.Add(1500*time.Millisecond)); err != nil {
				t.Fatalf("Query: %v", err)
			}
			req := f.last(t)
			if req.method != http.MethodPost {
				t.Errorf("method = %s, want POST", req.method)
			}
			if req.path != tt.wantPath {
				t.Errorf("path = %s, want %s", req.path, tt.wantPath)
			}
			if got := req.form.Get("query"); got != `sum(rate(x{a="b"}[5m]))` {
				t.Errorf("query = %q", got)
			}
			if got := req.form.Get("time"); got != "2026-11-24T12:30:01.5Z" {
				t.Errorf("time = %q", got)
			}
			if got := req.header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
				t.Errorf("Content-Type = %q", got)
			}
			if got := req.header.Get("User-Agent"); got != "doorstep/test" {
				t.Errorf("User-Agent = %q", got)
			}
			if got := req.header.Get("Authorization"); got != "" {
				t.Errorf("Authorization = %q, want none", got)
			}
		})
	}
}

func TestQueryRangeRequest(t *testing.T) {
	t.Parallel()

	f, srv := newServer(t, http.StatusOK, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	c := newClient(t, srv.URL+"/prom")
	from := at.Add(-30 * time.Minute)
	got, err := c.QueryRange(t.Context(), "up", from, at, 90*time.Second)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if got.Type != promquery.ResultMatrix {
		t.Errorf("type = %q", got.Type)
	}
	req := f.last(t)
	want := map[string]string{"query": "up", "start": "2026-11-24T12:00:00Z", "end": "2026-11-24T12:30:00Z", "step": "90"}
	for k, v := range want {
		if got := req.form.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if req.path != "/prom/api/v1/query_range" {
		t.Errorf("path = %s", req.path)
	}

	if _, err := c.QueryRange(t.Context(), "up", from, at, 0); err == nil {
		t.Error("QueryRange accepted a zero step")
	}
	if f.count() != 1 {
		t.Errorf("requests = %d, want 1", f.count())
	}
}

func TestAuthorization(t *testing.T) {
	t.Parallel()

	secretErr := errors.New("prometheus main bearer_token: environment variable TOKEN is not set")
	tests := []struct {
		name      string
		authorize func(*http.Request) error
		want      string
		wantErr   error
	}{
		{
			name:      "bearer",
			authorize: promquery.BearerAuth(func() (string, error) { return "s3cret", nil }),
			want:      "Bearer s3cret",
		},
		{
			name:      "basic",
			authorize: promquery.BasicAuth("doorstep", func() (string, error) { return "pa:ss", nil }),
			want:      "Basic ZG9vcnN0ZXA6cGE6c3M=",
		},
		{
			name:      "bearer secret error",
			authorize: promquery.BearerAuth(func() (string, error) { return "", secretErr }),
			wantErr:   secretErr,
		},
		{
			name:      "basic secret error",
			authorize: promquery.BasicAuth("u", func() (string, error) { return "", secretErr }),
			wantErr:   secretErr,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, srv := newServer(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
			c := newClient(t, srv.URL, func(o *promquery.Options) { o.Authorize = tt.authorize })
			_, err := c.Query(t.Context(), "up", at)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				if f.count() != 0 {
					t.Error("request was sent without credentials")
				}
				return
			}
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if got := f.last(t).header.Get("Authorization"); got != tt.want {
				t.Errorf("Authorization = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSecretIsReadOnEveryRequest(t *testing.T) {
	t.Parallel()

	f, srv := newServer(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	tokens := []string{"one", "two"}
	var mu sync.Mutex
	reads := 0
	c := newClient(t, srv.URL, func(o *promquery.Options) {
		o.Authorize = promquery.BearerAuth(func() (string, error) {
			mu.Lock()
			defer mu.Unlock()
			token := tokens[reads]
			reads++
			return token, nil
		})
	})
	for _, want := range tokens {
		if _, err := c.Query(t.Context(), "up", at); err != nil {
			t.Fatalf("Query: %v", err)
		}
		if got := f.last(t).header.Get("Authorization"); got != "Bearer "+want {
			t.Errorf("Authorization = %q, want Bearer %s", got, want)
		}
	}
}

func TestErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		body      string
		wantAPI   *promquery.Error
		wantIs    error
		wantText  string
		forbidden string
	}{
		{
			name:     "bad query",
			status:   http.StatusBadRequest,
			body:     `{"status":"error","errorType":"bad_data","error":"1:5: parse error: unexpected end of input"}`,
			wantAPI:  &promquery.Error{Status: 400, Type: "bad_data", Message: "1:5: parse error: unexpected end of input"},
			wantText: "prometheus responded 400 Bad Request: bad_data: 1:5: parse error: unexpected end of input",
		},
		{
			name:     "execution error",
			status:   http.StatusUnprocessableEntity,
			body:     `{"status":"error","errorType":"execution","error":"many-to-many matching not allowed"}`,
			wantAPI:  &promquery.Error{Status: 422, Type: "execution", Message: "many-to-many matching not allowed"},
			wantText: "422 Unprocessable Entity: execution: many-to-many matching not allowed",
		},
		{
			name:     "server error with envelope",
			status:   http.StatusServiceUnavailable,
			body:     `{"status":"error","errorType":"unavailable","error":"overloaded"}`,
			wantAPI:  &promquery.Error{Status: 503, Type: "unavailable", Message: "overloaded"},
			wantText: "503 Service Unavailable: unavailable: overloaded",
		},
		{
			name:     "server error without envelope",
			status:   http.StatusInternalServerError,
			body:     "<html>upstream broke</html>",
			wantAPI:  &promquery.Error{Status: 500, Message: "<html>upstream broke</html>"},
			wantText: "500 Internal Server Error: <html>upstream broke</html>",
		},
		{
			name:     "long error body is cut",
			status:   http.StatusBadGateway,
			body:     strings.Repeat("x", 1000),
			wantText: strings.Repeat("x", 256) + "...",
		},
		{
			name:     "error status on 200",
			status:   http.StatusOK,
			body:     `{"status":"error","errorType":"timeout","error":"query timed out"}`,
			wantAPI:  &promquery.Error{Status: 200, Type: "timeout", Message: "query timed out"},
			wantText: "prometheus reported an error: timeout: query timed out",
		},
		{name: "not JSON", status: http.StatusOK, body: "hello", wantIs: promquery.ErrMalformedResponse},
		{name: "unknown result type", status: http.StatusOK, body: `{"status":"success","data":{"resultType":"histogram","result":[]}}`, wantIs: promquery.ErrMalformedResponse, wantText: `unknown result type "histogram"`},
		{name: "bad sample", status: http.StatusOK, body: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1]}]}}`, wantIs: promquery.ErrMalformedResponse},
		{name: "bad value", status: http.StatusOK, body: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"abc"]}]}}`, wantIs: promquery.ErrMalformedResponse},
		{name: "numeric value", status: http.StatusOK, body: `{"status":"success","data":{"resultType":"scalar","result":[1,2]}}`, wantIs: promquery.ErrMalformedResponse},
		{name: "bad time", status: http.StatusOK, body: `{"status":"success","data":{"resultType":"scalar","result":["x","2"]}}`, wantIs: promquery.ErrMalformedResponse},
		{name: "bad matrix", status: http.StatusOK, body: `{"status":"success","data":{"resultType":"matrix","result":{}}}`, wantIs: promquery.ErrMalformedResponse},
		{name: "oversized", status: http.StatusOK, body: `{"status":"success","data":{"resultType":"string","result":[1,"` + strings.Repeat("a", promquery.MaxResponseBytes) + `"]}}`, wantIs: promquery.ErrResponseTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, srv := newServer(t, tt.status, tt.body)
			c := newClient(t, srv.URL, func(o *promquery.Options) {
				o.Authorize = promquery.BearerAuth(func() (string, error) { return "topsecret", nil })
			})
			_, err := c.Query(t.Context(), "up", at)
			if err == nil {
				t.Fatal("Query succeeded")
			}
			if tt.wantAPI != nil {
				var apiErr *promquery.Error
				if !errors.As(err, &apiErr) {
					t.Fatalf("error %v is not a *promquery.Error", err)
				}
				if *apiErr != *tt.wantAPI {
					t.Errorf("error = %+v, want %+v", *apiErr, *tt.wantAPI)
				}
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error = %v, want %v", err, tt.wantIs)
			}
			if tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantText)
			}
			if strings.Contains(err.Error(), "topsecret") || strings.Contains(err.Error(), srv.URL) {
				t.Errorf("error %q leaks the token or URL", err)
			}
		})
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	t.Parallel()

	target, targetSrv := newServer(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	f, srv := newServer(t, 0, "")
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetSrv.URL+"/api/v1/query", http.StatusTemporaryRedirect)
	}
	c := newClient(t, srv.URL, func(o *promquery.Options) {
		o.Authorize = promquery.BearerAuth(func() (string, error) { return "topsecret", nil })
	})
	_, err := c.Query(t.Context(), "up", at)
	var apiErr *promquery.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTemporaryRedirect {
		t.Fatalf("error = %v, want a 307 *promquery.Error", err)
	}
	if target.count() != 0 {
		t.Error("redirect was followed")
	}
}

func TestTransportErrors(t *testing.T) {
	t.Parallel()

	t.Run("context cancelled", func(t *testing.T) {
		t.Parallel()
		release := make(chan struct{})
		f, srv := newServer(t, 0, "")
		f.handler = func(http.ResponseWriter, *http.Request) { <-release }
		t.Cleanup(func() { close(release) })
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		_, err := newClient(t, srv.URL).Query(ctx, "up", at)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context.DeadlineExceeded", err)
		}
		if strings.Contains(err.Error(), srv.URL) {
			t.Errorf("error %q contains the URL", err)
		}
	})

	t.Run("connection refused", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.NotFoundHandler())
		base := srv.URL
		srv.Close()
		_, err := newClient(t, base).Query(t.Context(), "up", at)
		if err == nil {
			t.Fatal("Query succeeded")
		}
		if strings.Contains(err.Error(), base) {
			t.Errorf("error %q contains the URL", err)
		}
	})
}

func TestNewValidatesBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		url string
		ok  bool
	}{
		{"http://prometheus:9090", true},
		{"https://prom.example.com/prefix/", true},
		{"", false},
		{"prometheus:9090", false},
		{"ftp://prom", false},
		{"http://", false},
		{"http://user:pw@prom", false},
		{"http://prom?x=1", false},
		{"http://prom?", false},
		{"http://prom#f", false},
		{"http://prom\x7f", false},
	}
	for _, tt := range tests {
		_, err := promquery.New(promquery.Options{BaseURL: tt.url})
		if (err == nil) != tt.ok {
			t.Errorf("New(%q) error = %v, want ok %v", tt.url, err, tt.ok)
		}
		if err != nil && !errors.Is(err, promquery.ErrInvalidBaseURL) {
			t.Errorf("New(%q) error = %v, want ErrInvalidBaseURL", tt.url, err)
		}
	}
}

func TestNewHTTPClient(t *testing.T) {
	t.Parallel()

	c := promquery.NewHTTPClient()
	if c.Timeout != promquery.DefaultTimeout {
		t.Errorf("timeout = %s", c.Timeout)
	}
	if c.CheckRedirect == nil || !errors.Is(c.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Error("redirects are followed")
	}
	if c.Transport == http.DefaultTransport {
		t.Error("transport is shared with http.DefaultTransport")
	}
}
