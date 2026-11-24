package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

const maxCompareCommits = 250

var ErrCommitNotFound = errors.New("commit not found")

type CommitRange struct {
	Status  Comparison
	Total   int
	Commits []CommitInfo
}

type CommitPullRequest struct {
	Number  int
	Merged  bool
	BaseRef string
	Labels  []string
}

type commitResponse struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

func (c commitResponse) info() CommitInfo {
	return CommitInfo{SHA: c.SHA, Message: c.Commit.Message}
}

func (c *Client) CommitRange(ctx context.Context, owner, repo, base, head string, limit int) (CommitRange, error) {
	path, err := repoPath(owner, repo, "compare", base+"..."+head)
	if err != nil {
		return CommitRange{}, err
	}
	for _, sha := range []string{base, head} {
		if !commitSHAPattern.MatchString(sha) {
			return CommitRange{}, fmt.Errorf("compare %s/%s: %w: %q must be 40 lowercase hex characters", owner, repo, ErrInvalidCommitSHA, sha)
		}
	}
	if limit < 1 || limit > maxCompareCommits {
		return CommitRange{}, fmt.Errorf("compare %s/%s: limit %d must be between 1 and %d", owner, repo, limit, maxCompareCommits)
	}
	var out struct {
		Status       Comparison       `json:"status"`
		TotalCommits int              `json:"total_commits"`
		Commits      []commitResponse `json:"commits"`
	}
	path += "?" + url.Values{"per_page": {strconv.Itoa(limit)}}.Encode()
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		if hasStatus(err, http.StatusNotFound) {
			return CommitRange{}, fmt.Errorf("compare %s...%s in %s/%s: %w: %w", base, head, owner, repo, ErrCommitNotFound, err)
		}
		return CommitRange{}, fmt.Errorf("compare %s...%s in %s/%s: %w", base, head, owner, repo, err)
	}
	switch out.Status {
	case ComparisonAhead, ComparisonBehind, ComparisonIdentical, ComparisonDiverged:
	default:
		return CommitRange{}, fmt.Errorf("compare %s...%s in %s/%s: unexpected status %q", base, head, owner, repo, out.Status)
	}
	r := CommitRange{Status: out.Status, Total: max(out.TotalCommits, len(out.Commits))}
	for _, commit := range out.Commits[:min(len(out.Commits), limit)] {
		if !commitSHAPattern.MatchString(commit.SHA) {
			return CommitRange{}, fmt.Errorf("compare %s...%s in %s/%s: %w: %q", base, head, owner, repo, ErrInvalidCommitSHA, commit.SHA)
		}
		r.Commits = append(r.Commits, commit.info())
	}
	return r, nil
}

func (c *Client) Commit(ctx context.Context, owner, repo, sha string) (CommitInfo, error) {
	path, err := commitPath(owner, repo, sha)
	if err != nil {
		return CommitInfo{}, err
	}
	var out commitResponse
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		if hasStatus(err, http.StatusNotFound) || hasStatus(err, http.StatusUnprocessableEntity) {
			return CommitInfo{}, fmt.Errorf("get commit %s in %s/%s: %w: %w", sha, owner, repo, ErrCommitNotFound, err)
		}
		return CommitInfo{}, fmt.Errorf("get commit %s in %s/%s: %w", sha, owner, repo, err)
	}
	if out.SHA != sha {
		return CommitInfo{}, fmt.Errorf("get commit %s in %s/%s: response names commit %q", sha, owner, repo, out.SHA)
	}
	return out.info(), nil
}

func (c *Client) CommitPullRequests(ctx context.Context, owner, repo, sha string) ([]CommitPullRequest, error) {
	path, err := commitPath(owner, repo, sha, "pulls")
	if err != nil {
		return nil, err
	}
	var out []struct {
		Number   int     `json:"number"`
		Merged   bool    `json:"merged"`
		MergedAt *string `json:"merged_at"`
		Base     struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	path += "?" + url.Values{"per_page": {"100"}}.Encode()
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		if hasStatus(err, http.StatusNotFound) || hasStatus(err, http.StatusUnprocessableEntity) {
			return nil, fmt.Errorf("pull requests of commit %s in %s/%s: %w: %w", sha, owner, repo, ErrCommitNotFound, err)
		}
		return nil, fmt.Errorf("pull requests of commit %s in %s/%s: %w", sha, owner, repo, err)
	}
	pulls := make([]CommitPullRequest, 0, len(out))
	for _, p := range out {
		pr := CommitPullRequest{Number: p.Number, Merged: p.Merged || p.MergedAt != nil, BaseRef: p.Base.Ref}
		for _, l := range p.Labels {
			pr.Labels = append(pr.Labels, l.Name)
		}
		pulls = append(pulls, pr)
	}
	return pulls, nil
}

func commitPath(owner, repo, sha string, rest ...string) (string, error) {
	if !commitSHAPattern.MatchString(sha) {
		return "", fmt.Errorf("%w: %q must be 40 lowercase hex characters", ErrInvalidCommitSHA, sha)
	}
	return repoPath(owner, repo, append([]string{"commits", sha}, rest...)...)
}
