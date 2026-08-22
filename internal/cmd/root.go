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
		newRunCmd(&g),
		newDataSourceCmd(&g),
		// auth and profile take no globalFlags: auth login builds its client
		// from the URL and key it just prompted for, and profile only reads
		// and writes the config file, so neither resolves a connection.
		newAuthCmd(),
		newProfileCmd(),
	)
	return cmd
}

// interruptSignals lists the signals Execute cancels the command context on
// and then dies of. It is a function so tests can assert the registered set
// without a second copy of the list to keep in step.
func interruptSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

// Execute runs the root command and returns the process exit code. A run a
// signal ended reports nothing and dies of that signal instead, so on unix
// this usually does not return at all; what it returns there is only for
// the platforms and dispositions that cannot deliver one.
//
// SIGINT and SIGTERM cancel the command context so a running query can
// cancel its server-side job before the process goes, and so `auth login`
// abandons a prompt it is waiting on.
//
// An explicit channel rather than signal.NotifyContext, which can support
// neither half of that: its cancellation cause is an unexported type that
// is not an os.Signal, so the signal cannot be recovered to re-raise it,
// and it keeps the handler installed for the whole run, which swallows a
// second signal sent during a slow clean-up.
func Execute() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signals := interruptSignals()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, signals...)
	defer signal.Stop(ch)

	// The signal that arrived, if one did. Buffered so the watcher can
	// hand it over and finish whether or not anyone is left to read it.
	received := make(chan os.Signal, 1)
	go func() {
		sig := <-ch // os/signal never closes the channel

		// Resetting every registered signal rather than the one that
		// arrived is what makes a second signal end the process at once:
		// it meets the default disposition instead of this handler, so
		// there is no second code path to write. A SIGTERM following a
		// SIGINT would still be swallowed if only the latter were reset.
		//
		// It has to happen before the signal is handed on, not after: a
		// receive from received then guarantees this call has returned.
		// The other order leaves a window in which the re-raise lands in
		// the channel above, which nobody reads any more, and dieOfSignal
		// waits for a process death that never comes.
		signal.Reset(signals...)

		// Handing the signal on before cancelling, for the same kind of
		// reason: cancelling first would let the command unwind and the
		// receive below run while this send is still pending, and the run
		// would be reported as the failure the cancellation produced.
		received <- sig
		cancel()
	}()

	err := newRootCmd().ExecuteContext(ctx)

	// The signal is checked before the error is even looked at. A run the
	// user stopped did not fail, whatever error unwound it — a cancelled
	// context, an abandoned prompt, or a --timeout expiry whose clean-up
	// the signal interrupted — so none of them is reported or mapped to an
	// exit code. Discarding the error unread is also what keeps an
	// interrupted run from ever being called a timeout.
	select {
	case sig := <-received:
		return dieOfSignal(sig)
	default:
	}

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
