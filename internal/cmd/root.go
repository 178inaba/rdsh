// Package cmd implements the rdsh command-line interface.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// timeoutExitCode follows the GNU timeout convention.
const timeoutExitCode = 124

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rdsh",
		Short: "Run ad-hoc SQL on Redash from the command line",
		// Errors are printed once in Execute; the default behavior would
		// print usage and the error again on every runtime failure, which
		// is noise for the primary (agent) consumer.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.PersistentFlags().String("profile", "", "profile to use for this invocation")
	cmd.AddCommand(newQueryCmd(), newAuthCmd(), newProfileCmd(), newDataSourceCmd())
	return cmd
}

// Execute runs the root command and returns the process exit code. SIGINT
// and SIGTERM cancel the command context so a running query can cancel its
// server-side job before the process exits.
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := newRootCmd().ExecuteContext(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
	}
	return exitCode(err)
}

func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errTimeout):
		return timeoutExitCode
	default:
		return 1
	}
}
