package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	maxReasonRunes  = 500
	maxMessageRunes = 1000

	colorInfo     = "#1D9BD1"
	colorSuccess  = "#2EB67D"
	colorWarning  = "#ECB22E"
	colorFailure  = "#E01E5A"
	colorCritical = "#8B0000"
	colorNeutral  = "#9E9E9E"
	colorLock     = "#6F42C1"
)

type SlackOptions struct {
	Name       string
	URL        func() (string, error)
	Kinds      []core.NotificationKind
	Links      Links
	HTTPClient *http.Client
	UserAgent  string
	Now        func() time.Time
}

type Slack struct {
	name      string
	url       func() (string, error)
	kinds     kindSet
	links     Links
	sender    httpSender
	userAgent string
}

func NewSlack(opts SlackOptions) (*Slack, error) {
	if opts.Name == "" {
		return nil, errors.New("notify: slack sink needs a name")
	}
	if opts.URL == nil {
		return nil, fmt.Errorf("notify: slack sink %s needs a webhook URL source", opts.Name)
	}
	kinds, err := newKindSet(opts.Kinds)
	if err != nil {
		return nil, fmt.Errorf("notify: slack sink %s: %w", opts.Name, err)
	}
	return &Slack{
		name:      opts.Name,
		url:       opts.URL,
		kinds:     kinds,
		links:     opts.Links,
		sender:    newHTTPSender(opts.HTTPClient, opts.Now),
		userAgent: userAgentOrDefault(opts.UserAgent),
	}, nil
}

func (s *Slack) Name() string { return s.name }

func (s *Slack) Type() string { return TypeSlack }

func (s *Slack) Accepts(n core.Notification) bool { return s.kinds[n.Kind] }

func (s *Slack) Prepare(n core.Notification) Delivery {
	body, err := encodeJSON(slackMessageFor(n, s.links))
	if err != nil {
		return failedDelivery(permanent(fmt.Errorf("encode slack message: %w", err)))
	}
	header := http.Header{
		"Content-Type": {"application/json"},
		"User-Agent":   {s.userAgent},
	}
	return func(ctx context.Context) error {
		endpoint, err := s.url()
		if err != nil {
			return fmt.Errorf("read slack webhook URL: %w", err)
		}
		return s.sender.post(ctx, endpoint, header, body)
	}
}

func failedDelivery(err error) Delivery {
	return func(context.Context) error { return err }
}

func userAgentOrDefault(ua string) string {
	if ua == "" {
		return "doorstep"
	}
	return ua
}

type slackMessage struct {
	Text        string            `json:"text"`
	Attachments []slackAttachment `json:"attachments"`
}

type slackAttachment struct {
	Color    string       `json:"color"`
	Fallback string       `json:"fallback"`
	Blocks   []slackBlock `json:"blocks"`
}

type slackBlock struct {
	Type     string      `json:"type"`
	Text     *slackText  `json:"text,omitempty"`
	Fields   []slackText `json:"fields,omitempty"`
	Elements []slackText `json:"elements,omitempty"`
}

type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func mrkdwn(text string) slackText {
	return slackText{Type: "mrkdwn", Text: text}
}

func slackMessageFor(n core.Notification, links Links) slackMessage {
	plain := summary(n)
	blocks := []slackBlock{{Type: "section", Text: new(mrkdwn(slackHeadline(n, links)))}}
	if fields := slackFields(n, links); len(fields) > 0 {
		blocks = append(blocks, slackBlock{Type: "section", Fields: fields})
	}
	if url := links.deployment(n.Deployment); url != "" {
		blocks = append(blocks, slackBlock{Type: "context", Elements: []slackText{mrkdwn(slackLink(url, "View deployment"))}})
	}
	return slackMessage{
		Text:        slackEscape(plain),
		Attachments: []slackAttachment{{Color: slackColor(n), Fallback: plain, Blocks: blocks}},
	}
}

func slackHeadline(n core.Notification, links Links) string {
	lines := []string{"*" + slackEscape(describeTarget(n.Target)) + "*: " + slackEscape(outcome(n))}
	if id, ok := rollbackOfID(n); ok {
		original := slackDeploymentLink(n.RollbackOf, id, links)
		if n.Kind == core.NotificationDeployRolledBack {
			lines = append(lines, slackEscape(upperFirst(deploymentNoun(n.Deployment)))+" re-pinned the image that ran before "+original)
		} else {
			lines = append(lines, "Rollback of "+original)
		}
	}
	if n.Message != "" {
		lines = append(lines, slackEscape(truncateText(n.Message, maxMessageRunes)))
	}
	return strings.Join(lines, "\n")
}

func slackDeploymentLink(d *core.Deployment, id core.DeploymentID, links Links) string {
	text := "deploy " + deploymentRef(id)
	if d == nil {
		d = &core.Deployment{ID: id}
	} else {
		text = deploymentNoun(d)
	}
	if url := links.deployment(d); url != "" {
		return slackLink(url, text)
	}
	return slackEscape(text)
}

func slackFields(n core.Notification, links Links) []slackText {
	if n.Lock != nil {
		return slackLockFields(n)
	}
	d := n.Deployment
	var fields []slackText
	if d.Commit.SHA != "" {
		commit := slackEscape(d.Commit.Repository + "@" + shortSHA(d.Commit.SHA))
		if url := links.commit(d.Commit); url != "" {
			commit = slackLink(url, d.Commit.Repository+"@"+shortSHA(d.Commit.SHA))
		}
		fields = append(fields, slackField("Commit", commit))
	}
	if d.Digest != "" {
		fields = append(fields, slackField("Image", "`"+slackEscape(shortDigest(d.Digest))+"`"))
	}
	if requester := firstNonEmpty(d.RequestedBy, n.Actor); requester != "" {
		fields = append(fields, slackField("Requested by", slackEscape(requester)))
	}
	if duration, ok := deploymentDuration(d); ok {
		fields = append(fields, slackField("Duration", formatDuration(duration)))
	}
	if d.FailureReason != "" {
		fields = append(fields, slackField("Reason", slackEscape(truncateText(d.FailureReason, maxReasonRunes))))
	}
	return fields
}

func slackLockFields(n core.Notification) []slackText {
	l := n.Lock
	fields := []slackText{slackField("Owner", slackEscape(lockOwner(l)))}
	if l.Reason != "" {
		fields = append(fields, slackField("Reason", slackEscape(truncateText(l.Reason, maxReasonRunes))))
	}
	if !l.CreatedAt.IsZero() && !n.OccurredAt.Before(l.CreatedAt) && n.LockChange != core.LockReleased {
		fields = append(fields, slackField("Held for", formatDuration(n.OccurredAt.Sub(l.CreatedAt))))
	}
	if l.ExpiresAt != nil && n.LockChange != core.LockReleased {
		fields = append(fields, slackField("Expires", formatTime(*l.ExpiresAt)))
	}
	return fields
}

func slackField(label, value string) slackText {
	return mrkdwn("*" + label + "*\n" + value)
}

func slackColor(n core.Notification) string {
	switch n.Kind {
	case core.NotificationDeployQueued, core.NotificationDeployStarted:
		return colorInfo
	case core.NotificationDeploySucceeded:
		return colorSuccess
	case core.NotificationDeployFailed:
		if isRollbackFailure(n) {
			return colorCritical
		}
		return colorFailure
	case core.NotificationDeployRolledBack:
		return colorWarning
	case core.NotificationDeployCancelled, core.NotificationDeploySuperseded:
		return colorNeutral
	case core.NotificationLockChanged:
		if n.LockChange == core.LockReleased {
			return colorNeutral
		}
		return colorLock
	case core.NotificationLockReminder:
		return colorLock
	}
	return colorNeutral
}

func slackLink(url, text string) string {
	return "<" + slackEscape(url) + "|" + strings.ReplaceAll(slackEscape(text), "|", "¦") + ">"
}

func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
