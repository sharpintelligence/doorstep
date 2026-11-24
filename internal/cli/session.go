package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/store"
)

func newSessionCommand(lookupEnv LookupEnv, currentUser func() string) *cobra.Command {
	admin := &adminCommand{lookupEnv: lookupEnv, currentUser: currentUser}
	cmd := &cobra.Command{
		Use:   "session",
		Short: "List and revoke sign-in sessions",
		Long:  "List and revoke the browser sessions created by sign-in. These commands talk to the database directly and record revocations in the audit log.",
		Args:  cobra.NoArgs,
	}
	admin.bindFlags(cmd.PersistentFlags())

	withSessions := func(cmd *cobra.Command, fn func(ctx context.Context, sessions *auth.Sessions) error) error {
		return admin.withRepository(cmd, func(ctx context.Context, repo auth.Repository, _ *slog.Logger) error {
			return fn(ctx, auth.NewSessions(repo))
		})
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List active sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withSessions(cmd, func(ctx context.Context, sessions *auth.Sessions) error {
				list, err := sessions.List(ctx)
				if err != nil {
					return err
				}
				return printSessions(cmd.OutOrStdout(), list)
			})
		},
	}

	var user string
	var all bool
	revoke := &cobra.Command{
		Use:   "revoke (--user USER | --all)",
		Short: "End one user's sessions, or everyone's, immediately",
		Long:  "End sessions immediately. --user matches a session's username case-insensitively, its subject, or its actor ID (provider:subject). The user must sign in again, and gets the role the current configuration maps them to.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (user == "") == !all {
				return errors.New("set exactly one of --user and --all")
			}
			by, err := admin.requester()
			if err != nil {
				return err
			}
			return withSessions(cmd, func(ctx context.Context, sessions *auth.Sessions) error {
				n, err := sessions.Revoke(ctx, auth.RevokeSessionsInput{User: user, All: all, By: by})
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.ErrOrStderr(), "revoked %d %s\n", n, plural(n, "session", "sessions"))
				return err
			})
		},
	}
	revoke.Flags().StringVar(&user, "user", "", "username, subject or provider:subject whose sessions to end")
	revoke.Flags().BoolVar(&all, "all", false, "end every session, signing everyone out")

	cmd.AddCommand(list, revoke)
	return cmd
}

func printSessions(out io.Writer, sessions []store.Session) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ACTOR\tUSERNAME\tROLE\tCREATED\tEXPIRES")
	for _, s := range sessions {
		actor := auth.UserActor(s.Provider, s.Subject, s.Username)
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			actor, actor.Name, s.Role, formatTime(&s.CreatedAt, "-"), formatTime(&s.ExpiresAt, "-"))
	}
	return w.Flush()
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
