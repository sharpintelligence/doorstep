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
)

const (
	flagGlobal = "global"
	flagReason = "reason"
	flagFor    = "for"
	flagUntil  = "until"
	flagOutput = "output"

	outputTable = "table"
	outputJSON  = "json"
)

type lockBody struct {
	Reason    string  `json:"reason"`
	ExpiresIn *string `json:"expires_in,omitempty"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

type lockInfo struct {
	Scope     string     `json:"scope"`
	App       string     `json:"app,omitempty"`
	Owner     string     `json:"owner"`
	Reason    string     `json:"reason"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type lockList struct {
	Locks []lockInfo `json:"locks"`
}

func (l lockInfo) name() string {
	if l.Scope == "global" {
		return "global"
	}
	return l.App
}

func lockPath(app string, global bool) string {
	if global {
		return "/api/v1/lock"
	}
	return "/api/v1/apps/" + app + "/lock"
}

func lockScopeArg(args []string, global bool) (string, error) {
	switch {
	case global && len(args) > 0:
		return "", fmt.Errorf("give either an app or --%s, not both", flagGlobal)
	case global:
		return "", nil
	case len(args) == 0:
		return "", fmt.Errorf("an app name or --%s is required", flagGlobal)
	}
	if err := config.ValidateAppName(args[0]); err != nil {
		return "", fmt.Errorf("app name: %w", err)
	}
	return args[0], nil
}

func newLockCommand(lookupEnv LookupEnv) *cobra.Command {
	var server, reason, until string
	var global bool
	var duration time.Duration
	cmd := &cobra.Command{
		Use:   "lock [<app>] --reason <reason>",
		Short: "Freeze deploys of an app, or of every app with --global",
		Long: "Freeze deploys of an app, or of every app with --global. New deploys keep queueing but do not start until the lock is released or expires. " +
			"Rollbacks and deploys already in progress are not stopped. The lock owner is the identity of the API token.",
		Example: "  doorstep lock blog --reason \"database migration\" --for 2h\n" +
			"  doorstep lock --global --reason \"holiday freeze\" --until 2027-01-02T09:00:00Z",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := lockScopeArg(args, global)
			if err != nil {
				return err
			}
			body := lockBody{Reason: reason}
			changed := cmd.Flags().Changed
			if changed(flagFor) && changed(flagUntil) {
				return fmt.Errorf("give at most one of --%s and --%s", flagFor, flagUntil)
			}
			if changed(flagFor) {
				if duration <= 0 {
					return fmt.Errorf("--%s: must be positive, got %s", flagFor, duration)
				}
				s := duration.String()
				body.ExpiresIn = &s
			}
			if changed(flagUntil) {
				at, err := time.Parse(time.RFC3339, until)
				if err != nil {
					return fmt.Errorf("--%s: must be an RFC 3339 time such as 2027-01-02T09:00:00Z", flagUntil)
				}
				s := at.Format(time.RFC3339)
				body.ExpiresAt = &s
			}
			client, err := newAPIClient(changed, server, lookupEnv)
			if err != nil {
				return err
			}
			var lock lockInfo
			if err := client.do(cmd.Context(), http.MethodPost, lockPath(app, global), body, &lock); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "locked %s by %s %s: %s\n", lock.name(), lock.Owner, expiryText(lock.ExpiresAt), lock.Reason)
			return err
		},
	}
	flags := cmd.Flags()
	addServerFlag(flags, &server)
	flags.BoolVar(&global, flagGlobal, false, "lock every app")
	flags.StringVar(&reason, flagReason, "", "why deploys are frozen (required)")
	flags.DurationVar(&duration, flagFor, 0, "release the lock automatically after this long, such as 2h")
	flags.StringVar(&until, flagUntil, "", "release the lock automatically at this RFC 3339 time")
	_ = cmd.MarkFlagRequired(flagReason)
	return cmd
}

func newUnlockCommand(lookupEnv LookupEnv) *cobra.Command {
	var server string
	var global bool
	cmd := &cobra.Command{
		Use:     "unlock [<app>]",
		Short:   "Release the freeze on an app, or the global freeze with --global",
		Long:    "Release the freeze on an app, or the global freeze with --global. Anyone with API access may release any lock; the release is recorded with their identity.",
		Example: "  doorstep unlock blog\n  doorstep unlock --global",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := lockScopeArg(args, global)
			if err != nil {
				return err
			}
			client, err := newAPIClient(cmd.Flags().Changed, server, lookupEnv)
			if err != nil {
				return err
			}
			if err := client.do(cmd.Context(), http.MethodDelete, lockPath(app, global), nil, nil); err != nil {
				return err
			}
			name := app
			if global {
				name = "global"
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "unlocked %s\n", name)
			return err
		},
	}
	addServerFlag(cmd.Flags(), &server)
	cmd.Flags().BoolVar(&global, flagGlobal, false, "release the global freeze")
	return cmd
}

func newLocksCommand(lookupEnv LookupEnv) *cobra.Command {
	var server, output string
	cmd := &cobra.Command{
		Use:   "locks",
		Short: "List active freezes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if output != outputTable && output != outputJSON {
				return fmt.Errorf("--%s: must be %s or %s, got %q", flagOutput, outputTable, outputJSON, output)
			}
			client, err := newAPIClient(cmd.Flags().Changed, server, lookupEnv)
			if err != nil {
				return err
			}
			var list lockList
			if err := client.do(cmd.Context(), http.MethodGet, "/api/v1/locks", nil, &list); err != nil {
				return err
			}
			if list.Locks == nil {
				list.Locks = []lockInfo{}
			}
			if output == outputJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(list)
			}
			return writeLockTable(cmd.OutOrStdout(), list.Locks)
		},
	}
	addServerFlag(cmd.Flags(), &server)
	cmd.Flags().StringVarP(&output, flagOutput, "o", outputTable, "output format: table or json")
	return cmd
}

func writeLockTable(out io.Writer, locks []lockInfo) error {
	if len(locks) == 0 {
		_, err := fmt.Fprintln(out, "no locks")
		return err
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SCOPE\tOWNER\tSINCE\tEXPIRES\tREASON")
	for _, l := range locks {
		expires := "-"
		if l.ExpiresAt != nil {
			expires = l.ExpiresAt.UTC().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", l.name(), l.Owner, l.CreatedAt.UTC().Format(time.RFC3339), expires, l.Reason)
	}
	return w.Flush()
}

func expiryText(at *time.Time) string {
	if at == nil {
		return "until released"
	}
	return "until " + at.UTC().Format(time.RFC3339)
}
