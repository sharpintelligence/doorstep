package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/logging"
	"github.com/sharpintelligence/doorstep/internal/store"
)

func newMigrateCommand(lookupEnv LookupEnv) *cobra.Command {
	var overrides config.Config
	var configFlag string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending database migrations",
		Long:  "Apply pending database migrations and print each applied version. Migrations are forward-only; serve also applies them on start.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveDatabaseCommandConfig(cmd.Flags().Changed, overrides, configFlag, lookupEnv)
			if err != nil {
				return err
			}
			return migrate(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), cfg)
		},
	}
	bindDatabaseCommandFlags(cmd.PersistentFlags(), &configFlag, &overrides)

	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Print applied and pending database migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveDatabaseCommandConfig(cmd.Flags().Changed, overrides, configFlag, lookupEnv)
			if err != nil {
				return err
			}
			return migrationStatus(cmd.Context(), cmd.OutOrStdout(), cfg)
		},
	})
	return cmd
}

func bindDatabaseCommandFlags(flags *pflag.FlagSet, configFlag *string, overrides *config.Config) {
	addConfigFlag(flags, configFlag)
	bindLogFlags(flags, overrides)
	bindDatabaseFlags(flags, overrides)
}

func resolveDatabaseCommandConfig(changed func(name string) bool, overrides config.Config, configFlag string, lookupEnv LookupEnv) (config.Config, error) {
	file, err := loadConfigFile(changed, configFlag, lookupEnv)
	if err != nil {
		return config.Config{}, err
	}
	cfg, err := resolveConfig(changed, overrides, file, lookupEnv)
	if err != nil {
		return config.Config{}, err
	}
	if err := cfg.Log.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	if err := cfg.Database.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("invalid configuration: database: %w", err)
	}
	return cfg, nil
}

func migrate(ctx context.Context, stdout, logOutput io.Writer, cfg config.Config) error {
	logger, err := logging.New(logOutput, cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		return err
	}
	st, err := openStore(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer st.Close()

	applied, err := st.Migrate(ctx, logger)
	for _, m := range applied {
		if _, werr := fmt.Fprintf(stdout, "applied %d %s (%s)\n", m.Version, m.Name, m.Duration.Round(time.Millisecond)); werr != nil {
			return werr
		}
	}
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if len(applied) == 0 {
		_, err = fmt.Fprintln(stdout, "no pending migrations")
	}
	return err
}

func migrationStatus(ctx context.Context, stdout io.Writer, cfg config.Config) error {
	st, err := openStore(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer st.Close()

	states, err := st.MigrationStatus(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "VERSION\tSTATE\tAPPLIED AT\tNAME")
	for _, m := range states {
		state, appliedAt := "pending", "-"
		if m.Applied {
			state, appliedAt = "applied", m.AppliedAt.UTC().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", m.Version, state, appliedAt, m.Name)
	}
	return w.Flush()
}

func migrationVersions(applied []store.AppliedMigration) []int64 {
	versions := make([]int64, 0, len(applied))
	for _, m := range applied {
		versions = append(versions, m.Version)
	}
	return versions
}
