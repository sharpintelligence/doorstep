package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/intake"
	"github.com/sharpintelligence/doorstep/internal/metrics"
	"github.com/sharpintelligence/doorstep/internal/queue"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type intakeInput struct {
	config    config.Config
	catalog   config.Catalog
	githubApp *github.App
	store     *store.Store
	maxBatch  func(app string) int
	readFile  func(string) ([]byte, error)
	recorder  *metrics.Recorder
	announcer announcer
	logger    *slog.Logger
}

type intakeWiring struct {
	routes map[string]http.Handler
	runs   []func(context.Context) error
}

func newIntake(ctx context.Context, in intakeInput) (intakeWiring, error) {
	wiring := intakeWiring{routes: map[string]http.Handler{}}
	if in.githubApp == nil {
		return wiring, nil
	}
	secret, err := in.config.GitHub.ReadWebhookSecret(in.readFile)
	if err != nil {
		return intakeWiring{}, err
	}
	if len(in.catalog.Apps) == 0 {
		in.logger.InfoContext(ctx, "github intake idle: no apps configured")
		return wiring, nil
	}
	apps := intakeApps(in.catalog)
	var commits intake.Commits = intake.NewPostgresCommits(in.store, queueFor(in.maxBatch, in.logger))
	if in.announcer != nil {
		commits = announcingCommits{Commits: commits, announcer: in.announcer}
	}
	service, err := intake.New(intake.Options{
		Apps:       apps,
		Commits:    commits,
		Checks:     intake.GitHub{Clients: in.githubApp},
		Branches:   intake.GitHub{Clients: in.githubApp},
		Migrations: intake.GitHub{Clients: in.githubApp},
		Baselines:  in.store,
		Logger:     in.logger,
	})
	if err != nil {
		return intakeWiring{}, err
	}
	observer := intakeObserver{recorder: in.recorder}

	if secret != nil {
		deliveries := intake.NewPostgresDeliveries(in.store)
		handler, err := intake.NewWebhook(intake.WebhookOptions{
			Secret:        secret,
			Intake:        service,
			Deliveries:    deliveries,
			Installations: in.githubApp,
			Observer:      observer,
			Logger:        in.logger,
		})
		if err != nil {
			return intakeWiring{}, err
		}
		sweeper, err := intake.NewSweeper(intake.SweeperOptions{Deliveries: deliveries, Logger: in.logger})
		if err != nil {
			return intakeWiring{}, err
		}
		wiring.routes[intake.WebhookPattern] = handler
		wiring.runs = append(wiring.runs, sweeper.Run)
		in.logger.InfoContext(ctx, "github webhooks enabled", slog.String("route", intake.WebhookPattern))
	}

	interval := in.config.Intake.PollInterval
	if interval > 0 {
		poller, err := intake.NewPoller(intake.PollerOptions{
			Apps:     apps,
			Intake:   service,
			Branches: intake.GitHub{Clients: in.githubApp},
			Claims:   in.store,
			Interval: interval,
			Observer: observer,
			Logger:   in.logger,
		})
		if err != nil {
			return intakeWiring{}, err
		}
		wiring.runs = append(wiring.runs, poller.Run)
	}

	switch {
	case secret == nil && interval > 0:
		in.logger.InfoContext(ctx, "github webhooks disabled, polling only", slog.Duration("poll_interval", interval))
	case secret == nil:
		in.logger.WarnContext(ctx, "github webhooks and polling are both disabled: new commits are not detected")
	case interval > 0:
		in.logger.InfoContext(ctx, "github polling enabled", slog.Duration("poll_interval", interval))
	default:
		in.logger.InfoContext(ctx, "github polling disabled, webhooks only")
	}
	return wiring, nil
}

func queueFor(maxBatch func(app string) int, logger *slog.Logger) func(tx *store.Store) intake.Queue {
	return func(tx *store.Store) intake.Queue {
		return intakeQueue{queue: queue.New(queue.Options{Store: tx, MaxBatch: maxBatch, Logger: logger})}
	}
}

type intakeQueue struct {
	queue *queue.Queue
}

func (q intakeQueue) Enqueue(ctx context.Context, d core.Deployment) (core.Deployment, error) {
	result, err := q.queue.Enqueue(ctx, d, queue.EnqueueOptions{})
	if err != nil {
		return core.Deployment{}, fmt.Errorf("enqueue %s: %w", d.Target, err)
	}
	return result.Deployment, nil
}

type intakeObserver struct {
	recorder *metrics.Recorder
}

func (o intakeObserver) WebhookDelivery(event string, result intake.WebhookResult) {
	o.recorder.WebhookDelivery(metrics.WebhookEvent(event), metrics.WebhookResult(result))
}

func (o intakeObserver) PollCycle(result intake.PollResult) {
	o.recorder.PollCycle(metrics.PollResult(result))
}
