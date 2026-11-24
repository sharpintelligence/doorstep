package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/user"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/logging"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const (
	flagActor = "actor"
	envActor  = "DOORSTEP_ACTOR"
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

func cliRequester(actorFlag string, lookupEnv LookupEnv, osUser string) (auth.Requester, error) {
	requester := auth.Requester{Actor: cliActor(osUser)}
	if osUser != "" {
		requester.Origin = map[string]string{"os_user": osUser}
	}
	name := actorFlag
	source := "--" + flagActor
	if name == "" {
		name, _ = lookupEnv(envActor)
		source = envActor
	}
	if name == "" {
		return requester, nil
	}
	actor := core.Actor{Kind: core.ActorCLI, ID: name, Name: name}
	if err := actor.Validate(); err != nil {
		return auth.Requester{}, fmt.Errorf("%s: %w", source, err)
	}
	requester.Actor = actor
	return requester, nil
}

type adminCommand struct {
	lookupEnv   LookupEnv
	currentUser func() string
	overrides   config.Config
	configFlag  string
	actorFlag   string
}

func (a *adminCommand) bindFlags(flags *pflag.FlagSet) {
	bindDatabaseCommandFlags(flags, &a.configFlag, &a.overrides)
	flags.StringVar(&a.actorFlag, flagActor, "",
		"who is running the command, recorded in the audit log as cli:<actor> (env "+envActor+"; default the operating system user)")
}

func (a *adminCommand) requester() (auth.Requester, error) {
	return cliRequester(a.actorFlag, a.lookupEnv, a.currentUser())
}

func (a *adminCommand) withRepository(cmd *cobra.Command, fn func(ctx context.Context, repo auth.Repository, logger *slog.Logger) error) error {
	cfg, err := resolveDatabaseCommandConfig(cmd.Flags().Changed, a.overrides, a.configFlag, a.lookupEnv)
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
	return fn(ctx, auth.NewRepository(st), logger)
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
