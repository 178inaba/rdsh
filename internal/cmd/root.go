// Package cmd implements the rdsh command-line interface.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/178inaba/rdsh/internal/config"
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

// resolveConnection loads the config and applies the profile/env
// precedence. The --profile persistent flag is defined on the root command.
func resolveConnection(cmd *cobra.Command) (config.Connection, error) {
	profile, err := cmd.Flags().GetString("profile")
	if err != nil {
		return config.Connection{}, err
	}
	cfg, err := config.Load()
	if err != nil {
		return config.Connection{}, err
	}
	return config.Resolve(cfg, profile)
}

// terminalFile reports the underlying *os.File of a command input stream
// and whether it is a real terminal. Non-*os.File readers report
// (nil, false); tests use them as scriptable stdin.
func terminalFile(in io.Reader) (*os.File, bool) {
	f, ok := in.(*os.File)
	if !ok {
		return nil, false
	}
	return f, term.IsTerminal(int(f.Fd()))
}
