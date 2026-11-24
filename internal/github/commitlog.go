package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

const maxCommitsPerPage = 100

type CommitInfo struct {
	SHA     string
	Message string
}

func (r *Repo) ListCommits(ctx context.Context, ref, path string, limit int) ([]CommitInfo, error) {
	if err := validateRef(ref); err != nil {
		return nil, err
	}
	if err := validatePath(path); err != nil {
		return nil, err
	}
	if limit < 1 || limit > maxCommitsPerPage {
		return nil, fmt.Errorf("list commits: limit %d must be between 1 and %d", limit, maxCommitsPerPage)
	}
	query := url.Values{"sha": {ref}, "path": {path}, "per_page": {strconv.Itoa(limit)}}
	var out []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := r.do(ctx, http.MethodGet, r.endpoint("commits"), query, nil, &out); err != nil {
		return nil, fmt.Errorf("list commits of %s on %s in %s: %w", path, ref, r.FullName(), err)
	}
	out = out[:min(len(out), limit)]
	commits := make([]CommitInfo, 0, len(out))
	for _, c := range out {
		commits = append(commits, CommitInfo{SHA: c.SHA, Message: c.Commit.Message})
	}
	return commits, nil
}
