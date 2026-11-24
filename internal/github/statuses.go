package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type CommitStatusState string

const (
	CommitStatusPending CommitStatusState = "pending"
	CommitStatusSuccess CommitStatusState = "success"
	CommitStatusFailure CommitStatusState = "failure"
	CommitStatusError   CommitStatusState = "error"
)

func (s CommitStatusState) valid() bool {
	switch s {
	case CommitStatusPending, CommitStatusSuccess, CommitStatusFailure, CommitStatusError:
		return true
	default:
		return false
	}
}

type CommitStatusRequest struct {
	State       CommitStatusState
	Context     string
	Description string
	TargetURL   string
}

type commitStatusBody struct {
	State       CommitStatusState `json:"state"`
	Context     string            `json:"context"`
	Description string            `json:"description,omitempty"`
	TargetURL   string            `json:"target_url,omitempty"`
}

func (c *Client) CreateCommitStatus(ctx context.Context, owner, repo, sha string, req CommitStatusRequest) error {
	if !commitSHAPattern.MatchString(sha) {
		return fmt.Errorf("create commit status: %w: %q must be 40 lowercase hex characters", ErrInvalidCommitSHA, sha)
	}
	switch {
	case !req.State.valid():
		return fmt.Errorf("create commit status: unknown state %q", req.State)
	case strings.TrimSpace(req.Context) == "":
		return errors.New("create commit status: context must not be empty")
	}
	p, err := repoPath(owner, repo, "statuses", sha)
	if err != nil {
		return err
	}
	body := commitStatusBody{
		State:       req.State,
		Context:     req.Context,
		Description: truncateDescription(req.Description),
		TargetURL:   req.TargetURL,
	}
	if err := c.Do(ctx, http.MethodPost, p, body, nil); err != nil {
		return fmt.Errorf("create %s commit status %s for %s/%s@%s: %w", req.State, req.Context, owner, repo, sha, err)
	}
	return nil
}
