package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
)

const flagUrgent = "urgent"

type deployBody struct {
	CommitSHA string `json:"commit_sha"`
	Urgent    bool   `json:"urgent,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type cancelBody struct {
	Reason string `json:"reason,omitempty"`
}

type deployInfo struct {
	Deployment deploymentInfo `json:"deployment"`
	Created    bool           `json:"created"`
}

type cancelInfo struct {
	Deployment deploymentInfo `json:"deployment"`
}

func newDeployCommand(lookupEnv LookupEnv) *cobra.Command {
	var server, reason, output string
	var urgent bool
	cmd := &cobra.Command{
		Use:   "deploy <app> <commit-sha> [--urgent] [--reason <text>]",
		Short: "Queue a deploy of a commit on an app's branch",
		Long: "Queue a deploy of a commit. The commit must be the full 40 character SHA of a commit reachable from the branch the app follows, " +
			"so only merged code can be deployed. doorstep checks blocks and flags database migrations as it does for merges, " +
			"and runs the before gates (required checks included) when the deploy starts. --urgent queues it ahead of normal deploys. " +
			"If a deploy of the commit is already queued or running, doorstep reports that deployment instead of queueing another one.",
		Example: "  doorstep deploy blog 3f1c0d9e8b7a6f5e4d3c2b1a09f8e7d6c5b4a392\n" +
			"  doorstep deploy blog 3f1c0d9e8b7a6f5e4d3c2b1a09f8e7d6c5b4a392 --urgent --reason \"checkout hotfix\"",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, sha := args[0], strings.ToLower(args[1])
			if err := config.ValidateAppName(app); err != nil {
				return fmt.Errorf("app name: %w", err)
			}
			if err := core.ValidateCommitSHA(sha); err != nil {
				return fmt.Errorf("commit: %w", err)
			}
			if err := validateOutput(output); err != nil {
				return err
			}
			client, err := newAPIClient(cmd.Flags().Changed, server, lookupEnv)
			if err != nil {
				return err
			}
			var raw json.RawMessage
			body := deployBody{CommitSHA: sha, Urgent: urgent, Reason: strings.TrimSpace(reason)}
			if err := client.do(cmd.Context(), http.MethodPost, "/api/v1/apps/"+app+"/deployments", body, &raw); err != nil {
				return err
			}
			if output == outputJSON {
				return writeIndentedJSON(cmd.OutOrStdout(), raw)
			}
			var result deployInfo
			if err := json.Unmarshal(raw, &result); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			return writeDeploy(cmd.OutOrStdout(), app, urgent, result)
		},
	}
	flags := cmd.Flags()
	addServerFlag(flags, &server)
	flags.BoolVar(&urgent, flagUrgent, false, "queue ahead of normal deploys")
	flags.StringVar(&reason, flagReason, "", "why the commit is deployed by hand; recorded in the audit log")
	flags.StringVarP(&output, flagOutput, "o", outputTable, "output format: table or json")
	return cmd
}

func writeDeploy(out io.Writer, app string, urgent bool, r deployInfo) error {
	d := r.Deployment
	sha := shortSHA(d.Commit.SHA)
	line := fmt.Sprintf("deployment %d queued for %s: commit %s", d.ID, app, sha)
	if urgent {
		line += " (urgent)"
	}
	if !r.Created {
		line = fmt.Sprintf("deployment %d of commit %s is already %s for %s", d.ID, sha, d.State, app)
	}
	if d.Migration {
		line += "; flagged as a database migration"
		if d.MigrationReason != "" {
			line += ": " + d.MigrationReason
		}
	}
	_, err := fmt.Fprintln(out, line)
	return err
}

func newCancelCommand(lookupEnv LookupEnv) *cobra.Command {
	var server, reason, output string
	cmd := &cobra.Command{
		Use:   "cancel <deployment-id> [--reason <text>]",
		Short: "Cancel a queued or running deployment",
		Long: "Cancel a deployment that has not finished, deploy or rollback. A cancelled deployment stops at its next step and its pull request, if any, is closed. " +
			"A version that was already pinned in the GitOps repository stays pinned until a rollback or a newer deploy replaces it.",
		Example: "  doorstep cancel 42\n" +
			"  doorstep cancel 42 --reason \"wrong commit\"",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseDeploymentID(args[0])
			if err != nil {
				return err
			}
			if err := validateOutput(output); err != nil {
				return err
			}
			client, err := newAPIClient(cmd.Flags().Changed, server, lookupEnv)
			if err != nil {
				return err
			}
			var raw json.RawMessage
			path := "/api/v1/deployments/" + strconv.FormatInt(id, 10) + "/cancel"
			if err := client.do(cmd.Context(), http.MethodPost, path, cancelBody{Reason: strings.TrimSpace(reason)}, &raw); err != nil {
				return err
			}
			if output == outputJSON {
				return writeIndentedJSON(cmd.OutOrStdout(), raw)
			}
			var result cancelInfo
			if err := json.Unmarshal(raw, &result); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "deployment %d of %s %s\n", result.Deployment.ID, result.Deployment.App, result.Deployment.State)
			return err
		},
	}
	flags := cmd.Flags()
	addServerFlag(flags, &server)
	flags.StringVar(&reason, flagReason, "", "why the deployment is cancelled; recorded on the deployment and in the audit log")
	flags.StringVarP(&output, flagOutput, "o", outputTable, "output format: table or json")
	return cmd
}
