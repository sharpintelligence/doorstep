package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

const (
	PullStateOpen   = "open"
	PullStateClosed = "closed"

	MergeMethodSquash = "squash"

	MergeableBehind = "behind"
	MergeableDirty  = "dirty"
)

type PullRequest struct {
	Number         int
	State          string
	Merged         bool
	MergeableState string
	MergeCommitSHA string
	HeadRef        string
	HeadSHA        string
	HeadRepo       string
	BaseRef        string
	Title          string
	Body           string
	HTMLURL        string
	ChangedFiles   int
}

type NewPullRequest struct {
	Title string
	Body  string
	Head  string
	Base  string
}

type PullRequestUpdate struct {
	State string `json:"state,omitempty"`
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

type MergeRequest struct {
	SHA     string
	Title   string
	Message string
	Method  string
}

type pullResponse struct {
	Number         int     `json:"number"`
	State          string  `json:"state"`
	Merged         bool    `json:"merged"`
	MergedAt       *string `json:"merged_at"`
	MergeableState string  `json:"mergeable_state"`
	MergeCommitSHA *string `json:"merge_commit_sha"`
	Title          string  `json:"title"`
	Body           *string `json:"body"`
	HTMLURL        string  `json:"html_url"`
	ChangedFiles   int     `json:"changed_files"`
	Head           struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p pullResponse) toPullRequest() PullRequest {
	pr := PullRequest{
		Number:         p.Number,
		State:          p.State,
		Merged:         p.Merged || p.MergedAt != nil,
		MergeableState: p.MergeableState,
		HeadRef:        p.Head.Ref,
		HeadSHA:        p.Head.SHA,
		BaseRef:        p.Base.Ref,
		Title:          p.Title,
		HTMLURL:        p.HTMLURL,
		ChangedFiles:   p.ChangedFiles,
	}
	if p.MergeCommitSHA != nil {
		pr.MergeCommitSHA = *p.MergeCommitSHA
	}
	if p.Body != nil {
		pr.Body = *p.Body
	}
	if p.Head.Repo != nil {
		pr.HeadRepo = p.Head.Repo.FullName
	}
	return pr
}

func (r *Repo) ListPullRequests(ctx context.Context, head, base string) ([]PullRequest, error) {
	if err := validateRef(head); err != nil {
		return nil, err
	}
	if err := validateRef(base); err != nil {
		return nil, err
	}
	query := url.Values{
		"state":     {"all"},
		"head":      {r.owner + ":" + head},
		"base":      {base},
		"sort":      {"created"},
		"direction": {"desc"},
		"per_page":  {"100"},
	}
	var out []pullResponse
	if err := r.do(ctx, http.MethodGet, r.endpoint("pulls"), query, nil, &out); err != nil {
		return nil, fmt.Errorf("list pull requests from %s to %s in %s: %w", head, base, r.FullName(), err)
	}
	pulls := make([]PullRequest, 0, len(out))
	for _, p := range out {
		pulls = append(pulls, p.toPullRequest())
	}
	return pulls, nil
}

func (r *Repo) CreatePullRequest(ctx context.Context, req NewPullRequest) (PullRequest, error) {
	if err := validateRef(req.Head); err != nil {
		return PullRequest{}, err
	}
	if err := validateRef(req.Base); err != nil {
		return PullRequest{}, err
	}
	if req.Title == "" {
		return PullRequest{}, errors.New("create pull request: title is required")
	}
	body := struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}{Title: req.Title, Body: req.Body, Head: req.Head, Base: req.Base}
	var out pullResponse
	if err := r.do(ctx, http.MethodPost, r.endpoint("pulls"), nil, body, &out); err != nil {
		if HasMessage(err, http.StatusUnprocessableEntity, "already exists") {
			return PullRequest{}, fmt.Errorf("%w: from %s to %s in %s: %w", ErrPullRequestExists, req.Head, req.Base, r.FullName(), err)
		}
		return PullRequest{}, fmt.Errorf("create pull request from %s to %s in %s: %w", req.Head, req.Base, r.FullName(), err)
	}
	return out.toPullRequest(), nil
}

func (r *Repo) GetPullRequest(ctx context.Context, number int) (PullRequest, error) {
	var out pullResponse
	if err := r.do(ctx, http.MethodGet, r.pullEndpoint(number), nil, nil, &out); err != nil {
		return PullRequest{}, fmt.Errorf("get pull request #%d in %s: %w", number, r.FullName(), err)
	}
	return out.toPullRequest(), nil
}

func (r *Repo) UpdatePullRequest(ctx context.Context, number int, update PullRequestUpdate) (PullRequest, error) {
	var out pullResponse
	if err := r.do(ctx, http.MethodPatch, r.pullEndpoint(number), nil, update, &out); err != nil {
		return PullRequest{}, fmt.Errorf("update pull request #%d in %s: %w", number, r.FullName(), err)
	}
	return out.toPullRequest(), nil
}

func (r *Repo) ClosePullRequest(ctx context.Context, number int) error {
	_, err := r.UpdatePullRequest(ctx, number, PullRequestUpdate{State: PullStateClosed})
	return err
}

func (r *Repo) MergePullRequest(ctx context.Context, number int, req MergeRequest) (string, error) {
	if req.SHA == "" {
		return "", errors.New("merge pull request: head sha is required")
	}
	body := struct {
		CommitTitle   string `json:"commit_title,omitempty"`
		CommitMessage string `json:"commit_message,omitempty"`
		SHA           string `json:"sha"`
		MergeMethod   string `json:"merge_method,omitempty"`
	}{CommitTitle: req.Title, CommitMessage: req.Message, SHA: req.SHA, MergeMethod: req.Method}
	var out struct {
		SHA    string `json:"sha"`
		Merged bool   `json:"merged"`
	}
	if err := r.do(ctx, http.MethodPut, r.pullEndpoint(number)+"/merge", nil, body, &out); err != nil {
		return "", fmt.Errorf("merge pull request #%d in %s: %w", number, r.FullName(), err)
	}
	if !out.Merged || out.SHA == "" {
		return "", fmt.Errorf("merge pull request #%d in %s: not merged", number, r.FullName())
	}
	return out.SHA, nil
}

func (r *Repo) UpdatePullRequestBranch(ctx context.Context, number int, expectedHeadSHA string) error {
	body := struct {
		ExpectedHeadSHA string `json:"expected_head_sha"`
	}{ExpectedHeadSHA: expectedHeadSHA}
	if err := r.do(ctx, http.MethodPut, r.pullEndpoint(number)+"/update-branch", nil, body, nil); err != nil {
		return fmt.Errorf("update branch of pull request #%d in %s: %w", number, r.FullName(), err)
	}
	return nil
}

func (r *Repo) pullEndpoint(number int) string {
	return r.endpoint("pulls", strconv.Itoa(number))
}
