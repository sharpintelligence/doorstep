package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sharpintelligence/doorstep/internal/argocd"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/kube"
	"github.com/sharpintelligence/doorstep/internal/logging"
)

type repoOpener interface {
	Repo(ctx context.Context, owner, name string) (*github.Repo, error)
}

type githubAncestry struct {
	repos repoOpener
}

func (a githubAncestry) Contains(ctx context.Context, repository, ancestor, descendant string) (bool, error) {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" {
		return false, fmt.Errorf("ancestry: repository %q must be owner/name", repository)
	}
	repo, err := a.repos.Repo(ctx, owner, name)
	if err != nil {
		return false, err
	}
	comparison, err := repo.Compare(ctx, ancestor, descendant)
	if err != nil {
		if github.HasStatus(err, http.StatusNotFound) {
			return false, nil
		}
		return false, err
	}
	return comparison.HeadContainsBase(), nil
}

func newKubeClient(cfg config.Kubernetes, lookupEnv LookupEnv, readFile func(string) ([]byte, error)) (*kube.Client, error) {
	var kc kube.Config
	var err error
	if cfg.Kubeconfig != "" {
		kc, err = kube.LoadKubeconfig(cfg.Kubeconfig, cfg.Context, readFile)
	} else {
		kc, err = kube.InCluster(lookupEnv, readFile)
		if errors.Is(err, kube.ErrNotInCluster) {
			err = fmt.Errorf("%w; set %s or pass --%s to use a kubeconfig file", err, config.EnvKubeconfig, flagKubeconfig)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("kubernetes: %w", err)
	}
	client, err := kube.New(kc, kube.Options{UserAgent: userAgent()})
	if err != nil {
		return nil, fmt.Errorf("kubernetes: %w", err)
	}
	return client, nil
}

func syncApplications(catalog config.Catalog) map[core.Target]argocd.Application {
	apps := make(map[core.Target]argocd.Application, len(catalog.Apps))
	for _, a := range catalog.Apps {
		apps[core.AppTarget(a.Name)] = argocd.Application{
			Namespace:   a.ArgoCD.Namespace,
			Name:        a.ArgoCD.Application,
			Repository:  string(a.GitOps.Repo),
			SyncTimeout: a.ArgoCD.Timeout(),
		}
	}
	return apps
}

func newSyncWatcher(cfg config.Config, catalog config.Catalog, lookupEnv LookupEnv, githubApp *github.App, logger *slog.Logger) (*argocd.Watcher, error) {
	client, err := newKubeClient(cfg.Kubernetes, lookupEnv, os.ReadFile)
	if err != nil {
		return nil, err
	}
	opts := argocd.Options{Client: client, Applications: syncApplications(catalog), Logger: logger}
	if githubApp != nil {
		opts.Ancestry = githubAncestry{repos: githubApp}
	}
	return argocd.New(opts)
}

func newArgoCDCommand(lookupEnv LookupEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "argocd",
		Short: "Inspect the Argo CD Applications doorstep watches",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newArgoCDStatusCommand(lookupEnv))
	return cmd
}

func newArgoCDStatusCommand(lookupEnv LookupEnv) *cobra.Command {
	var overrides config.Config
	var configFlag, revision string
	cmd := &cobra.Command{
		Use:   "status <app>",
		Short: "Show the sync state and health of an app's Argo CD Application",
		Long: "Read the Argo CD Application of one configured app through the Kubernetes API, with the same connection and permissions as the server, " +
			"and print its sync state, revision, health and unhealthy resources. With --revision, also report whether the synced revision contains " +
			"that GitOps commit and why the app is not ready. Exits non-zero when the Application cannot be read.",
		Example: "  doorstep argocd status blog --config /etc/doorstep/config.yaml --kubeconfig ~/.kube/config --kube-context kind-homelab",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			changed := cmd.Flags().Changed
			path, err := resolveConfigPath(changed, configFlag, lookupEnv)
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
			cfg, err := resolveConfig(changed, overrides, file, lookupEnv)
			if err != nil {
				return err
			}
			catalog := file.Catalog()
			if err := errors.Join(cfg.ValidateSettings(), catalog.Validate()); err != nil {
				return fmt.Errorf("invalid configuration: %w", err)
			}
			app, ok := catalog.App(args[0])
			if !ok {
				return fmt.Errorf("unknown app %q (configured: %s)", args[0], appNames(catalog))
			}
			logger, err := logging.New(cmd.ErrOrStderr(), "warn", logging.FormatText)
			if err != nil {
				return err
			}
			var githubApp *github.App
			if revision != "" {
				if githubApp, err = newGitHubApp(cfg.GitHub, os.ReadFile, logger); err != nil {
					return err
				}
			}
			watcher, err := newSyncWatcher(cfg, catalog, lookupEnv, githubApp, logger)
			if err != nil {
				return err
			}
			target := core.AppTarget(app.Name)
			var status core.SyncStatus
			if revision != "" {
				status, err = watcher.Check(cmd.Context(), target, revision)
			} else {
				status, err = watcher.Status(cmd.Context(), target)
			}
			if err != nil {
				return err
			}
			application, _ := watcher.Application(target)
			return printSyncStatus(cmd.OutOrStdout(), application, revision, status)
		},
	}
	flags := cmd.Flags()
	addConfigFlag(flags, &configFlag)
	bindKubernetesFlags(flags, &overrides)
	flags.StringVar(&revision, "revision", "", "GitOps commit SHA to check for; uses the GitHub App, when configured, to accept later commits that contain it")
	return cmd
}

func printSyncStatus(w io.Writer, app argocd.Application, revision string, status core.SyncStatus) error {
	var b strings.Builder
	line := func(label, value string) {
		fmt.Fprintf(&b, "%-12s %s\n", label, value)
	}
	line("application", app.String())
	line("sync", string(status.State))
	line("revision", orNone(status.Revision))
	if status.OperationPhase != "" {
		line("operation", joinDetail(status.OperationPhase, status.OperationMessage))
	}
	line("health", joinDetail(string(status.Health.Status), status.Health.Message))
	if revision != "" {
		line("pin", revision)
		line("reached", yesNo(status.Reached))
		line("ready", yesNo(status.Ready()))
		if f := status.Failure; f != nil {
			line("reason", fmt.Sprintf("%s (%s): %s", f.Reason, f.Class, f.Message))
		}
	}
	if len(status.Health.Resources) > 0 {
		b.WriteString("resources\n")
		for _, r := range status.Health.Resources {
			kind := r.Kind
			if r.Group != "" {
				kind = r.Group + "/" + r.Kind
			}
			name := r.Name
			if r.Namespace != "" {
				name = r.Namespace + "/" + r.Name
			}
			fmt.Fprintf(&b, "  %s %s: %s", kind, name, r.Health)
			if r.Sync != "" {
				fmt.Fprintf(&b, ", %s", r.Sync)
			}
			if r.Message != "" {
				fmt.Fprintf(&b, ": %s", r.Message)
			}
			b.WriteByte('\n')
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func appNames(catalog config.Catalog) string {
	if len(catalog.Apps) == 0 {
		return "none"
	}
	names := make([]string, 0, len(catalog.Apps))
	for _, a := range catalog.Apps {
		names = append(names, a.Name)
	}
	return strings.Join(names, ", ")
}

func joinDetail(summary, detail string) string {
	if detail == "" {
		return summary
	}
	return summary + ": " + detail
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
