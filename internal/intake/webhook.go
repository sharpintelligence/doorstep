package intake

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const (
	WebhookPattern        = "POST /webhooks/github"
	MaxWebhookBytes       = 5 << 20
	DefaultWebhookTimeout = 25 * time.Second

	recordTimeout = 5 * time.Second
)

type Pusher interface {
	HandlePush(ctx context.Context, p Push) ([]Result, error)
	Reevaluate(ctx context.Context, repository, sha string) ([]Result, error)
}

type Deliveries interface {
	Claim(ctx context.Context, id, event string, staleAfter time.Duration) (store.DeliveryClaim, error)
	Complete(ctx context.Context, id string) error
	Release(ctx context.Context, id string) error
}

type InstallationCache interface {
	EvictInstallation(id int64)
	EvictRepository(owner, repo string) error
}

type WebhookOptions struct {
	Secret        []byte
	Intake        Pusher
	Deliveries    Deliveries
	Installations InstallationCache
	Observer      Observer
	Logger        *slog.Logger
	Timeout       time.Duration
}

type WebhookHandler struct {
	secret        []byte
	intake        Pusher
	deliveries    Deliveries
	installations InstallationCache
	observer      Observer
	logger        *slog.Logger
	timeout       time.Duration
}

func NewWebhook(opts WebhookOptions) (*WebhookHandler, error) {
	switch {
	case len(opts.Secret) == 0:
		return nil, errors.New("intake: webhook secret is required")
	case opts.Intake == nil:
		return nil, errors.New("intake: webhook intake is required")
	case opts.Deliveries == nil:
		return nil, errors.New("intake: webhook deliveries store is required")
	case opts.Installations == nil:
		return nil, errors.New("intake: webhook installation cache is required")
	case opts.Timeout < 0:
		return nil, errors.New("intake: webhook timeout must not be negative")
	}
	h := &WebhookHandler{
		secret:        append([]byte(nil), opts.Secret...),
		intake:        opts.Intake,
		deliveries:    opts.Deliveries,
		installations: opts.Installations,
		observer:      opts.Observer,
		logger:        opts.Logger,
		timeout:       opts.Timeout,
	}
	if h.observer == nil {
		h.observer = nopObserver{}
	}
	if h.logger == nil {
		h.logger = slog.New(slog.DiscardHandler)
	}
	if h.timeout == 0 {
		h.timeout = DefaultWebhookTimeout
	}
	return h, nil
}

type delivery struct {
	event string
	id    string
	log   *slog.Logger
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get(github.EventHeader)
	if r.ContentLength > MaxWebhookBytes {
		h.reject(w, r, event, http.StatusRequestEntityTooLarge, WebhookRejected, "payload too large")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxWebhookBytes))
	if err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			h.reject(w, r, event, http.StatusRequestEntityTooLarge, WebhookRejected, "payload too large")
			return
		}
		h.reject(w, r, event, http.StatusBadRequest, WebhookRejected, "could not read payload")
		return
	}
	if err := github.VerifySignature(h.secret, body, r.Header.Get(github.SignatureHeader)); err != nil {
		h.reject(w, r, event, http.StatusUnauthorized, WebhookUnauthorized, err.Error())
		return
	}
	id := r.Header.Get(github.DeliveryHeader)
	switch {
	case event == "":
		h.reject(w, r, event, http.StatusBadRequest, WebhookRejected, "missing "+github.EventHeader+" header")
		return
	case id == "" || len(id) > store.MaxDeliveryIDLength:
		h.reject(w, r, event, http.StatusBadRequest, WebhookRejected, "missing or invalid "+github.DeliveryHeader+" header")
		return
	}
	d := delivery{event: event, id: id, log: h.logger.With(slog.String("event", event), slog.String("delivery", id))}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), h.timeout)
	defer cancel()
	status, result, message := h.dispatch(ctx, d, body)
	h.observer.WebhookDelivery(event, result)
	if status != http.StatusOK {
		writeJSON(w, status, map[string]string{"error": message})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": string(result)})
}

func (h *WebhookHandler) dispatch(ctx context.Context, d delivery, body []byte) (int, WebhookResult, string) {
	switch d.event {
	case EventPing:
		d.log.InfoContext(ctx, "github webhook ping received")
		return http.StatusOK, WebhookProcessed, ""
	case EventPush:
		push, ok, err := parsePush(body)
		if err != nil {
			return h.malformed(ctx, d, err)
		}
		if !ok {
			d.log.DebugContext(ctx, "ignoring push that is not a branch update")
			return http.StatusOK, WebhookIgnored, ""
		}
		log := d.log.With(slog.String("repository", push.Repository), slog.String("branch", push.Branch), slog.String("sha", push.SHA))
		return h.claimAndRun(ctx, d, log, func(ctx context.Context) ([]Result, error) {
			return h.intake.HandlePush(ctx, push)
		})
	case EventCheckRun, EventCheckSuite, EventStatus:
		ref, err := parseCommitEvent(d.event, body)
		if err != nil {
			return h.malformed(ctx, d, err)
		}
		log := d.log.With(slog.String("repository", ref.Repository), slog.String("sha", ref.SHA))
		return h.run(ctx, log, func(ctx context.Context) ([]Result, error) {
			return h.intake.Reevaluate(ctx, ref.Repository, ref.SHA)
		})
	case EventInstallation, EventInstallationRepositories:
		change, err := parseInstallation(d.event, body)
		if err != nil {
			return h.malformed(ctx, d, err)
		}
		h.evict(ctx, d, change)
		return http.StatusOK, WebhookProcessed, ""
	default:
		d.log.DebugContext(ctx, "ignoring unhandled github webhook event")
		return http.StatusOK, WebhookIgnored, ""
	}
}

func (h *WebhookHandler) claimAndRun(ctx context.Context, d delivery, log *slog.Logger, work func(context.Context) ([]Result, error)) (int, WebhookResult, string) {
	claim, err := h.deliveries.Claim(ctx, d.id, d.event, h.staleClaim())
	if err != nil {
		log.ErrorContext(ctx, "recording github webhook delivery failed", slog.Any("error", err))
		return http.StatusInternalServerError, WebhookFailed, "could not record delivery"
	}
	switch claim {
	case store.DeliveryCompleted:
		log.InfoContext(ctx, "ignoring duplicate github webhook delivery")
		return http.StatusOK, WebhookDuplicate, ""
	case store.DeliveryInProgress:
		log.InfoContext(ctx, "github webhook delivery is already being handled")
		return http.StatusConflict, WebhookInProgress, "delivery is already being handled"
	case store.DeliveryClaimed:
	}
	status, result, message := h.run(ctx, log, work)
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if result == WebhookFailed {
		if err := h.deliveries.Release(recordCtx, d.id); err != nil {
			log.ErrorContext(ctx, "releasing github webhook delivery failed", slog.Any("error", err))
		}
		return status, result, message
	}
	if err := h.deliveries.Complete(recordCtx, d.id); err != nil {
		log.WarnContext(ctx, "recording github webhook delivery as completed failed", slog.Any("error", err))
	}
	return status, result, message
}

func (h *WebhookHandler) staleClaim() time.Duration {
	return h.timeout + 2*recordTimeout
}

func (h *WebhookHandler) run(ctx context.Context, log *slog.Logger, work func(context.Context) ([]Result, error)) (int, WebhookResult, string) {
	results, err := work(ctx)
	if err != nil {
		log.WarnContext(ctx, "handling github webhook failed", slog.Any("error", err))
		return http.StatusInternalServerError, WebhookFailed, "handling failed"
	}
	enqueued := 0
	for _, r := range results {
		if r.Enqueued {
			enqueued++
		}
	}
	log.InfoContext(ctx, "github webhook handled", slog.Int("tracked", len(results)), slog.Int("enqueued", enqueued))
	return http.StatusOK, WebhookProcessed, ""
}

func (h *WebhookHandler) evict(ctx context.Context, d delivery, change installationChange) {
	h.installations.EvictInstallation(change.ID)
	for _, repository := range change.Removed {
		if err := h.installations.EvictRepository(repository.Owner, repository.Name); err != nil {
			d.log.WarnContext(ctx, "evicting repository from the installation cache failed", slog.String("repository", repository.FullName), slog.Any("error", err))
		}
	}
	d.log.InfoContext(ctx, "github installation changed, cache evicted",
		slog.Int64("installation_id", change.ID),
		slog.Int("repositories_removed", len(change.Removed)),
	)
}

func (h *WebhookHandler) malformed(ctx context.Context, d delivery, err error) (int, WebhookResult, string) {
	d.log.WarnContext(ctx, "rejecting malformed github webhook payload", slog.Any("error", err))
	return http.StatusBadRequest, WebhookRejected, "malformed payload"
}

func (h *WebhookHandler) reject(w http.ResponseWriter, r *http.Request, event string, status int, result WebhookResult, message string) {
	h.logger.WarnContext(r.Context(), "rejecting github webhook", slog.Int("status", status), slog.String("reason", message))
	h.observer.WebhookDelivery(event, result)
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
