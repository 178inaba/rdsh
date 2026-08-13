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

// globalFlags carries the root persistent flag values into each command's
// RunE. newRootCmd binds them and hands the same pointer to every
// constructor that needs them, so no command or flag state lives at package
// level. The values are only final after flag parsing, so a RunE closure
// must read the fields when it runs rather than copy them at construction.
type globalFlags struct {
	profile string
}

func newRootCmd() *cobra.Command {
	var g globalFlags

	cmd := &cobra.Command{
		Use:   "rdsh",
		Short: "Run ad-hoc SQL on Redash from the command line",
		// Errors are printed once in Execute; the default behavior would
		// print usage and the error again on every runtime failure, which
		// is noise for the primary (agent) consumer.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.PersistentFlags().StringVar(&g.profile, "profile", "", "profile to use for this invocation")
	cmd.AddCommand(
		newQueryCmd(&g),
		newDataSourceCmd(&g),
		// auth and profile take no globalFlags: auth login builds its client
		// from the URL and key it just prompted for, and profile only reads
		// and writes the config file, so neither resolves a connection.
		newAuthCmd(),
		newProfileCmd(),
	)
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
// precedence. profile is the root --profile persistent flag value.
func resolveConnection(profile string) (config.Connection, error) {
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
