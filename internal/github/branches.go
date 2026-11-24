package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type gitRef struct {
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

func (c *Client) BranchHead(ctx context.Context, owner, repo, branch string) (string, error) {
	if err := validateRepository(owner, repo); err != nil {
		return "", err
	}
	escaped, err := escapeBranch(branch)
	if err != nil {
		return "", err
	}
	var ref gitRef
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/git/ref/heads/" + escaped
	if err := c.Do(ctx, http.MethodGet, path, nil, &ref); err != nil {
		if hasStatus(err, http.StatusNotFound) {
			return "", fmt.Errorf("head of %s/%s branch %s: %w: %w", owner, repo, branch, ErrBranchNotFound, err)
		}
		return "", fmt.Errorf("head of %s/%s branch %s: %w", owner, repo, branch, err)
	}
	if !commitSHAPattern.MatchString(ref.Object.SHA) {
		return "", fmt.Errorf("head of %s/%s branch %s: %w: %q", owner, repo, branch, ErrInvalidCommitSHA, ref.Object.SHA)
	}
	return ref.Object.SHA, nil
}

func escapeBranch(branch string) (string, error) {
	segments := strings.Split(branch, "/")
	for i, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("%w: %q", ErrInvalidBranch, branch)
		}
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/"), nil
}
