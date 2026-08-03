// Package cmd implements the rdsh command-line interface.
package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

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
	return cmd
}

// Execute runs the root command and returns the process exit code.
func Execute() int {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
	return 0
}
