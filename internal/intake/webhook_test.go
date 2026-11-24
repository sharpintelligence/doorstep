package intake_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/intake"
)

const webhookSecret = "It's a Secret to Everybody"

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type webhookHarness struct {
	intake        *fakeIntake
	deliveries    *fakeDeliveries
	installations *fakeInstallations
	observer      *fakeObserver
	handler       *intake.WebhookHandler
}

func newWebhookHarness(t *testing.T) *webhookHarness {
	t.Helper()
	h := &webhookHarness{
		intake:        &fakeIntake{},
		deliveries:    newFakeDeliveries(),
		installations: &fakeInstallations{},
		observer:      &fakeObserver{},
	}
	handler, err := intake.NewWebhook(intake.WebhookOptions{
		Secret:        []byte(webhookSecret),
		Intake:        h.intake,
		Deliveries:    h.deliveries,
		Installations: h.installations,
		Observer:      h.observer,
	})
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	h.handler = handler
	return h
}

type delivery struct {
	event     string
	id        string
	body      string
	signature string
	unsigned  bool
}

func (d delivery) request(ctx context.Context) *http.Request {
	body := []byte(d.body)
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if d.event != "" {
		r.Header.Set(github.EventHeader, d.event)
	}
	if d.id != "" {
		r.Header.Set(github.DeliveryHeader, d.id)
	}
	switch {
	case d.signature != "":
		r.Header.Set(github.SignatureHeader, d.signature)
	case !d.unsigned:
		r.Header.Set(github.SignatureHeader, sign(webhookSecret, body))
	}
	return r
}

func (h *webhookHarness) send(t *testing.T, d delivery) (int, map[string]string) {
	t.Helper()
	return serve(t, h.handler, d.request(t.Context()))
}

func serve(t *testing.T, handler http.Handler, r *http.Request) (int, map[string]string) {
	t.Helper()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body %q is not JSON: %v", w.Body.String(), err)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	return w.Code, body
}

func pushBody(ref, after string, deleted bool, sender, pusher string) string {
	b, _ := json.Marshal(map[string]any{
		"ref":        ref,
		"before":     shaB,
		"after":      after,
		"deleted":    deleted,
		"repository": map[string]any{"full_name": "Example-User/blog", "default_branch": "main"},
		"sender":     map[string]any{"login": sender},
		"pusher":     map[string]any{"name": pusher},
	})
	return string(b)
}

func TestWebhookHandlesEvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		event             string
		body              string
		wantStatus        string
		wantPushes        []intake.Push
		wantReevaluations []reevaluation
		wantClaimed       bool
		wantInstallations []int64
		wantRepositories  []string
	}{
		{
			name:       "push to a branch",
			event:      "push",
			body:       pushBody("refs/heads/main", shaA, false, "octocat", "The Octocat"),
			wantStatus: "processed",
			wantPushes: []intake.Push{{
				Repository: "Example-User/blog", Branch: "main", SHA: shaA, Source: core.SourceWebhook, PushedBy: "octocat",
			}},
			wantClaimed: true,
		},
		{
			name:       "push to another branch is still handed over",
			event:      "push",
			body:       pushBody("refs/heads/feature/x", shaA, false, "octocat", ""),
			wantStatus: "processed",
			wantPushes: []intake.Push{{
				Repository: "Example-User/blog", Branch: "feature/x", SHA: shaA, Source: core.SourceWebhook, PushedBy: "octocat",
			}},
			wantClaimed: true,
		},
		{
			name:       "pusher name when there is no sender",
			event:      "push",
			body:       pushBody("refs/heads/main", shaA, false, "", "octocat"),
			wantStatus: "processed",
			wantPushes: []intake.Push{{
				Repository: "Example-User/blog", Branch: "main", SHA: shaA, Source: core.SourceWebhook, PushedBy: "octocat",
			}},
			wantClaimed: true,
		},
		{
			name:       "branch deletion is ignored",
			event:      "push",
			body:       pushBody("refs/heads/main", strings.Repeat("0", 40), true, "octocat", ""),
			wantStatus: "ignored",
		},
		{
			name:       "deleted flag is ignored",
			event:      "push",
			body:       pushBody("refs/heads/main", shaA, true, "octocat", ""),
			wantStatus: "ignored",
		},
		{
			name:       "zero after SHA is ignored",
			event:      "push",
			body:       pushBody("refs/heads/main", strings.Repeat("0", 40), false, "octocat", ""),
			wantStatus: "ignored",
		},
		{
			name:       "tag push is ignored",
			event:      "push",
			body:       pushBody("refs/tags/v1.0.0", shaA, false, "octocat", ""),
			wantStatus: "ignored",
		},
		{
			name:       "bare refs/heads/ is ignored",
			event:      "push",
			body:       pushBody("refs/heads/", shaA, false, "octocat", ""),
			wantStatus: "ignored",
		},
		{
			name:              "check run",
			event:             "check_run",
			body:              `{"action":"completed","check_run":{"head_sha":"` + shaA + `","name":"test"},"repository":{"full_name":"example-user/blog"}}`,
			wantStatus:        "processed",
			wantReevaluations: []reevaluation{{repository: "example-user/blog", sha: shaA}},
		},
		{
			name:              "check run of any action",
			event:             "check_run",
			body:              `{"action":"rerequested","check_run":{"head_sha":"` + shaB + `"},"repository":{"full_name":"example-user/blog"}}`,
			wantStatus:        "processed",
			wantReevaluations: []reevaluation{{repository: "example-user/blog", sha: shaB}},
		},
		{
			name:              "check suite",
			event:             "check_suite",
			body:              `{"action":"completed","check_suite":{"head_sha":"` + shaA + `"},"repository":{"full_name":"example-user/blog"}}`,
			wantStatus:        "processed",
			wantReevaluations: []reevaluation{{repository: "example-user/blog", sha: shaA}},
		},
		{
			name:              "commit status",
			event:             "status",
			body:              `{"sha":"` + shaA + `","state":"success","context":"ci","repository":{"full_name":"example-user/blog"}}`,
			wantStatus:        "processed",
			wantReevaluations: []reevaluation{{repository: "example-user/blog", sha: shaA}},
		},
		{
			name:              "installation evicts the installation",
			event:             "installation",
			body:              `{"action":"suspend","installation":{"id":42}}`,
			wantStatus:        "processed",
			wantInstallations: []int64{42},
		},
		{
			name:              "installation repositories evicts removed repositories",
			event:             "installation_repositories",
			body:              `{"action":"removed","installation":{"id":42},"repositories_removed":[{"full_name":"example-user/blog"},{"full_name":"example-user/api"}],"repositories_added":[]}`,
			wantStatus:        "processed",
			wantInstallations: []int64{42},
			wantRepositories:  []string{"example-user/blog", "example-user/api"},
		},
		{
			name:              "installation repositories added",
			event:             "installation_repositories",
			body:              `{"action":"added","installation":{"id":7},"repositories_added":[{"full_name":"example-user/blog"}],"repositories_removed":[]}`,
			wantStatus:        "processed",
			wantInstallations: []int64{7},
		},
		{
			name:       "ping",
			event:      "ping",
			body:       `{"zen":"Keep it logically awesome.","hook_id":1}`,
			wantStatus: "processed",
		},
		{
			name:       "pull request is ignored",
			event:      "pull_request",
			body:       `{"action":"opened"}`,
			wantStatus: "ignored",
		},
		{
			name:       "unknown event with any body is ignored",
			event:      "workflow_run",
			body:       `not even json`,
			wantStatus: "ignored",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newWebhookHarness(t)
			code, body := h.send(t, delivery{event: tt.event, id: "delivery-1", body: tt.body})
			if code != http.StatusOK || body["status"] != tt.wantStatus {
				t.Fatalf("response = %d %v, want 200 status %s", code, body, tt.wantStatus)
			}
			pushes, reevaluations, _ := h.intake.recorded()
			if !slices.Equal(pushes, tt.wantPushes) {
				t.Errorf("pushes = %+v, want %+v", pushes, tt.wantPushes)
			}
			if !slices.Equal(reevaluations, tt.wantReevaluations) {
				t.Errorf("reevaluations = %+v, want %+v", reevaluations, tt.wantReevaluations)
			}
			claimed, _ := h.deliveries.state()
			if _, ok := claimed["delivery-1"]; ok != tt.wantClaimed {
				t.Errorf("delivery claimed = %t, want %t", ok, tt.wantClaimed)
			}
			if completed := h.deliveries.completedIDs(); (len(completed) == 1) != tt.wantClaimed {
				t.Errorf("completed deliveries = %v, want the claim completed: %t", completed, tt.wantClaimed)
			}
			installations, repositories := h.installations.evicted()
			if !slices.Equal(installations, tt.wantInstallations) || !slices.Equal(repositories, tt.wantRepositories) {
				t.Errorf("evicted %v %v, want %v %v", installations, repositories, tt.wantInstallations, tt.wantRepositories)
			}
			deliveries, _ := h.observer.recorded()
			if want := []string{tt.event + "/" + tt.wantStatus}; !slices.Equal(deliveries, want) {
				t.Errorf("observed %v, want %v", deliveries, want)
			}
		})
	}
}

func TestWebhookRejectsInvalidDeliveries(t *testing.T) {
	t.Parallel()

	validPush := pushBody("refs/heads/main", shaA, false, "octocat", "")
	tests := []struct {
		name       string
		delivery   delivery
		wantCode   int
		wantError  string
		wantResult intake.WebhookResult
	}{
		{
			name:       "missing signature",
			delivery:   delivery{event: "push", id: "d", body: validPush, unsigned: true},
			wantCode:   http.StatusUnauthorized,
			wantError:  "missing webhook signature",
			wantResult: intake.WebhookUnauthorized,
		},
		{
			name:       "signature with another secret",
			delivery:   delivery{event: "push", id: "d", body: validPush, signature: sign("wrong", []byte(validPush))},
			wantCode:   http.StatusUnauthorized,
			wantError:  "invalid webhook signature",
			wantResult: intake.WebhookUnauthorized,
		},
		{
			name:       "sha1 signature",
			delivery:   delivery{event: "push", id: "d", body: validPush, signature: "sha1=0123456789abcdef0123456789abcdef01234567"},
			wantCode:   http.StatusUnauthorized,
			wantError:  "invalid webhook signature",
			wantResult: intake.WebhookUnauthorized,
		},
		{
			name:       "signature is checked before the event header",
			delivery:   delivery{id: "d", body: validPush, unsigned: true},
			wantCode:   http.StatusUnauthorized,
			wantError:  "missing webhook signature",
			wantResult: intake.WebhookUnauthorized,
		},
		{
			name:       "signature is checked before parsing",
			delivery:   delivery{event: "push", id: "d", body: "{", signature: sign("wrong", []byte("{"))},
			wantCode:   http.StatusUnauthorized,
			wantError:  "invalid webhook signature",
			wantResult: intake.WebhookUnauthorized,
		},
		{
			name:       "missing event header",
			delivery:   delivery{id: "d", body: validPush},
			wantCode:   http.StatusBadRequest,
			wantError:  "missing X-GitHub-Event header",
			wantResult: intake.WebhookRejected,
		},
		{
			name:       "missing delivery header",
			delivery:   delivery{event: "push", body: validPush},
			wantCode:   http.StatusBadRequest,
			wantError:  "X-GitHub-Delivery",
			wantResult: intake.WebhookRejected,
		},
		{
			name:       "delivery id too long",
			delivery:   delivery{event: "push", id: strings.Repeat("d", 129), body: validPush},
			wantCode:   http.StatusBadRequest,
			wantError:  "X-GitHub-Delivery",
			wantResult: intake.WebhookRejected,
		},
		{
			name:       "malformed push",
			delivery:   delivery{event: "push", id: "d", body: `{"ref":`},
			wantCode:   http.StatusBadRequest,
			wantError:  "malformed payload",
			wantResult: intake.WebhookRejected,
		},
		{
			name:       "push without a repository",
			delivery:   delivery{event: "push", id: "d", body: `{"ref":"refs/heads/main","after":"` + shaA + `"}`},
			wantCode:   http.StatusBadRequest,
			wantError:  "malformed payload",
			wantResult: intake.WebhookRejected,
		},
		{
			name:       "push with a short SHA",
			delivery:   delivery{event: "push", id: "d", body: pushBody("refs/heads/main", "abc123", false, "octocat", "")},
			wantCode:   http.StatusBadRequest,
			wantError:  "malformed payload",
			wantResult: intake.WebhookRejected,
		},
		{
			name:       "check run without a SHA",
			delivery:   delivery{event: "check_run", id: "d", body: `{"check_run":{},"repository":{"full_name":"example-user/blog"}}`},
			wantCode:   http.StatusBadRequest,
			wantError:  "malformed payload",
			wantResult: intake.WebhookRejected,
		},
		{
			name:       "status with an invalid repository",
			delivery:   delivery{event: "status", id: "d", body: `{"sha":"` + shaA + `","repository":{"full_name":"blog"}}`},
			wantCode:   http.StatusBadRequest,
			wantError:  "malformed payload",
			wantResult: intake.WebhookRejected,
		},
		{
			name:       "installation without an id",
			delivery:   delivery{event: "installation", id: "d", body: `{"action":"deleted","installation":{}}`},
			wantCode:   http.StatusBadRequest,
			wantError:  "malformed payload",
			wantResult: intake.WebhookRejected,
		},
		{
			name:       "removed repository with an invalid name",
			delivery:   delivery{event: "installation_repositories", id: "d", body: `{"installation":{"id":1},"repositories_removed":[{"full_name":"x"}]}`},
			wantCode:   http.StatusBadRequest,
			wantError:  "malformed payload",
			wantResult: intake.WebhookRejected,
		},
		{
			name:       "oversized body",
			delivery:   delivery{event: "push", id: "d", body: `{"pad":"` + strings.Repeat("x", intake.MaxWebhookBytes) + `"}`},
			wantCode:   http.StatusRequestEntityTooLarge,
			wantError:  "payload too large",
			wantResult: intake.WebhookRejected,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newWebhookHarness(t)
			code, body := h.send(t, tt.delivery)
			if code != tt.wantCode || !strings.Contains(body["error"], tt.wantError) {
				t.Errorf("response = %d %v, want %d with error containing %q", code, body, tt.wantCode, tt.wantError)
			}
			pushes, reevaluations, _ := h.intake.recorded()
			if len(pushes)+len(reevaluations) != 0 {
				t.Errorf("intake called: %v %v", pushes, reevaluations)
			}
			if claimed, _ := h.deliveries.state(); len(claimed) != 0 {
				t.Errorf("claimed %v for a rejected delivery", claimed)
			}
			deliveries, _ := h.observer.recorded()
			if want := []string{tt.delivery.event + "/" + string(tt.wantResult)}; !slices.Equal(deliveries, want) {
				t.Errorf("observed %v, want %v", deliveries, want)
			}
		})
	}
}

func TestWebhookRejectsOversizedStreamedBody(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	body := strings.Repeat("x", intake.MaxWebhookBytes+1)
	r := delivery{event: "push", id: "d", body: body}.request(t.Context())
	r.ContentLength = -1
	r.Body = io.NopCloser(strings.NewReader(body))
	code, resp := serve(t, h.handler, r)
	if code != http.StatusRequestEntityTooLarge || resp["error"] != "payload too large" {
		t.Errorf("response = %d %v, want 413", code, resp)
	}
}

func TestWebhookAcceptsBodyAtTheLimit(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	prefix := `{"zen":"`
	body := prefix + strings.Repeat("x", intake.MaxWebhookBytes-len(prefix)-2) + `"}`
	if code, resp := h.send(t, delivery{event: "ping", id: "d", body: body}); code != http.StatusOK {
		t.Errorf("response = %d %v, want 200", code, resp)
	}
}

func TestWebhookDuplicateDeliveryIsNotHandledTwice(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	d := delivery{event: "push", id: "delivery-1", body: pushBody("refs/heads/main", shaA, false, "octocat", "")}
	if code, body := h.send(t, d); code != http.StatusOK || body["status"] != "processed" {
		t.Fatalf("first delivery = %d %v", code, body)
	}
	if code, body := h.send(t, d); code != http.StatusOK || body["status"] != "duplicate" {
		t.Fatalf("redelivery = %d %v, want 200 duplicate", code, body)
	}
	pushes, _, _ := h.intake.recorded()
	if len(pushes) != 1 {
		t.Errorf("pushes %v, want one", pushes)
	}
	deliveries, _ := h.observer.recorded()
	if want := []string{"push/processed", "push/duplicate"}; !slices.Equal(deliveries, want) {
		t.Errorf("observed %v, want %v", deliveries, want)
	}
	if want := []time.Duration{intake.DefaultWebhookTimeout + 10*time.Second, intake.DefaultWebhookTimeout + 10*time.Second}; !slices.Equal(h.deliveries.staleAfter, want) {
		t.Errorf("claims treat a claim as stranded after %v, want %v", h.deliveries.staleAfter, want)
	}
}

func TestWebhookCommitEventsAreNotDeduplicated(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	status := delivery{event: "status", id: "delivery-1", body: `{"sha":"` + shaA + `","repository":{"full_name":"example-user/blog"}}`}
	for range 2 {
		if code, body := h.send(t, status); code != http.StatusOK || body["status"] != "processed" {
			t.Fatalf("status delivery = %d %v, want processed", code, body)
		}
	}
	_, reevaluations, _ := h.intake.recorded()
	if want := slices.Repeat([]reevaluation{{repository: "example-user/blog", sha: shaA}}, 2); !slices.Equal(reevaluations, want) {
		t.Errorf("reevaluations = %v, want %v", reevaluations, want)
	}
	if claimed, _ := h.deliveries.state(); len(claimed) != 0 || len(h.deliveries.staleAfter) != 0 {
		t.Errorf("commit events claimed deliveries: %v", claimed)
	}
}

func TestWebhookDeliveryInProgressIsAConflict(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	h.deliveries.claimed["delivery-1"] = "push"
	code, body := h.send(t, delivery{event: "push", id: "delivery-1", body: pushBody("refs/heads/main", shaA, false, "octocat", "")})
	if code != http.StatusConflict || body["error"] != "delivery is already being handled" {
		t.Fatalf("response = %d %v, want 409", code, body)
	}
	if pushes, _, _ := h.intake.recorded(); len(pushes) != 0 {
		t.Errorf("pushes = %v, want none", pushes)
	}
	if _, released := h.deliveries.state(); len(released) != 0 {
		t.Errorf("released %v, want the other attempt's claim kept", released)
	}
	if deliveries, _ := h.observer.recorded(); !slices.Equal(deliveries, []string{"push/in_progress"}) {
		t.Errorf("observed %v, want push/in_progress", deliveries)
	}
}

func TestWebhookRecordingErrorsKeepTheOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func(*webhookHarness)
		wantCode   int
		wantResult string
	}{
		{
			name:       "completing fails after success",
			setup:      func(h *webhookHarness) { h.deliveries.completeErr = errBoom },
			wantCode:   http.StatusOK,
			wantResult: "push/processed",
		},
		{
			name: "releasing fails after failure",
			setup: func(h *webhookHarness) {
				h.deliveries.releaseErr = errBoom
				h.intake.failPushes(errBoom)
			},
			wantCode:   http.StatusInternalServerError,
			wantResult: "push/failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newWebhookHarness(t)
			tt.setup(h)
			code, _ := h.send(t, delivery{event: "push", id: "delivery-1", body: pushBody("refs/heads/main", shaA, false, "octocat", "")})
			if code != tt.wantCode {
				t.Errorf("response = %d, want %d", code, tt.wantCode)
			}
			if deliveries, _ := h.observer.recorded(); !slices.Equal(deliveries, []string{tt.wantResult}) {
				t.Errorf("observed %v, want %s", deliveries, tt.wantResult)
			}
		})
	}
}

func TestWebhookFailureReleasesTheClaim(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		event        string
		body         string
		fail         func(*fakeIntake)
		wantReleased []string
	}{
		{
			name:         "push",
			event:        "push",
			body:         pushBody("refs/heads/main", shaA, false, "octocat", ""),
			fail:         func(f *fakeIntake) { f.failPushes(errBoom) },
			wantReleased: []string{"delivery-1"},
		},
		{
			name:  "status has no claim to release",
			event: "status",
			body:  `{"sha":"` + shaA + `","repository":{"full_name":"example-user/blog"}}`,
			fail: func(f *fakeIntake) {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.reevaluateErr = errBoom
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newWebhookHarness(t)
			tt.fail(h.intake)
			d := delivery{event: tt.event, id: "delivery-1", body: tt.body}
			code, body := h.send(t, d)
			if code != http.StatusInternalServerError || body["error"] != "handling failed" {
				t.Fatalf("response = %d %v, want 500", code, body)
			}
			if strings.Contains(body["error"], "boom") {
				t.Errorf("response leaks the error: %v", body)
			}
			claimed, released := h.deliveries.state()
			if len(claimed) != 0 || !slices.Equal(released, tt.wantReleased) {
				t.Errorf("claimed %v released %v, want released %v", claimed, released, tt.wantReleased)
			}

			h.intake.mu.Lock()
			h.intake.pushErr, h.intake.reevaluateErr = nil, nil
			h.intake.mu.Unlock()
			if code, body := h.send(t, d); code != http.StatusOK || body["status"] != "processed" {
				t.Errorf("redelivery after failure = %d %v, want processed", code, body)
			}
			deliveries, _ := h.observer.recorded()
			if want := []string{tt.event + "/failed", tt.event + "/processed"}; !slices.Equal(deliveries, want) {
				t.Errorf("observed %v, want %v", deliveries, want)
			}
		})
	}
}

func TestWebhookClaimErrorFails(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	h.deliveries.claimErr = errBoom
	code, body := h.send(t, delivery{event: "push", id: "d", body: pushBody("refs/heads/main", shaA, false, "octocat", "")})
	if code != http.StatusInternalServerError || body["error"] != "could not record delivery" {
		t.Errorf("response = %d %v, want 500", code, body)
	}
	if pushes, _, _ := h.intake.recorded(); len(pushes) != 0 {
		t.Errorf("pushes = %v, want none", pushes)
	}
}

func TestWebhookClientDisconnectDoesNotCancelHandling(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := delivery{event: "push", id: "d", body: pushBody("refs/heads/main", shaA, false, "octocat", "")}.request(ctx)
	code, body := serve(t, h.handler, r)
	if code != http.StatusOK || body["status"] != "processed" {
		t.Fatalf("response = %d %v, want processed", code, body)
	}
	pushes, _, ctxErrs := h.intake.recorded()
	if len(pushes) != 1 || len(ctxErrs) != 1 || ctxErrs[0] != nil {
		t.Errorf("pushes %v with context errors %v, want one push on a live context", pushes, ctxErrs)
	}
}

func TestWebhookPatternAllowsOnlyPost(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	mux := http.NewServeMux()
	mux.Handle(intake.WebhookPattern, h.handler)
	tests := []struct {
		method string
		want   int
	}{
		{method: http.MethodGet, want: http.StatusMethodNotAllowed},
		{method: http.MethodPut, want: http.StatusMethodNotAllowed},
		{method: http.MethodPost, want: http.StatusOK},
	}
	for _, tt := range tests {
		r := delivery{event: "ping", id: "d", body: `{}`}.request(t.Context())
		r.Method = tt.method
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tt.want {
			t.Errorf("%s = %d, want %d", tt.method, w.Code, tt.want)
		}
	}
}

func TestNewWebhookValidatesOptions(t *testing.T) {
	t.Parallel()

	valid := func() intake.WebhookOptions {
		return intake.WebhookOptions{
			Secret:        []byte("s"),
			Intake:        &fakeIntake{},
			Deliveries:    newFakeDeliveries(),
			Installations: &fakeInstallations{},
		}
	}
	tests := []struct {
		name    string
		mutate  func(*intake.WebhookOptions)
		wantErr string
	}{
		{name: "valid", mutate: func(*intake.WebhookOptions) {}},
		{name: "empty secret", mutate: func(o *intake.WebhookOptions) { o.Secret = nil }, wantErr: "webhook secret is required"},
		{name: "no intake", mutate: func(o *intake.WebhookOptions) { o.Intake = nil }, wantErr: "intake is required"},
		{name: "no deliveries", mutate: func(o *intake.WebhookOptions) { o.Deliveries = nil }, wantErr: "deliveries store is required"},
		{name: "no installation cache", mutate: func(o *intake.WebhookOptions) { o.Installations = nil }, wantErr: "installation cache is required"},
		{name: "negative timeout", mutate: func(o *intake.WebhookOptions) { o.Timeout = -1 }, wantErr: "timeout must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := valid()
			tt.mutate(&opts)
			_, err := intake.NewWebhook(opts)
			if (err == nil) != (tt.wantErr == "") || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("NewWebhook error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
