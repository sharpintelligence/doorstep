package intake

import (
	"context"
	"fmt"

	"github.com/sharpintelligence/doorstep/internal/github"
)

type RepoClients interface {
	RepoClient(ctx context.Context, owner, repo string) (*github.Client, error)
}

type GitHubChecks struct {
	Clients RepoClients
}

func (g GitHubChecks) Checks(ctx context.Context, repository, sha string, withSuites bool) (Snapshot, error) {
	owner, name, err := splitRepository(repository)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read checks: %w", err)
	}
	client, err := g.Clients.RepoClient(ctx, owner, name)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read checks for %s@%s: %w", repository, sha, err)
	}
	var s Snapshot
	if s.CheckRuns, err = client.ListCheckRuns(ctx, owner, name, sha); err != nil {
		return Snapshot{}, err
	}
	if s.Statuses, err = client.CombinedStatus(ctx, owner, name, sha); err != nil {
		return Snapshot{}, err
	}
	if withSuites {
		if s.Suites, err = client.ListCheckSuites(ctx, owner, name, sha); err != nil {
			return Snapshot{}, err
		}
	}
	return s, nil
}
