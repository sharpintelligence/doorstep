package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/metrics"
	"github.com/sharpintelligence/doorstep/internal/notify"
)

const (
	testNotificationActor = "doorstep notify test"
	testNotificationSHA   = "0000000000000000000000000000000000000000"
	testNotificationApp   = "doorstep-test"
)

type secretSource struct {
	lookupEnv LookupEnv
	readFile  func(string) ([]byte, error)
}

func (s secretSource) reader(ref config.SecretRef) func() (string, error) {
	return func() (string, error) { return ref.Read(s.lookupEnv, s.readFile) }
}

type notifierInput struct {
	catalog    config.Catalog
	config     config.Config
	secrets    secretSource
	repos      notify.RepoClients
	logger     *slog.Logger
	observer   notify.Observer
	httpClient *http.Client
	userAgent  string
}

func newNotifier(catalog config.Catalog, cfg config.Config, lookupEnv LookupEnv, githubApp *github.App, logger *slog.Logger, recorder *metrics.Recorder) (*notify.Dispatcher, error) {
	in := notifierInput{
		catalog:    catalog,
		config:     cfg,
		secrets:    secretSource{lookupEnv: lookupEnv, readFile: config.ReadSecretFile},
		logger:     logger,
		httpClient: notify.NewHTTPClient(),
		userAgent:  userAgent(),
	}
	if githubApp != nil {
		in.repos = githubApp
	}
	if recorder != nil {
		in.observer = metricsObserver{recorder: recorder}
	}
	return buildNotifier(in)
}

func buildNotifier(in notifierInput) (*notify.Dispatcher, error) {
	links, err := notificationLinks(in.config)
	if err != nil {
		return nil, err
	}
	sinks := make([]notify.Sink, 0, len(in.catalog.Notifications))
	for _, n := range in.catalog.Notifications {
		sink, err := newNotificationSink(n, links, in.secrets, in.httpClient, in.userAgent)
		if err != nil {
			return nil, err
		}
		sinks = append(sinks, sink)
	}
	routes := make(map[string][]string, len(in.catalog.Apps))
	deployments := make(map[string]bool, len(in.catalog.Apps))
	for _, a := range in.catalog.Apps {
		if len(a.Notify) > 0 {
			routes[a.Name] = slices.Clone(a.Notify)
		}
		deployments[a.Name] = a.Source.DeploymentsEnabled()
	}
	var global []notify.Sink
	if in.repos != nil {
		sink, err := notify.NewGitHub(notify.GitHubOptions{Repos: in.repos, Apps: deployments, Links: links, Logger: in.logger})
		if err != nil {
			return nil, err
		}
		global = append(global, sink)
	}
	return notify.New(notify.Options{
		Sinks:    sinks,
		Routes:   routes,
		Global:   global,
		Logger:   in.logger,
		Observer: in.observer,
	})
}

func notificationLinks(cfg config.Config) (notify.Links, error) {
	var links notify.Links
	if base := cfg.Server.PublicBaseURL(); base != "" {
		links.DeploymentURL = func(id core.DeploymentID) string {
			return base + "/deployments/" + strconv.FormatInt(int64(id), 10)
		}
	}
	api, err := github.ParseBaseURL(cfg.GitHub.APIURL)
	if err != nil {
		return notify.Links{}, err
	}
	web := github.WebBaseURL(api).String()
	links.CommitURL = func(repository, sha string) string {
		return web + "/" + repository + "/commit/" + sha
	}
	return links, nil
}

func newNotificationSink(n config.Notification, links notify.Links, secrets secretSource, client *http.Client, ua string) (notify.Sink, error) {
	kinds, err := notificationKinds(n.EventNames())
	if err != nil {
		return nil, fmt.Errorf("notification %s: %w", n.Name, err)
	}
	endpoint := secrets.reader(n.Endpoint())
	switch n.Type {
	case config.NotifySlack:
		return notify.NewSlack(notify.SlackOptions{
			Name: n.Name, URL: endpoint, Kinds: kinds, Links: links, HTTPClient: client, UserAgent: ua,
		})
	case config.NotifyWebhook:
		opts := notify.WebhookOptions{
			Name: n.Name, URL: endpoint, Kinds: kinds, Links: links, HTTPClient: client, UserAgent: ua,
		}
		if ref, ok := n.SigningSecret(); ok {
			opts.Secret = secrets.reader(ref)
		}
		return notify.NewWebhook(opts)
	}
	return nil, fmt.Errorf("notification %s: unknown type %q", n.Name, n.Type)
}

func notificationKinds(events []string) ([]core.NotificationKind, error) {
	kinds := make([]core.NotificationKind, 0, len(events))
	for _, event := range events {
		kind, err := core.ParseNotificationKind(event)
		if err != nil {
			return nil, err
		}
		kinds = append(kinds, kind)
	}
	return kinds, nil
}

type metricsObserver struct {
	recorder *metrics.Recorder
}

func (o metricsObserver) Delivered(sinkType string, result notify.Result) {
	o.recorder.NotificationDelivered(metrics.NotificationType(sinkType), metrics.NotificationResult(result))
}

func newNotifyCommand(lookupEnv LookupEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "notify",
		Short: "Work with notification targets",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newNotifyTestCommand(lookupEnv))
	return cmd
}

func newNotifyTestCommand(lookupEnv LookupEnv) *cobra.Command {
	var configFlag string
	cmd := &cobra.Command{
		Use:   "test <notification-name>",
		Short: "Send a test deploy.succeeded notification to one notification target",
		Long: "Send one synthetic deploy.succeeded notification to the named notifications[] target from the configuration file, " +
			"with a single attempt and the same secrets, links and HTTP client as the server. Exits non-zero when the target does not accept it.",
		Example: "  doorstep notify test ops --config /etc/doorstep/config.yaml",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath(cmd.Flags().Changed, configFlag, lookupEnv)
			if err != nil {
				return err
			}
			if path == "" {
				return fmt.Errorf("no configuration file: pass --%s or set %s", flagConfig, config.EnvConfig)
			}
			file, err := loadFile(path)
			if err != nil {
				return err
			}
			cfg, err := config.FromEnv(file.Apply(config.Default()), lookupEnv)
			if err != nil {
				return fmt.Errorf("invalid environment: %w", err)
			}
			catalog := file.Catalog()
			if err := errors.Join(cfg.ValidateSettings(), catalog.Validate()); err != nil {
				return fmt.Errorf("invalid configuration: %w", err)
			}
			target, ok := catalog.Notification(args[0])
			if !ok {
				return fmt.Errorf("unknown notification %q (configured: %s)", args[0], notificationNames(catalog))
			}
			links, err := notificationLinks(cfg)
			if err != nil {
				return err
			}
			secrets := secretSource{lookupEnv: lookupEnv, readFile: config.ReadSecretFile}
			sink, err := newNotificationSink(target, links, secrets, notify.NewHTTPClient(), userAgent())
			if err != nil {
				return err
			}
			n := testNotification(catalog, target.Name, time.Now())
			if _, err := notify.SingleAttempt().Do(cmd.Context(), sink.Prepare(n)); err != nil {
				return fmt.Errorf("notification %s (%s): %w", target.Name, sink.Type(), err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "ok: sent %s to %s (%s)\n", n.Kind, target.Name, sink.Type())
			return err
		},
	}
	addConfigFlag(cmd.Flags(), &configFlag)
	return cmd
}

func notificationNames(catalog config.Catalog) string {
	if len(catalog.Notifications) == 0 {
		return "none"
	}
	names := make([]string, 0, len(catalog.Notifications))
	for _, n := range catalog.Notifications {
		names = append(names, n.Name)
	}
	return strings.Join(names, ", ")
}

func testNotification(catalog config.Catalog, target string, now time.Time) core.Notification {
	app := config.App{
		Name:   testNotificationApp,
		Source: config.Source{Repo: "example/" + testNotificationApp},
		Image:  config.Image{Repository: "registry.example.com/" + testNotificationApp},
	}
	for _, a := range catalog.Apps {
		if slices.Contains(a.Notify, target) {
			app = a
			break
		}
	}
	t := core.Target{App: app.Name, Environment: app.Name}
	started := now.Add(-time.Minute)
	return core.Notification{
		Kind:   core.NotificationDeploySucceeded,
		Target: t,
		Deployment: &core.Deployment{
			Target:      t,
			Kind:        core.KindDeploy,
			State:       core.StateSucceeded,
			Source:      core.SourceCLI,
			RequestedBy: testNotificationActor,
			Commit:      core.Commit{Repository: string(app.Source.Repo), SHA: testNotificationSHA},
			Image:       core.ImageRef{Repository: app.Image.Repository, Tag: testNotificationSHA},
			Digest:      core.Digest("sha256:" + strings.Repeat("0", 64)),
			CreatedAt:   started,
			UpdatedAt:   now,
			StartedAt:   &started,
			FinishedAt:  &now,
		},
		Actor:      testNotificationActor,
		Message:    "Test notification from doorstep notify test; nothing was deployed.",
		OccurredAt: now,
	}
}
