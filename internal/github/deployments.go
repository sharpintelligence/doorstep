package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxDescriptionRunes = 140
	deploymentsPerPage  = 100
)

type DeploymentState string

const (
	DeploymentQueued     DeploymentState = "queued"
	DeploymentInProgress DeploymentState = "in_progress"
	DeploymentSuccess    DeploymentState = "success"
	DeploymentFailure    DeploymentState = "failure"
	DeploymentInactive   DeploymentState = "inactive"
	DeploymentError      DeploymentState = "error"
)

type DeploymentRequest struct {
	Ref         string
	Environment string
	Task        string
	Description string
	Payload     any
}

type Deployment struct {
	ID          int64
	SHA         string
	Ref         string
	Task        string
	Environment string
	Payload     json.RawMessage
}

type DeploymentFilter struct {
	SHA         string
	Environment string
	Task        string
}

type DeploymentStatusRequest struct {
	State        DeploymentState
	Description  string
	LogURL       string
	AutoInactive bool
}

type deploymentBody struct {
	Ref              string   `json:"ref"`
	Environment      string   `json:"environment,omitempty"`
	Task             string   `json:"task,omitempty"`
	Description      string   `json:"description,omitempty"`
	Payload          any      `json:"payload,omitempty"`
	AutoMerge        bool     `json:"auto_merge"`
	RequiredContexts []string `json:"required_contexts"`
}

type deploymentStatusBody struct {
	State        DeploymentState `json:"state"`
	Description  string          `json:"description,omitempty"`
	LogURL       string          `json:"log_url,omitempty"`
	AutoInactive bool            `json:"auto_inactive"`
}

type deploymentResponse struct {
	ID          int64           `json:"id"`
	SHA         string          `json:"sha"`
	Ref         string          `json:"ref"`
	Task        string          `json:"task"`
	Environment string          `json:"environment"`
	Payload     json.RawMessage `json:"payload"`
}

func (r deploymentResponse) deployment() Deployment {
	return Deployment{
		ID:          r.ID,
		SHA:         r.SHA,
		Ref:         r.Ref,
		Task:        r.Task,
		Environment: r.Environment,
		Payload:     normalizePayload(r.Payload),
	}
}

func normalizePayload(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] != '"' {
		return trimmed
	}
	var encoded string
	if err := json.Unmarshal(trimmed, &encoded); err != nil || !json.Valid([]byte(encoded)) {
		return nil
	}
	return json.RawMessage(encoded)
}

func truncateDescription(s string) string {
	if utf8.RuneCountInString(s) <= maxDescriptionRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxDescriptionRunes-1]) + "…"
}

func repoPath(owner, repo string, segments ...string) (string, error) {
	if err := validateRepository(owner, repo); err != nil {
		return "", err
	}
	escaped := []string{"", "repos", url.PathEscape(owner), url.PathEscape(repo)}
	for _, s := range segments {
		escaped = append(escaped, url.PathEscape(s))
	}
	return strings.Join(escaped, "/"), nil
}

func (c *Client) ListDeployments(ctx context.Context, owner, repo string, filter DeploymentFilter) ([]Deployment, error) {
	p, err := repoPath(owner, repo, "deployments")
	if err != nil {
		return nil, err
	}
	query := url.Values{"per_page": {strconv.Itoa(deploymentsPerPage)}}
	for key, value := range map[string]string{"sha": filter.SHA, "environment": filter.Environment, "task": filter.Task} {
		if value != "" {
			query.Set(key, value)
		}
	}
	var resp []deploymentResponse
	if err := c.Do(ctx, http.MethodGet, p+"?"+query.Encode(), nil, &resp); err != nil {
		return nil, fmt.Errorf("list deployments for %s/%s: %w", owner, repo, err)
	}
	deployments := make([]Deployment, 0, len(resp))
	for _, r := range resp {
		deployments = append(deployments, r.deployment())
	}
	return deployments, nil
}

func (c *Client) CreateDeployment(ctx context.Context, owner, repo string, req DeploymentRequest) (Deployment, error) {
	p, err := repoPath(owner, repo, "deployments")
	if err != nil {
		return Deployment{}, err
	}
	body := deploymentBody{
		Ref:              req.Ref,
		Environment:      req.Environment,
		Task:             req.Task,
		Description:      truncateDescription(req.Description),
		Payload:          req.Payload,
		RequiredContexts: []string{},
	}
	var resp deploymentResponse
	if err := c.Do(ctx, http.MethodPost, p, body, &resp); err != nil {
		return Deployment{}, fmt.Errorf("create deployment for %s/%s: %w", owner, repo, err)
	}
	if resp.ID == 0 {
		return Deployment{}, fmt.Errorf("create deployment for %s/%s: response has no deployment id", owner, repo)
	}
	return resp.deployment(), nil
}

func (c *Client) CreateDeploymentStatus(ctx context.Context, owner, repo string, deploymentID int64, req DeploymentStatusRequest) error {
	p, err := repoPath(owner, repo, "deployments", strconv.FormatInt(deploymentID, 10), "statuses")
	if err != nil {
		return err
	}
	body := deploymentStatusBody{
		State:        req.State,
		Description:  truncateDescription(req.Description),
		LogURL:       req.LogURL,
		AutoInactive: req.AutoInactive,
	}
	if err := c.Do(ctx, http.MethodPost, p, body, nil); err != nil {
		return fmt.Errorf("create %s deployment status for %s/%s deployment %d: %w", req.State, owner, repo, deploymentID, err)
	}
	return nil
}
