package notify

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	PayloadVersion = 1

	HeaderEvent     = "X-Doorstep-Event"
	HeaderDelivery  = "X-Doorstep-Delivery"
	HeaderSignature = "X-Doorstep-Signature-256"

	signaturePrefix = "sha256="
)

type WebhookOptions struct {
	Name       string
	URL        func() (string, error)
	Secret     func() (string, error)
	Kinds      []core.NotificationKind
	Links      Links
	HTTPClient *http.Client
	UserAgent  string
	NewID      func() string
	Now        func() time.Time
}

type Webhook struct {
	name      string
	url       func() (string, error)
	secret    func() (string, error)
	kinds     kindSet
	links     Links
	sender    httpSender
	userAgent string
	newID     func() string
}

func NewWebhook(opts WebhookOptions) (*Webhook, error) {
	if opts.Name == "" {
		return nil, errors.New("notify: webhook sink needs a name")
	}
	if opts.URL == nil {
		return nil, fmt.Errorf("notify: webhook sink %s needs a URL source", opts.Name)
	}
	kinds, err := newKindSet(opts.Kinds)
	if err != nil {
		return nil, fmt.Errorf("notify: webhook sink %s: %w", opts.Name, err)
	}
	w := &Webhook{
		name:      opts.Name,
		url:       opts.URL,
		secret:    opts.Secret,
		kinds:     kinds,
		links:     opts.Links,
		sender:    newHTTPSender(opts.HTTPClient, opts.Now),
		userAgent: userAgentOrDefault(opts.UserAgent),
		newID:     opts.NewID,
	}
	if w.newID == nil {
		w.newID = newDeliveryID
	}
	return w, nil
}

func (w *Webhook) Name() string { return w.name }

func (w *Webhook) Type() string { return TypeWebhook }

func (w *Webhook) Accepts(n core.Notification) bool { return w.kinds[n.Kind] }

func (w *Webhook) Prepare(n core.Notification) Delivery {
	id := w.newID()
	body, err := encodeJSON(webhookPayloadFor(n, id, w.links))
	if err != nil {
		return failedDelivery(permanent(fmt.Errorf("encode webhook payload: %w", err)))
	}
	return func(ctx context.Context) error {
		endpoint, err := w.url()
		if err != nil {
			return fmt.Errorf("read webhook URL: %w", err)
		}
		header := http.Header{
			"Content-Type": {"application/json"},
			"User-Agent":   {w.userAgent},
			HeaderEvent:    {n.Kind.String()},
			HeaderDelivery: {id},
		}
		if w.secret != nil {
			secret, err := w.secret()
			if err != nil {
				return fmt.Errorf("read webhook secret: %w", err)
			}
			header.Set(HeaderSignature, Sign([]byte(secret), body))
		}
		return w.sender.post(ctx, endpoint, header, body)
	}
}

func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

func newDeliveryID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type webhookPayload struct {
	Version     int                `json:"version"`
	ID          string             `json:"id"`
	Type        string             `json:"type"`
	OccurredAt  time.Time          `json:"occurred_at"`
	App         string             `json:"app"`
	Environment string             `json:"environment"`
	Actor       string             `json:"actor,omitempty"`
	Message     string             `json:"message,omitempty"`
	Deployment  *webhookDeployment `json:"deployment,omitempty"`
	Lock        *webhookLock       `json:"lock,omitempty"`
}

type webhookDeployment struct {
	ID             int64         `json:"id"`
	Kind           string        `json:"kind"`
	State          string        `json:"state"`
	Source         string        `json:"source,omitempty"`
	RequestedBy    string        `json:"requested_by,omitempty"`
	Commit         webhookCommit `json:"commit"`
	Image          string        `json:"image,omitempty"`
	Digest         string        `json:"digest,omitempty"`
	PreviousDigest string        `json:"previous_digest,omitempty"`
	PinCommitSHA   string        `json:"pin_commit_sha,omitempty"`
	FailureReason  string        `json:"failure_reason,omitempty"`
	RollbackOfID   *int64        `json:"rollback_of_id,omitempty"`
	StartedAt      *time.Time    `json:"started_at,omitempty"`
	FinishedAt     *time.Time    `json:"finished_at,omitempty"`
	URL            string        `json:"url,omitempty"`
}

type webhookCommit struct {
	Repository string `json:"repository"`
	SHA        string `json:"sha"`
}

type webhookLock struct {
	Kind      string     `json:"kind"`
	Owner     string     `json:"owner"`
	Reason    string     `json:"reason,omitempty"`
	Change    string     `json:"change,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func webhookPayloadFor(n core.Notification, id string, links Links) webhookPayload {
	p := webhookPayload{
		Version:     PayloadVersion,
		ID:          id,
		Type:        n.Kind.String(),
		OccurredAt:  n.OccurredAt.UTC(),
		App:         n.Target.App,
		Environment: n.Target.Environment,
		Actor:       n.Actor,
		Message:     n.Message,
	}
	if d := n.Deployment; d != nil {
		wd := &webhookDeployment{
			ID:             int64(d.ID),
			Kind:           string(d.Kind),
			State:          string(d.State),
			Source:         string(d.Source),
			RequestedBy:    d.RequestedBy,
			Commit:         webhookCommit{Repository: d.Commit.Repository, SHA: d.Commit.SHA},
			Digest:         string(d.Digest),
			PreviousDigest: string(d.PreviousDigest),
			PinCommitSHA:   d.PinCommitSHA,
			FailureReason:  d.FailureReason,
			StartedAt:      utc(d.StartedAt),
			FinishedAt:     utc(d.FinishedAt),
			URL:            links.deployment(d),
		}
		if d.Image.Repository != "" {
			wd.Image = d.Image.String()
		}
		if rollbackOf, ok := rollbackOfID(n); ok {
			wd.RollbackOfID = new(int64(rollbackOf))
		}
		p.Deployment = wd
	}
	if l := n.Lock; l != nil {
		p.Lock = &webhookLock{
			Kind:      string(l.Kind),
			Owner:     l.Owner,
			Reason:    l.Reason,
			Change:    string(n.LockChange),
			CreatedAt: l.CreatedAt.UTC(),
			ExpiresAt: utc(l.ExpiresAt),
		}
	}
	return p
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	return new(t.UTC())
}
