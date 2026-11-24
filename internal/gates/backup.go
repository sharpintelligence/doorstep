package gates

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

const (
	BackupRequestTimeout = 20 * time.Second
	BackupPollInterval   = 15 * time.Second
	BackupEvent          = "backup.requested"

	HeaderBackupEvent     = "X-Doorstep-Event"
	HeaderBackupSignature = "X-Doorstep-Signature-256"
	HeaderIdempotencyKey  = "Idempotency-Key"

	backupEventName          = "backup"
	maxBackupResponseBytes   = 4 << 10
	maxBackupReferenceLength = 200
	notFlaggedAsMigration    = "release is not flagged as a database migration"
)

const (
	backupRunning   = "running"
	backupSucceeded = "succeeded"
	backupFailed    = "failed"
)

type BackupDeps struct {
	Client     *http.Client
	ReadSecret func(config.SecretRef) (string, error)
	Sign       func(secret, body []byte) string
	Now        func() time.Time
	UserAgent  string
}

func RegisterBackup(r *Registry, deps BackupDeps) error {
	if r == nil {
		return errors.New("register backup gate: no registry")
	}
	var errs []error
	if deps.Client == nil {
		errs = append(errs, errors.New("an HTTP client is required"))
	}
	if deps.ReadSecret == nil {
		errs = append(errs, errors.New("a secret reader is required"))
	}
	if deps.Sign == nil {
		errs = append(errs, errors.New("a signer is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("register backup gate: %w", err)
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.UserAgent == "" {
		deps.UserAgent = "doorstep"
	}
	err := r.Register(config.GateBackup, Definition{Kind: metrics.GateKindBackup, Deferred: true, New: deps.newBackup})
	if err != nil {
		return fmt.Errorf("register backup gate: %w", err)
	}
	return nil
}

type backupGate struct {
	deps             BackupDeps
	name             string
	when             config.GateWhen
	applyToRollbacks bool
	endpoint         config.SecretRef
	secret           config.SecretRef
}

func (d BackupDeps) newBackup(app config.App, gate config.Gate) (Gate, error) {
	endpoint, ok := gate.WebhookEndpoint(app.Name)
	if !ok {
		return nil, fmt.Errorf("backup gate %s has no webhook", gate.Name)
	}
	g := backupGate{
		deps:             d,
		name:             gate.Name,
		when:             gate.When,
		applyToRollbacks: gate.ApplyToRollbacks,
		endpoint:         endpoint,
	}
	switch g.when {
	case config.GateWhenMigration, config.GateWhenAlways:
	case "":
		g.when = config.GateWhenMigration
	default:
		return nil, fmt.Errorf("backup gate %s: unknown when %q", gate.Name, gate.When)
	}
	secret, ok := gate.WebhookSecret(app.Name)
	if !ok {
		return nil, fmt.Errorf("backup gate %s has no webhook secret", gate.Name)
	}
	g.secret = secret
	return g, nil
}

type backupEvidence struct {
	RequestedAt   string `json:"requested_at"`
	LastRequestAt string `json:"last_request_at"`
	Requests      int    `json:"requests"`
	StatusCode    int    `json:"status_code"`
	State         string `json:"state,omitempty"`
	Reference     string `json:"reference,omitempty"`
}

type backupRequest struct {
	Event       string           `json:"event"`
	Gate        string           `json:"gate"`
	RequestedAt string           `json:"requested_at"`
	Deployment  backupDeployment `json:"deployment"`
}

type backupDeployment struct {
	ID              core.DeploymentID `json:"id"`
	App             string            `json:"app"`
	Environment     string            `json:"environment"`
	Kind            core.Kind         `json:"kind"`
	Commit          backupCommit      `json:"commit"`
	Image           backupImage       `json:"image"`
	Migration       bool              `json:"migration"`
	MigrationReason string            `json:"migration_reason,omitempty"`
}

type backupCommit struct {
	Repository string `json:"repository"`
	SHA        string `json:"sha"`
}

type backupImage struct {
	Repository string      `json:"repository"`
	Tag        string      `json:"tag"`
	Digest     core.Digest `json:"digest,omitempty"`
}

type backupResponse struct {
	Status    string `json:"status"`
	Message   string `json:"message"`
	Reference string `json:"reference"`
}

func (g backupGate) Evaluate(ctx context.Context, req Request) (Evaluation, error) {
	if err := ctx.Err(); err != nil {
		return Evaluation{}, err
	}
	d := req.Deployment
	if d.Kind == core.KindRollback && !g.applyToRollbacks {
		return Evaluation{Status: core.GateSkipped, Message: skippedForRollback}, nil
	}
	if g.when == config.GateWhenMigration && !d.Migration {
		return Evaluation{Status: core.GateSkipped, Message: notFlaggedAsMigration}, nil
	}

	now := g.deps.Now()
	ev, pending := previousBackup(req.Previous)
	if pending {
		if last, ok := parseTimestamp(ev.LastRequestAt); ok && now.Sub(last) < BackupPollInterval {
			return Evaluation{Status: core.GatePending, Message: req.Previous.Message, Evidence: req.Previous.Evidence}, nil
		}
	}
	if _, ok := parseTimestamp(ev.RequestedAt); !ok {
		ev.RequestedAt = timestamp(now)
	}

	body, err := json.Marshal(backupRequest{
		Event:       BackupEvent,
		Gate:        g.name,
		RequestedAt: ev.RequestedAt,
		Deployment: backupDeployment{
			ID:              d.ID,
			App:             d.Target.App,
			Environment:     d.Target.Environment,
			Kind:            d.Kind,
			Commit:          backupCommit{Repository: d.Commit.Repository, SHA: d.Commit.SHA},
			Image:           backupImage{Repository: d.Image.Repository, Tag: d.Image.Tag, Digest: d.Digest},
			Migration:       d.Migration,
			MigrationReason: d.MigrationReason,
		},
	})
	if err != nil {
		return Evaluation{}, fmt.Errorf("encode backup request: %w", err)
	}
	endpoint, err := g.deps.ReadSecret(g.endpoint)
	if err != nil {
		return Evaluation{}, fmt.Errorf("read backup webhook URL: %w", err)
	}
	target, err := url.Parse(endpoint)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return Evaluation{}, errors.New("backup webhook URL must be an absolute http or https URL")
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("User-Agent", g.deps.UserAgent)
	header.Set(HeaderBackupEvent, backupEventName)
	header.Set(HeaderIdempotencyKey, g.idempotencyKey(d.ID, ev.RequestedAt))
	secret, err := g.deps.ReadSecret(g.secret)
	if err != nil {
		return Evaluation{}, fmt.Errorf("read backup webhook secret: %w", err)
	}
	header.Set(HeaderBackupSignature, g.deps.Sign([]byte(secret), body))

	status, answer, postErr := g.post(ctx, target.String(), header, body)
	if err := ctx.Err(); err != nil {
		return Evaluation{}, err
	}
	ev.LastRequestAt = timestamp(now)
	ev.Requests++
	ev.StatusCode = status
	ev.State = answer.Status
	if answer.Reference != "" {
		ev.Reference = truncateRunes(answer.Reference, maxBackupReferenceLength)
	}
	evidence, err := json.Marshal(ev)
	if err != nil {
		return Evaluation{}, fmt.Errorf("encode backup evidence: %w", err)
	}
	verdict, message := backupVerdict(status, answer)
	if postErr != nil {
		verdict, message = core.GatePending, postErr.Error()+"; retrying"
	}
	return Evaluation{Status: verdict, Message: clean(message), Evidence: evidence}, nil
}

func (g backupGate) idempotencyKey(id core.DeploymentID, requestedAt string) string {
	return "doorstep-backup-" + strconv.FormatInt(int64(id), 10) + "-" + g.name + "-" + requestedAt
}

func (g backupGate) post(ctx context.Context, target string, header http.Header, body []byte) (int, backupResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, BackupRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, backupResponse{}, errors.New("build backup webhook request: invalid URL")
	}
	req.Header = header
	resp, err := g.deps.Client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, backupResponse{}, fmt.Errorf("backup webhook did not answer within %s", BackupRequestTimeout)
		}
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err
		}
		return 0, backupResponse{}, fmt.Errorf("post backup webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxBackupResponseBytes))
	var answer backupResponse
	if json.Unmarshal(data, &answer) != nil {
		answer = backupResponse{Message: strings.Join(strings.Fields(strings.ToValidUTF8(string(data), "")), " ")}
	}
	answer.Status = strings.ToLower(strings.TrimSpace(answer.Status))
	switch answer.Status {
	case backupRunning, backupSucceeded, backupFailed:
	default:
		answer.Status = ""
	}
	return resp.StatusCode, answer, nil
}

func backupVerdict(status int, answer backupResponse) (core.GateStatus, string) {
	detail := answer.Message
	if answer.Reference != "" {
		detail = strings.TrimSpace(detail + " (" + truncateRunes(answer.Reference, maxBackupReferenceLength) + ")")
	}
	with := func(msg string) string {
		if detail == "" {
			return msg
		}
		return msg + ": " + detail
	}
	code := "HTTP " + strconv.Itoa(status)
	success := status >= http.StatusOK && status < http.StatusMultipleChoices
	switch {
	case answer.Status == backupFailed:
		return core.GateFailed, with("backup failed (" + code + ")")
	case success && answer.Status == backupSucceeded:
		return core.GatePassed, with("backup completed")
	case success && (answer.Status == backupRunning || status == http.StatusAccepted):
		return core.GatePending, with("backup running")
	case success:
		return core.GatePassed, with("backup completed")
	case status >= http.StatusMultipleChoices && status < http.StatusBadRequest:
		return core.GateFailed, "backup webhook answered " + code + "; redirects are not followed"
	case status == http.StatusRequestTimeout, status == http.StatusTooEarly, status == http.StatusTooManyRequests, status >= http.StatusInternalServerError:
		return core.GatePending, with("backup webhook answered " + code + "; retrying")
	default:
		return core.GateFailed, with("backup webhook rejected the request (" + code + ")")
	}
}

func previousBackup(prev *core.GateResult) (backupEvidence, bool) {
	if prev == nil || prev.Status != core.GatePending || len(prev.Evidence) == 0 {
		return backupEvidence{}, false
	}
	var ev backupEvidence
	if json.Unmarshal(prev.Evidence, &ev) != nil {
		return backupEvidence{}, false
	}
	return ev, true
}

func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}
