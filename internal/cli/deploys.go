package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/deploys"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/intake"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type deployService interface {
	Deploy(ctx context.Context, req deploys.Request) (deploys.Result, error)
	Cancel(ctx context.Context, req deploys.CancelRequest) (core.Deployment, error)
}

func deployApps(catalog config.Catalog) []deploys.App {
	apps := make([]deploys.App, 0, len(catalog.Apps))
	for _, a := range catalog.Apps {
		apps = append(apps, deploys.App{
			Target:          core.AppTarget(a.Name),
			Repository:      string(a.Source.Repo),
			Branch:          a.Source.Branch,
			ImageRepository: a.Image.Repository,
		})
	}
	return apps
}

type branchCommits struct {
	repos repoOpener
}

func (c branchCommits) OnBranch(ctx context.Context, repository, branch, sha string) (bool, error) {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" {
		return false, fmt.Errorf("repository %q must be owner/name", repository)
	}
	repo, err := c.repos.Repo(ctx, owner, name)
	if err != nil {
		return false, err
	}
	head, err := repo.BranchSHA(ctx, branch)
	if err != nil {
		return false, err
	}
	if head == sha {
		return true, nil
	}
	comparison, err := repo.Compare(ctx, sha, head)
	if github.HasStatus(err, http.StatusNotFound) {
		return false, fmt.Errorf("%w: %s in %s", deploys.ErrCommitNotFound, sha, repository)
	}
	if err != nil {
		return false, err
	}
	return comparison.HeadContainsBase(), nil
}

type migrationFlagger interface {
	MigrationFlag(ctx context.Context, t core.Target, commit core.Commit) (intake.MigrationFlag, error)
}

type intakeMigrations struct {
	intake migrationFlagger
}

func (m intakeMigrations) MigrationFlag(ctx context.Context, t core.Target, commit core.Commit) (bool, string, error) {
	flag, err := m.intake.MigrationFlag(ctx, t, commit)
	return flag.Flagged, flag.Reason, err
}

type announcingDeploys struct {
	deploys   deployService
	announcer announcer
}

func (d announcingDeploys) Deploy(ctx context.Context, req deploys.Request) (deploys.Result, error) {
	result, err := d.deploys.Deploy(ctx, req)
	if err != nil || !result.Created {
		return result, err
	}
	ctx, cancel := announceContext(ctx)
	defer cancel()
	d.announcer.Queued(ctx, result.Deployment.ID)
	return result, nil
}

func (d announcingDeploys) Cancel(ctx context.Context, req deploys.CancelRequest) (core.Deployment, error) {
	cancelled, err := d.deploys.Cancel(ctx, req)
	if err != nil {
		return cancelled, err
	}
	ctx, cancel := announceContext(ctx)
	defer cancel()
	d.announcer.Cancelled(ctx, cancelled.ID)
	return cancelled, nil
}

type deploysInput struct {
	store     *store.Store
	catalog   config.Catalog
	githubApp *github.App
	intake    migrationFlagger
	announcer announcer
	logger    *slog.Logger
}

func newDeploys(in deploysInput) (deployService, error) {
	service, err := deploys.New(deploys.Options{
		Repository: deploys.NewRepository(in.store),
		Apps:       deployApps(in.catalog),
		Commits:    branchCommits{repos: in.githubApp},
		Migrations: intakeMigrations{intake: in.intake},
		Logger:     in.logger,
	})
	if err != nil {
		return nil, err
	}
	return announcingDeploys{deploys: service, announcer: in.announcer}, nil
}
