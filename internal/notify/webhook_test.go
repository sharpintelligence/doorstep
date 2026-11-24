package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const testDeliveryID = "00112233445566778899aabbccddeeff"

func TestWebhookPayloadGolden(t *testing.T) {
	t.Parallel()

	for _, s := range samples() {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			body, err := encodeJSON(webhookPayloadFor(s.n, testDeliveryID, testLinks()))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			assertGolden(t, "webhook/"+s.name+".json", body)
		})
	}
}

func TestWebhookPayloadWithoutLinks(t *testing.T) {
	t.Parallel()

	p := webhookPayloadFor(sampleNamed(t, "deploy-succeeded"), testDeliveryID, Links{})
	if p.Deployment.URL != "" {
		t.Errorf("deployment URL = %q without a public URL", p.Deployment.URL)
	}
}

type capturedRequest struct {
	header http.Header
	body   []byte
}

type captureServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []capturedRequest
	statuses []int
}

func newCaptureServer(t *testing.T, statuses ...int) *captureServer {
	t.Helper()
	c := &captureServer{statuses: statuses}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.requests = append(c.requests, capturedRequest{header: r.Header.Clone(), body: body})
		status := http.StatusNoContent
		if len(c.statuses) > 0 {
			status, c.statuses = c.statuses[0], c.statuses[1:]
		}
		c.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *captureServer) captured() []capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedRequest(nil), c.requests...)
}

func independentSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookSend(t *testing.T) {
	t.Parallel()

	srv := newCaptureServer(t, http.StatusBadGateway, http.StatusOK)
	var secretReads, urlReads atomic.Int32
	sink, err := NewWebhook(WebhookOptions{
		Name:      "hooks",
		URL:       func() (string, error) { urlReads.Add(1); return srv.URL + "/doorstep", nil },
		Secret:    func() (string, error) { secretReads.Add(1); return "topsecret", nil },
		Links:     testLinks(),
		UserAgent: "doorstep/test",
	})
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	if sink.Name() != "hooks" || sink.Type() != TypeWebhook {
		t.Errorf("Name, Type = %q, %q", sink.Name(), sink.Type())
	}
	d := newTestDispatcher(t, Options{Sinks: []Sink{sink}, Routes: map[string][]string{"blog": {"hooks"}}})
	d.Notify(t.Context(), sampleNamed(t, "deploy-succeeded"))
	closeDispatcher(t, d)

	reqs := srv.captured()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want 2 (one retry)", len(reqs))
	}
	if secretReads.Load() != 2 || urlReads.Load() != 2 {
		t.Errorf("secret read %d times, URL read %d times; want once per attempt", secretReads.Load(), urlReads.Load())
	}
	first, second := reqs[0], reqs[1]
	if string(first.body) != string(second.body) {
		t.Errorf("retry body differs:\n%s\n%s", first.body, second.body)
	}
	id := first.header.Get(HeaderDelivery)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) || second.header.Get(HeaderDelivery) != id {
		t.Errorf("delivery ids = %q, %q; want the same 128-bit hex id", id, second.header.Get(HeaderDelivery))
	}
	for _, r := range reqs {
		if got, want := r.header.Get(HeaderSignature), independentSignature("topsecret", r.body); got != want {
			t.Errorf("signature = %q, want %q", got, want)
		}
		if r.header.Get(HeaderEvent) != "deploy.succeeded" || r.header.Get("Content-Type") != "application/json" || r.header.Get("User-Agent") != "doorstep/test" {
			t.Errorf("headers = %v", r.header)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(first.body, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["id"] != id || payload["version"] != float64(1) || payload["type"] != "deploy.succeeded" {
		t.Errorf("payload = %v", payload)
	}
}

func TestWebhookUnsigned(t *testing.T) {
	t.Parallel()

	srv := newCaptureServer(t)
	sink, err := NewWebhook(WebhookOptions{Name: "hooks", URL: func() (string, error) { return srv.URL, nil }})
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	if err := sink.Prepare(sampleNamed(t, "lock-acquired"))(t.Context()); err != nil {
		t.Fatalf("delivery: %v", err)
	}
	reqs := srv.captured()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests", len(reqs))
	}
	if _, ok := reqs[0].header[HeaderSignature]; ok {
		t.Errorf("unsigned webhook sent %s", HeaderSignature)
	}
	if reqs[0].header.Get(HeaderEvent) != "lock.changed" {
		t.Errorf("event header = %q", reqs[0].header.Get(HeaderEvent))
	}
}

func TestWebhookSecretRotation(t *testing.T) {
	t.Parallel()

	srv := newCaptureServer(t)
	var mu sync.Mutex
	secret := "first"
	sink, err := NewWebhook(WebhookOptions{
		Name:   "hooks",
		URL:    func() (string, error) { return srv.URL, nil },
		Secret: func() (string, error) { mu.Lock(); defer mu.Unlock(); return secret, nil },
	})
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	n := sampleNamed(t, "deploy-started")
	if err := sink.Prepare(n)(t.Context()); err != nil {
		t.Fatalf("delivery: %v", err)
	}
	mu.Lock()
	secret = "second"
	mu.Unlock()
	if err := sink.Prepare(n)(t.Context()); err != nil {
		t.Fatalf("delivery: %v", err)
	}
	reqs := srv.captured()
	if reqs[0].header.Get(HeaderSignature) != independentSignature("first", reqs[0].body) ||
		reqs[1].header.Get(HeaderSignature) != independentSignature("second", reqs[1].body) {
		t.Error("rotated secret was not used for the next delivery")
	}
	if reqs[0].header.Get(HeaderDelivery) == reqs[1].header.Get(HeaderDelivery) {
		t.Error("separate deliveries share an id")
	}
}

func TestWebhookSecretErrorIsRetried(t *testing.T) {
	t.Parallel()

	srv := newCaptureServer(t)
	var calls atomic.Int32
	sink, err := NewWebhook(WebhookOptions{
		Name: "hooks",
		URL:  func() (string, error) { return srv.URL, nil },
		Secret: func() (string, error) {
			if calls.Add(1) == 1 {
				return "", errors.New("notification hooks secret: environment variable HOOK_SECRET is not set")
			}
			return "late", nil
		},
		NewID: func() string { return testDeliveryID },
	})
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	attempts, err := fastPolicy().Do(t.Context(), sink.Prepare(sampleNamed(t, "deploy-started")))
	if err != nil || attempts != 2 {
		t.Fatalf("Do() = %d, %v; want success on the second attempt", attempts, err)
	}
	reqs := srv.captured()
	if len(reqs) != 1 || reqs[0].header.Get(HeaderDelivery) != testDeliveryID {
		t.Errorf("requests = %+v", reqs)
	}
}

func TestWebhookAcceptsAndOptions(t *testing.T) {
	t.Parallel()

	url := func() (string, error) { return "https://example.com", nil }
	if _, err := NewWebhook(WebhookOptions{URL: url}); err == nil {
		t.Error("NewWebhook without a name succeeded")
	}
	if _, err := NewWebhook(WebhookOptions{Name: "x"}); err == nil {
		t.Error("NewWebhook without a URL succeeded")
	}
	if _, err := NewWebhook(WebhookOptions{Name: "x", URL: url, Kinds: []core.NotificationKind{}}); err == nil {
		t.Error("NewWebhook with no kinds succeeded")
	}
	locks, err := NewWebhook(WebhookOptions{Name: "x", URL: url, Kinds: []core.NotificationKind{core.NotificationLockChanged, core.NotificationDeployQueued}})
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	for _, s := range samples() {
		want := s.n.Kind == core.NotificationLockChanged || s.n.Kind == core.NotificationDeployQueued
		if got := locks.Accepts(s.n); got != want {
			t.Errorf("Accepts(%s) = %v, want %v", s.name, got, want)
		}
	}
}

func TestSign(t *testing.T) {
	t.Parallel()

	body := []byte(`{"version":1}`)
	if got, want := Sign([]byte("k"), body), independentSignature("k", body); got != want {
		t.Errorf("Sign() = %q, want %q", got, want)
	}
	if !strings.HasPrefix(Sign(nil, nil), "sha256=") {
		t.Error("signature has no algorithm prefix")
	}
	if a, b := newDeliveryID(), newDeliveryID(); a == b {
		t.Error("delivery ids repeat")
	}
}
