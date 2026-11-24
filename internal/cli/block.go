package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
)

type blockBody struct {
	Digest    string `json:"digest,omitempty"`
	CommitSHA string `json:"commit_sha,omitempty"`
	Reason    string `json:"reason"`
}

type blockList struct {
	Blocks []blockInfo `json:"blocks"`
}

func blockArgs(args []string) (string, core.BlockRef, error) {
	if err := config.ValidateAppName(args[0]); err != nil {
		return "", core.BlockRef{}, fmt.Errorf("app name: %w", err)
	}
	ref, err := core.ParseBlockRef(args[1])
	if err != nil {
		return "", core.BlockRef{}, err
	}
	return args[0], ref, nil
}

func blocksPath(app string) string {
	return "/api/v1/apps/" + app + "/blocks"
}

func newBlockCommand(lookupEnv LookupEnv) *cobra.Command {
	var server, reason string
	cmd := &cobra.Command{
		Use:   "block <app> <digest|commit> --reason <reason>",
		Short: "Stop a version of an app from being deployed automatically",
		Long: "Stop a version of an app from being deployed automatically. A digest block stops deploys that resolve to that image digest; " +
			"a commit block stops the commit from being queued. Rollbacks are never blocked. doorstep rollback blocks the version it replaces on its own.",
		Example: "  doorstep block blog sha256:4f1c... --reason \"leaks memory\"\n" +
			"  doorstep block blog 0123456789abcdef0123456789abcdef01234567 --reason \"bad migration\"",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, ref, err := blockArgs(args)
			if err != nil {
				return err
			}
			client, err := newAPIClient(cmd.Flags().Changed, server, lookupEnv)
			if err != nil {
				return err
			}
			body := blockBody{Digest: string(ref.Digest), CommitSHA: ref.CommitSHA, Reason: reason}
			var block blockInfo
			if err := client.do(cmd.Context(), http.MethodPost, blocksPath(app), body, &block); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "blocked %s for %s by %s: %s\n", block.subject(), app, block.CreatedBy, block.Reason)
			return err
		},
	}
	addServerFlag(cmd.Flags(), &server)
	cmd.Flags().StringVar(&reason, flagReason, "", "why the version must not be deployed (required)")
	_ = cmd.MarkFlagRequired(flagReason)
	return cmd
}

func newUnblockCommand(lookupEnv LookupEnv) *cobra.Command {
	var server string
	cmd := &cobra.Command{
		Use:   "unblock <app> <digest|commit>",
		Short: "Allow a blocked version of an app to be deployed again",
		Long: "Allow a blocked version of an app to be deployed again. Giving a commit clears every block of that commit. " +
			"A commit that is still the branch head and green is queued by the next check event or poll.",
		Example: "  doorstep unblock blog sha256:4f1c...\n  doorstep unblock blog 0123456789abcdef0123456789abcdef01234567",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, ref, err := blockArgs(args)
			if err != nil {
				return err
			}
			client, err := newAPIClient(cmd.Flags().Changed, server, lookupEnv)
			if err != nil {
				return err
			}
			var cleared blockList
			if err := client.do(cmd.Context(), http.MethodDelete, blocksPath(app)+"/"+ref.String(), nil, &cleared); err != nil {
				return err
			}
			for _, b := range cleared.Blocks {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "unblocked %s for %s\n", b.subject(), app); err != nil {
					return err
				}
			}
			return nil
		},
	}
	addServerFlag(cmd.Flags(), &server)
	return cmd
}

func newBlocksCommand(lookupEnv LookupEnv) *cobra.Command {
	var server, output string
	cmd := &cobra.Command{
		Use:   "blocks <app>",
		Short: "List the blocked versions of an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]
			if err := config.ValidateAppName(app); err != nil {
				return fmt.Errorf("app name: %w", err)
			}
			if err := validateOutput(output); err != nil {
				return err
			}
			client, err := newAPIClient(cmd.Flags().Changed, server, lookupEnv)
			if err != nil {
				return err
			}
			var raw json.RawMessage
			if err := client.do(cmd.Context(), http.MethodGet, blocksPath(app), nil, &raw); err != nil {
				return err
			}
			if output == outputJSON {
				return writeIndentedJSON(cmd.OutOrStdout(), raw)
			}
			var list blockList
			if err := json.Unmarshal(raw, &list); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			return writeBlockTable(cmd.OutOrStdout(), app, list.Blocks)
		},
	}
	addServerFlag(cmd.Flags(), &server)
	cmd.Flags().StringVarP(&output, flagOutput, "o", outputTable, "output format: table or json")
	return cmd
}

func writeBlockTable(out io.Writer, app string, blocks []blockInfo) error {
	if len(blocks) == 0 {
		_, err := fmt.Fprintf(out, "no blocks for %s\n", app)
		return err
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tDIGEST\tCOMMIT\tBY\tSINCE\tIN GIT\tREASON")
	for _, b := range blocks {
		digest, commit, inGit := "-", "-", "no"
		if b.Digest != "" {
			digest = shortDigest(b.Digest)
		}
		if b.Commit != nil {
			commit = b.Commit.SHA[:min(len(b.Commit.SHA), 12)]
		}
		if b.RecordedInGit {
			inGit = "yes"
		}
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			b.ID, digest, commit, b.CreatedBy, b.CreatedAt.UTC().Format(time.RFC3339), inGit, b.Reason)
	}
	return w.Flush()
}
