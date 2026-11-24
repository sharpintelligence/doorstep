package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sharpintelligence/doorstep/internal/config"
)

const flagConfig = "config"

func addConfigFlag(flags *pflag.FlagSet, dst *string) {
	flags.StringVar(dst, flagConfig, "",
		"path to the YAML configuration file (env "+config.EnvConfig+")")
}

func resolveConfigPath(changed func(name string) bool, flagValue string, lookupEnv LookupEnv) (string, error) {
	if changed(flagConfig) {
		if flagValue == "" {
			return "", fmt.Errorf("--%s: must not be empty", flagConfig)
		}
		return flagValue, nil
	}
	if v, ok := lookupEnv(config.EnvConfig); ok {
		if v == "" {
			return "", fmt.Errorf("%s: must not be empty", config.EnvConfig)
		}
		return v, nil
	}
	return "", nil
}

func loadConfigFile(changed func(name string) bool, flagValue string, lookupEnv LookupEnv) (config.File, error) {
	path, err := resolveConfigPath(changed, flagValue, lookupEnv)
	if err != nil {
		return config.File{}, err
	}
	return loadConfigFileAt(path)
}

func loadConfigFileAt(path string) (config.File, error) {
	if path == "" {
		return config.File{}, nil
	}
	return loadFile(path)
}

func loadFile(path string) (config.File, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		if pathErr, ok := errors.AsType[*fs.PathError](err); ok {
			err = pathErr.Err
		}
		return config.File{}, fmt.Errorf("config file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	file, err := config.Parse(f)
	if err != nil {
		return config.File{}, fmt.Errorf("config file %s: %w", path, err)
	}
	return file, nil
}

func newConfigCommand(lookupEnv LookupEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Work with the configuration file",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newConfigValidateCommand(lookupEnv))
	return cmd
}

func newConfigValidateCommand(lookupEnv LookupEnv) *cobra.Command {
	var configFlag string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Parse and validate the configuration file without resolving secrets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			catalog := file.Catalog()
			if err := errors.Join(file.Apply(config.Default()).ValidateSettings(), catalog.Validate()); err != nil {
				return fmt.Errorf("invalid configuration: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "ok: %d apps, %d notification targets\n",
				len(catalog.Apps), len(catalog.Notifications))
			return err
		},
	}
	addConfigFlag(cmd.Flags(), &configFlag)
	return cmd
}
