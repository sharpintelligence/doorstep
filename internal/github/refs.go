package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func (r *Repo) BranchSHA(ctx context.Context, branch string) (string, error) {
	if err := validateRef(branch); err != nil {
		return "", err
	}
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := r.do(ctx, http.MethodGet, r.endpoint("git/ref/heads", branch), nil, nil, &out); err != nil {
		if HasStatus(err, http.StatusNotFound) {
			return "", fmt.Errorf("%w: branch %s in %s: %w", ErrRefNotFound, branch, r.FullName(), err)
		}
		return "", fmt.Errorf("get branch %s in %s: %w", branch, r.FullName(), err)
	}
	if out.Object.SHA == "" {
		return "", fmt.Errorf("get branch %s in %s: response has no sha", branch, r.FullName())
	}
	return out.Object.SHA, nil
}

func (r *Repo) CreateBranch(ctx context.Context, branch, sha string) error {
	if err := validateRef(branch); err != nil {
		return err
	}
	body := struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}{Ref: "refs/heads/" + branch, SHA: sha}
	if err := r.do(ctx, http.MethodPost, r.endpoint("git/refs"), nil, body, nil); err != nil {
		if hasMessage(err, http.StatusUnprocessableEntity, "already exists") {
			return fmt.Errorf("%w: branch %s in %s: %w", ErrRefExists, branch, r.FullName(), err)
		}
		return fmt.Errorf("create branch %s in %s: %w", branch, r.FullName(), err)
	}
	return nil
}

func (r *Repo) DeleteBranch(ctx context.Context, branch string) error {
	if err := validateRef(branch); err != nil {
		return err
	}
	if err := r.do(ctx, http.MethodDelete, r.endpoint("git/refs/heads", branch), nil, nil, nil); err != nil {
		if HasStatus(err, http.StatusNotFound) || hasMessage(err, http.StatusUnprocessableEntity, "does not exist") {
			return fmt.Errorf("%w: branch %s in %s: %w", ErrRefNotFound, branch, r.FullName(), err)
		}
		return fmt.Errorf("delete branch %s in %s: %w", branch, r.FullName(), err)
	}
	return nil
}

func hasMessage(err error, status int, fragment string) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status &&
		strings.Contains(strings.ToLower(apiErr.Message), fragment)
}
