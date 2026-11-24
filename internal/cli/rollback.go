package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sharpintelligence/doorstep/internal/config"
)

const (
	flagTo                       = "to"
	flagConfirmMigrationRollback = "confirm-migration-rollback"
)

type rollbackBody struct {
	ToDeploymentID           *int64 `json:"to_deployment_id,omitempty"`
	Reason                   string `json:"reason,omitempty"`
	ConfirmMigrationRollback bool   `json:"confirm_migration_rollback,omitempty"`
}

type blockCommitInfo struct {
	Repository string `json:"repository"`
	SHA        string `json:"sha"`
}

type blockInfo struct {
	ID            int64            `json:"id"`
	App           string           `json:"app"`
	Digest        string           `json:"digest"`
	Commit        *blockCommitInfo `json:"commit"`
	Reason        string           `json:"reason"`
	CreatedBy     string           `json:"created_by"`
	CreatedAt     time.Time        `json:"created_at"`
	RecordedInGit bool             `json:"recorded_in_git"`
}

type rollbackInfo struct {
	Deployment deploymentInfo   `json:"deployment"`
	RolledBack deploymentInfo   `json:"rolled_back"`
	Block      *blockInfo       `json:"block"`
	Cancelled  []int64          `json:"cancelled"`
	Migrations []deploymentInfo `json:"migrations"`
}

func (b blockInfo) subject() string {
	switch {
	case b.Digest != "" && b.Commit != nil:
		return b.Digest + " (commit " + b.Commit.SHA + ")"
	case b.Digest != "":
		return b.Digest
	case b.Commit != nil:
		return "commit " + b.Commit.SHA
	default:
		return "block " + strconv.FormatInt(b.ID, 10)
	}
}

func newRollbackCommand(lookupEnv LookupEnv) *cobra.Command {
	var server, reason, output string
	var to int64
	var confirmMigration bool
	cmd := &cobra.Command{
		Use:   "rollback <app> [--to <deployment-id>] [--reason <text>] [--confirm-migration-rollback]",
		Short: "Roll an app back to its previous good version",
		Long: "Roll an app back to its previous good version. doorstep queues a rollback deployment ahead of every other deploy, " +
			"which re-pins the digest of the last successful deployment through the normal deploy path, cancels the deploy in flight, " +
			"and blocks the version it replaces so it is not deployed again automatically. Use --to to pick an earlier successful deployment. " +
			"Remove the block with doorstep unblock. A rollback that would undo a release flagged as containing database migrations " +
			"is refused until you confirm it with --confirm-migration-rollback, since the old version may not work with the migrated schema.",
		Example: "  doorstep rollback blog\n" +
			"  doorstep rollback blog --to 41 --reason \"checkout errors\"\n" +
			"  doorstep rollback blog --to 41 --confirm-migration-rollback",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]
			if err := config.ValidateAppName(app); err != nil {
				return fmt.Errorf("app name: %w", err)
			}
			if err := validateOutput(output); err != nil {
				return err
			}
			body := rollbackBody{Reason: strings.TrimSpace(reason), ConfirmMigrationRollback: confirmMigration}
			if cmd.Flags().Changed(flagTo) {
				if to <= 0 {
					return fmt.Errorf("--%s: must be a positive deployment id, got %d", flagTo, to)
				}
				body.ToDeploymentID = &to
			}
			client, err := newAPIClient(cmd.Flags().Changed, server, lookupEnv)
			if err != nil {
				return err
			}
			var raw json.RawMessage
			if err := client.do(cmd.Context(), http.MethodPost, "/api/v1/apps/"+app+"/rollback", body, &raw); err != nil {
				return err
			}
			if output == outputJSON {
				return writeIndentedJSON(cmd.OutOrStdout(), raw)
			}
			var result rollbackInfo
			if err := json.Unmarshal(raw, &result); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			return writeRollback(cmd.OutOrStdout(), app, result)
		},
	}
	flags := cmd.Flags()
	addServerFlag(flags, &server)
	flags.Int64Var(&to, flagTo, 0, "roll back to this successful deployment instead of the previous one")
	flags.StringVar(&reason, flagReason, "", "why the version is rolled back; recorded on the block")
	flags.BoolVar(&confirmMigration, flagConfirmMigrationRollback, false, "confirm a rollback across releases flagged as containing database migrations")
	flags.StringVarP(&output, flagOutput, "o", outputTable, "output format: table or json")
	return cmd
}

func writeRollback(out io.Writer, app string, r rollbackInfo) error {
	line := fmt.Sprintf("rollback %d queued for %s: %s@%s (reverting deployment %d)",
		r.Deployment.ID, app, r.Deployment.Image.Repository, shortDigest(r.Deployment.Image.Digest), r.RolledBack.ID)
	if r.Block != nil {
		line += "; blocked " + r.Block.subject()
	}
	if len(r.Cancelled) > 0 {
		ids := make([]string, 0, len(r.Cancelled))
		for _, id := range r.Cancelled {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		line += "; cancelled " + strings.Join(ids, ", ")
	}
	if len(r.Migrations) > 0 {
		ids := make([]string, 0, len(r.Migrations))
		for _, d := range r.Migrations {
			ids = append(ids, strconv.FormatInt(d.ID, 10))
		}
		word := "deployment "
		if len(ids) > 1 {
			word = "deployments "
		}
		line += "; confirmed rollback across migrations in " + word + strings.Join(ids, ", ")
	}
	_, err := fmt.Fprintln(out, line)
	return err
}
