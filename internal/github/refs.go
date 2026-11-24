package github

import (
	"context"
	"fmt"
	"net/http"
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
		if HasMessage(err, http.StatusUnprocessableEntity, "already exists") {
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
		if HasStatus(err, http.StatusNotFound) || HasMessage(err, http.StatusUnprocessableEntity, "reference does not exist") {
			return fmt.Errorf("%w: branch %s in %s: %w", ErrRefNotFound, branch, r.FullName(), err)
		}
		return fmt.Errorf("delete branch %s in %s: %w", branch, r.FullName(), err)
	}
	return nil
}

func (r *Repo) ResetBranch(ctx context.Context, branch, sha string) error {
	if err := validateRef(branch); err != nil {
		return err
	}
	body := struct {
		SHA   string `json:"sha"`
		Force bool   `json:"force"`
	}{SHA: sha, Force: true}
	if err := r.do(ctx, http.MethodPatch, r.endpoint("git/refs/heads", branch), nil, body, nil); err != nil {
		if HasStatus(err, http.StatusNotFound) || HasMessage(err, http.StatusUnprocessableEntity, "reference does not exist") {
			return fmt.Errorf("%w: branch %s in %s: %w", ErrRefNotFound, branch, r.FullName(), err)
		}
		return fmt.Errorf("reset branch %s in %s: %w", branch, r.FullName(), err)
	}
	return nil
}
