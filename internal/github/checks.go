package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

const (
	checksPerPage = 100
	maxCheckPages = 10
)

var commitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type AppRef struct {
	Slug string `json:"slug"`
}

type CheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	App        AppRef `json:"app"`
}

type CommitStatus struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
}

type CheckSuite struct {
	ID                   int64  `json:"id"`
	Status               string `json:"status"`
	Conclusion           string `json:"conclusion"`
	LatestCheckRunsCount int    `json:"latest_check_runs_count"`
	App                  AppRef `json:"app"`
}

type checkRunsPage struct {
	TotalCount int        `json:"total_count"`
	CheckRuns  []CheckRun `json:"check_runs"`
}

type combinedStatusPage struct {
	TotalCount int            `json:"total_count"`
	Statuses   []CommitStatus `json:"statuses"`
}

type checkSuitesPage struct {
	TotalCount  int          `json:"total_count"`
	CheckSuites []CheckSuite `json:"check_suites"`
}

type commitResource struct {
	owner, repo, sha string
	resource         string
	query            string
	what             string
}

func (c *Client) ListCheckRuns(ctx context.Context, owner, repo, sha string) ([]CheckRun, error) {
	r := commitResource{owner: owner, repo: repo, sha: sha, resource: "check-runs", query: "filter=latest&", what: "check runs"}
	return collectPages(ctx, c, r, func(p checkRunsPage) (int, []CheckRun) { return p.TotalCount, p.CheckRuns })
}

func (c *Client) CombinedStatus(ctx context.Context, owner, repo, sha string) ([]CommitStatus, error) {
	r := commitResource{owner: owner, repo: repo, sha: sha, resource: "status", what: "commit statuses"}
	return collectPages(ctx, c, r, func(p combinedStatusPage) (int, []CommitStatus) { return p.TotalCount, p.Statuses })
}

func (c *Client) ListCheckSuites(ctx context.Context, owner, repo, sha string) ([]CheckSuite, error) {
	r := commitResource{owner: owner, repo: repo, sha: sha, resource: "check-suites", what: "check suites"}
	return collectPages(ctx, c, r, func(p checkSuitesPage) (int, []CheckSuite) { return p.TotalCount, p.CheckSuites })
}

func collectPages[P, T any](ctx context.Context, c *Client, r commitResource, items func(P) (int, []T)) ([]T, error) {
	if err := validateRepository(r.owner, r.repo); err != nil {
		return nil, err
	}
	if !commitSHAPattern.MatchString(r.sha) {
		return nil, fmt.Errorf("list %s: %w: %q must be 40 lowercase hex characters", r.what, ErrInvalidCommitSHA, r.sha)
	}
	base := "/repos/" + url.PathEscape(r.owner) + "/" + url.PathEscape(r.repo) + "/commits/" + r.sha + "/" + r.resource +
		"?" + r.query + "per_page=" + strconv.Itoa(checksPerPage) + "&page="

	all := []T{}
	for n := 1; n <= maxCheckPages; n++ {
		var page P
		if err := c.Do(ctx, http.MethodGet, base+strconv.Itoa(n), nil, &page); err != nil {
			return nil, fmt.Errorf("list %s for %s/%s@%s: %w", r.what, r.owner, r.repo, r.sha, err)
		}
		total, got := items(page)
		all = append(all, got...)
		if len(got) == 0 || len(all) >= total {
			return all, nil
		}
	}
	return nil, fmt.Errorf("list %s for %s/%s@%s: %w: more than %d", r.what, r.owner, r.repo, r.sha, ErrTooManyChecks, maxCheckPages*checksPerPage)
}
