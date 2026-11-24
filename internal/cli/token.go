package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/user"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/logging"
	"github.com/sharpintelligence/doorstep/internal/store"
)

var errSchemaBehind = errors.New("database schema is not up to date; run doorstep migrate first")

func osUsername() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}

func cliActor(username string) core.Actor {
	actor := core.Actor{Kind: core.ActorCLI, ID: username, Name: username}
	if username == "" || actor.Validate() != nil {
		return core.Actor{Kind: core.ActorCLI, ID: "unknown", Name: "unknown"}
	}
	return actor
}

func newTokenCommand(lookupEnv LookupEnv, currentUser func() string) *cobra.Command {
	var overrides config.Config
	var configFlag string
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Create, list and revoke API tokens",
		Long:  "Create, list and revoke API tokens for the HTTP API. These commands talk to the database directly and record who ran them in the audit log.",
		Args:  cobra.NoArgs,
	}
	bindDatabaseCommandFlags(cmd.PersistentFlags(), &configFlag, &overrides)

	withTokens := func(cmd *cobra.Command, fn func(ctx context.Context, tokens *auth.Tokens) error) error {
		cfg, err := resolveDatabaseCommandConfig(cmd.Flags().Changed, overrides, configFlag, lookupEnv)
		if err != nil {
			return err
		}
		logger, err := logging.New(cmd.ErrOrStderr(), cfg.Log.Level, cfg.Log.Format)
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		st, err := openStore(ctx, cfg.Database)
		if err != nil {
			return err
		}
		defer st.Close()
		if err := requireCurrentSchema(ctx, st); err != nil {
			return err
		}
		return fn(ctx, auth.NewTokens(auth.NewRepository(st), logger))
	}

	var name, role string
	var expiresIn time.Duration
	create := &cobra.Command{
		Use:   "create",
		Short: "Create an API token and print it once",
		Long:  "Create an API token. The token is printed once on stdout and only its hash is stored; details go to stderr.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			parsed, err := core.ParseRole(role)
			if err != nil {
				return fmt.Errorf("--role: %w (want %s)", err, joinRoles())
			}
			if err := auth.ValidateTokenName(name); err != nil {
				return fmt.Errorf("--name: %w", err)
			}
			if expiresIn < 0 {
				return fmt.Errorf("--expires-in: must not be negative, got %s", expiresIn)
			}
			return withTokens(cmd, func(ctx context.Context, tokens *auth.Tokens) error {
				raw, created, err := tokens.Create(ctx, auth.CreateTokenInput{Name: name, Role: parsed, ExpiresIn: expiresIn, By: cliActor(currentUser())})
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), raw); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.ErrOrStderr(), "created token %s with role %s, expires %s\nstore it now: it is not shown again\n",
					created.Name, created.Role, formatTime(created.ExpiresAt, "never"))
				return err
			})
		},
	}
	create.Flags().StringVar(&name, "name", "", "unique token name: lowercase letters, digits, '.', '_' and '-' (required)")
	create.Flags().StringVar(&role, "role", "", "role granted to the token: "+joinRoles()+" (required)")
	create.Flags().DurationVar(&expiresIn, "expires-in", 0, "lifetime such as 720h; 0 never expires")
	_ = create.MarkFlagRequired("name")
	_ = create.MarkFlagRequired("role")

	list := &cobra.Command{
		Use:   "list",
		Short: "List API tokens, including revoked and expired ones",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withTokens(cmd, func(ctx context.Context, tokens *auth.Tokens) error {
				list, err := tokens.List(ctx)
				if err != nil {
					return err
				}
				return printTokens(cmd.OutOrStdout(), list)
			})
		},
	}

	revoke := &cobra.Command{
		Use:   "revoke NAME",
		Short: "Revoke an API token immediately",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withTokens(cmd, func(ctx context.Context, tokens *auth.Tokens) error {
				if err := tokens.Revoke(ctx, args[0], cliActor(currentUser())); err != nil {
					if errors.Is(err, core.ErrNotFound) {
						return fmt.Errorf("no active token named %q", args[0])
					}
					return err
				}
				_, err := fmt.Fprintf(cmd.ErrOrStderr(), "revoked token %s\n", args[0])
				return err
			})
		},
	}

	cmd.AddCommand(create, list, revoke)
	return cmd
}

func requireCurrentSchema(ctx context.Context, st *store.Store) error {
	states, err := st.MigrationStatus(ctx)
	if err != nil {
		return err
	}
	for _, m := range states {
		if !m.Applied {
			return errSchemaBehind
		}
	}
	return nil
}

func printTokens(out io.Writer, tokens []store.APIToken) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tROLE\tCREATED\tEXPIRES\tLAST USED\tSTATE")
	for _, t := range tokens {
		state := "active"
		switch {
		case t.RevokedAt != nil:
			state = "revoked"
		case t.Expired:
			state = "expired"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			t.Name, t.Role, formatTime(&t.CreatedAt, "-"), formatTime(t.ExpiresAt, "never"), formatTime(t.LastUsedAt, "never"), state)
	}
	return w.Flush()
}

func formatTime(t *time.Time, missing string) string {
	if t == nil {
		return missing
	}
	return t.UTC().Format(time.RFC3339)
}

func joinRoles() string {
	names := make([]string, 0, len(core.Roles()))
	for _, r := range core.Roles() {
		names = append(names, string(r))
	}
	return strings.Join(names, ", ")
}
