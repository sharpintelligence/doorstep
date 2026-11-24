package intake

import (
	"context"
	"errors"
	"fmt"

	"github.com/sharpintelligence/doorstep/internal/github"
)

type RepoClients interface {
	RepoClient(ctx context.Context, owner, repo string) (*github.Client, error)
}

type GitHub struct {
	Clients RepoClients
}

func (g GitHub) Checks(ctx context.Context, repository, sha string, names []string) (Snapshot, error) {
	client, owner, name, err := g.client(ctx, repository)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read checks for %s@%s: %w", repository, sha, err)
	}
	var s Snapshot
	if len(names) == 0 {
		if s.CheckRuns, err = client.ListCheckRuns(ctx, owner, name, sha); err != nil {
			return Snapshot{}, err
		}
		if s.Suites, err = client.ListCheckSuites(ctx, owner, name, sha); err != nil {
			return Snapshot{}, err
		}
	}
	for _, check := range uniqueNames(names) {
		runs, err := client.ListCheckRunsNamed(ctx, owner, name, sha, check)
		if err != nil {
			return Snapshot{}, err
		}
		s.CheckRuns = append(s.CheckRuns, runs...)
	}
	if s.Statuses, err = client.CombinedStatus(ctx, owner, name, sha); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

func (g GitHub) BranchHead(ctx context.Context, repository, branch string) (string, error) {
	client, owner, name, err := g.client(ctx, repository)
	if err != nil {
		return "", fmt.Errorf("read head of %s branch %s: %w", repository, branch, err)
	}
	head, err := client.BranchHead(ctx, owner, name, branch)
	if errors.Is(err, github.ErrBranchNotFound) {
		return "", nil
	}
	return head, err
}

func (g GitHub) client(ctx context.Context, repository string) (*github.Client, string, string, error) {
	owner, name, err := splitRepository(repository)
	if err != nil {
		return nil, "", "", err
	}
	client, err := g.Clients.RepoClient(ctx, owner, name)
	if err != nil {
		return nil, "", "", err
	}
	return client, owner, name, nil
}
