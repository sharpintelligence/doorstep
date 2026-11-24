package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

type LookupEnv func(string) (string, bool)

func Run(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv LookupEnv) int {
	root := newRootCommand(stdout, stderr, lookupEnv)
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "doorstep: %v\n", err)
		return 1
	}
	return 0
}

func newRootCommand(stdout, stderr io.Writer, lookupEnv LookupEnv) *cobra.Command {
	root := &cobra.Command{
		Use:           "doorstep",
		Short:         "Deployment engine for Argo CD",
		Long:          "doorstep queues and locks deploys, writes image digests to your GitOps repo, gates on health and metrics, and rolls back.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.AddCommand(
		newVersionCommand(),
		newServeCommand(lookupEnv),
		newMigrateCommand(lookupEnv),
		newTokenCommand(lookupEnv, osUsername),
		newSessionCommand(lookupEnv, osUsername),
		newImageCommand(lookupEnv),
		newConfigCommand(lookupEnv),
		newLockCommand(lookupEnv),
		newUnlockCommand(lookupEnv),
		newLocksCommand(lookupEnv),
		newDeployCommand(lookupEnv),
		newCancelCommand(lookupEnv),
		newDeploymentsCommand(lookupEnv),
		newDeploymentCommand(lookupEnv),
		newRollbackCommand(lookupEnv),
		newBlockCommand(lookupEnv),
		newUnblockCommand(lookupEnv),
		newBlocksCommand(lookupEnv),
		newNotifyCommand(lookupEnv),
		newArgoCDCommand(lookupEnv),
	)
	return root
}
