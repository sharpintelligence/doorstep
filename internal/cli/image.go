package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sharpintelligence/doorstep/internal/buildinfo"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/registry"
)

func newImageCommand(lookupEnv LookupEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Inspect container images in OCI registries",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newImageResolveCommand(lookupEnv))
	return cmd
}

func newImageResolveCommand(lookupEnv LookupEnv) *cobra.Command {
	var credentialsFile string
	cmd := &cobra.Command{
		Use:   "resolve <repository> <tag>",
		Short: "Resolve an image tag to its digest",
		Long: "Resolve an image tag to the digest it currently points at, using the same registry client and credentials as the server. " +
			"A multi-architecture image resolves to its index digest.",
		Example: "  doorstep image resolve ghcr.io/owner/app 0123456789abcdef0123456789abcdef01234567",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := credentialsFile
			if !cmd.Flags().Changed(flagRegistryAuthFile) {
				path, _ = lookupEnv(config.EnvRegistryCredentialsFile)
			}
			opts := registry.Options{UserAgent: "doorstep/" + buildinfo.Read().Version}
			if path != "" {
				creds, err := registry.LoadDockerConfigFile(path)
				if err != nil {
					return fmt.Errorf("registry credentials: %w", err)
				}
				opts.Credentials = creds
			}
			digest, err := registry.New(opts).Resolve(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), digest)
			return err
		},
	}
	addRegistryCredentialsFlag(cmd.Flags(), &credentialsFile)
	return cmd
}

func addRegistryCredentialsFlag(flags *pflag.FlagSet, dst *string) {
	flags.StringVar(dst, flagRegistryAuthFile, "",
		"path to a Docker config.json with registry credentials; anonymous when empty (env "+config.EnvRegistryCredentialsFile+")")
}
