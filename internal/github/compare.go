package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

type Comparison string

const (
	ComparisonAhead     Comparison = "ahead"
	ComparisonBehind    Comparison = "behind"
	ComparisonIdentical Comparison = "identical"
	ComparisonDiverged  Comparison = "diverged"
)

func (c Comparison) HeadContainsBase() bool {
	return c == ComparisonAhead || c == ComparisonIdentical
}

func (r *Repo) Compare(ctx context.Context, base, head string) (Comparison, error) {
	for _, sha := range []string{base, head} {
		if !commitSHAPattern.MatchString(sha) {
			return "", fmt.Errorf("compare %s: %w: %q must be 40 lowercase hex characters", r.FullName(), ErrInvalidCommitSHA, sha)
		}
	}
	var out struct {
		Status Comparison `json:"status"`
	}
	if err := r.do(ctx, http.MethodGet, r.endpoint("compare", base+"..."+head), url.Values{"per_page": {"1"}}, nil, &out); err != nil {
		return "", fmt.Errorf("compare %s...%s in %s: %w", base, head, r.FullName(), err)
	}
	switch out.Status {
	case ComparisonAhead, ComparisonBehind, ComparisonIdentical, ComparisonDiverged:
		return out.Status, nil
	default:
		return "", fmt.Errorf("compare %s...%s in %s: unexpected status %q", base, head, r.FullName(), out.Status)
	}
}
