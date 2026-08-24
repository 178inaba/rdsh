// Package cmd implements the rdsh command-line interface.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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

// helpReport carries a failure the help function found back out to
// Execute. cobra hands a non-runnable command's arguments to the help
// function and then returns nil whatever that function did, so a stray
// argument caught there cannot come back as ExecuteContext's error and has
// to travel beside it. It is per-tree rather than a package-level var for
// the same reason globalFlags is.
type helpReport struct {
	// err is what the override below built, or nil if it never fired.
	// Execute prints it and maps it like any other failure.
	err error
}

func newRootCmd() (*cobra.Command, *helpReport) {
	var g globalFlags
	var report helpReport

	cmd := &cobra.Command{
		Use:   "rdsh",
		Short: "Run ad-hoc SQL and manage saved queries on Redash",
		// Errors are printed once in Execute; the default behavior would
		// print usage and the error again on every runtime failure, which
		// is noise for the primary (agent) consumer.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.PersistentFlags().StringVar(&g.profile, "profile", "", "profile to use for this invocation")

	// cobra answers an argument that is not one of a group command's
	// subcommands by printing that group's help to stdout and returning
	// nil: a success, on the stream results are read from. It gets there
	// through the `!Runnable()` branch of Command.execute, which returns
	// flag.ErrHelp before ValidateArgs is ever reached, so no Args on a
	// group can catch it — cobra's own completion command has NoArgs and
	// still exits 0. Overriding the help function is what catches it, and
	// one override here covers every group in the tree, including the
	// completion command cobra generates during Execute, because HelpFunc
	// walks to the parent when a command has none of its own.
	//
	// The default rendering has to be captured before the override is
	// installed: afterwards Help() resolves to the override, so the
	// fall-through would recurse.
	defaultHelp := cmd.HelpFunc()
	cmd.SetHelpFunc(func(c *cobra.Command, args []string) {
		// c.Flags().Args() rather than args, which is the whole command
		// line as the root received it. execute parses the flags before
		// it decides the command is not runnable, so this is populated by
		// the time the help function is reached.
		stray := c.Flags().Args()
		if help, _ := c.Flags().GetBool("help"); help || c.Runnable() || len(stray) == 0 {
			defaultHelp(c, args)
			return
		}
		report.err = unknownSubcommandError(c, stray[0])
	})

	cmd.AddCommand(
		newRunCmd(&g),
		newQueryCmd(&g),
		newDataSourceCmd(&g),
		// auth and profile take no globalFlags: auth login builds its client
		// from the URL and key it just prompted for, and profile only reads
		// and writes the config file, so neither resolves a connection.
		newAuthCmd(),
		newProfileCmd(),
	)
	return cmd, &report
}

// unknownSubcommandError reports an argument that is not one of cmd's
// subcommands, in the words and with the candidates cobra's own legacyArgs
// produces for the root — the only place cobra reports this itself.
func unknownSubcommandError(cmd *cobra.Command, arg string) error {
	var candidates []string
	if arg == "help" {
		// cobra registers a help command on the root alone, so under a
		// group `help` is as stray as anything else. SuggestionsFor only
		// ever looks at registered subcommands, so the flag that does
		// what the caller meant has to be named here.
		candidates = []string{"--help"}
	} else {
		// findSuggestions, which is what the root-level report goes
		// through, sets this default before consulting SuggestionsFor;
		// SuggestionsFor reads the distance as it finds it. Left at zero
		// every Levenshtein candidate is silently lost — `lsit` stops
		// suggesting `list` — and only prefix matches survive. Assigned
		// rather than defaulted because nothing else in rdsh sets it.
		cmd.SuggestionsMinimumDistance = 2
		candidates = cmd.SuggestionsFor(arg)
	}

	var suggestions strings.Builder
	if len(candidates) > 0 {
		suggestions.WriteString("\n\nDid you mean this?\n")
		for _, candidate := range candidates {
			fmt.Fprintf(&suggestions, "\t%v\n", candidate)
		}
	}
	// No usage listing after it, which is what gh's equivalent ends with:
	// the root is SilenceUsage precisely because usage on every failure is
	// noise for the agent consumer, and stopping here is also what the
	// root-level report prints today.
	return fmt.Errorf("unknown command %q for %q%s", arg, cmd.CommandPath(), suggestions.String())
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

	root, report := newRootCmd()
	err := root.ExecuteContext(ctx)

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

	// Only now, so an interrupt still wins: cobra returns nil for a stray
	// argument the help function caught, and this is the one thing a nil
	// return can still be hiding.
	if err == nil {
		err = report.err
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

// defaultTimeout bounds every command that talks to Redash unless
// --timeout says otherwise.
const defaultTimeout = 90 * time.Second

// errTimeout marks a run aborted by --timeout expiry; exitCode maps it to
// timeoutExitCode so agents can mechanically distinguish "retry with a
// longer --timeout" from other failures. Its own text names no operation:
// the commands that reach it do different things, so timeoutOr's caller
// says what timed out.
var errTimeout = errors.New("timed out")

// timeoutFlag is the value type every --timeout is registered with. A
// pflag.Value rather than DurationVar so a negative duration is refused
// while cobra parses the flags — the same place format.Format refuses a
// typo — which is both earlier than any guard in a RunE and one place
// rather than seven.
type timeoutFlag time.Duration

func (f timeoutFlag) Duration() time.Duration { return time.Duration(f) }

func (f timeoutFlag) String() string { return time.Duration(f).String() }

// Type names the value shown in the --timeout help line. It reports what
// DurationVar reports, since the help text is part of the agent-facing
// contract.
func (timeoutFlag) Type() string { return "duration" }

// Set reports only what is wrong with the value; pflag prefixes the flag
// and the argument it was given.
func (f *timeoutFlag) Set(s string) error {
	d, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if d < 0 {
		return fmt.Errorf("must not be negative (got %s)", d)
	}
	*f = timeoutFlag(d)
	return nil
}

// addTimeoutFlag registers --timeout on cmd. The name, the type and the
// default are one contract across every command that talks to the server;
// only the usage differs, because run is the one with a server-side job to
// cancel. The default is assigned before the flag is registered, which is
// what pflag reads the displayed default from.
func addTimeoutFlag(cmd *cobra.Command, timeout *timeoutFlag, usage string) {
	*timeout = timeoutFlag(defaultTimeout)
	cmd.Flags().Var(timeout, "timeout", usage)
}

// withTimeout derives the context the server calls run under. Zero means no
// limit, so the parent is returned as it is alongside a cancel func that
// does nothing, and callers need no branch of their own.
//
// Each caller derives it where its requests begin rather than at the top of
// its run function, so that reading SQL from stdin and auth login's prompts
// stay off the clock.
func withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// timeoutOr reports err as the timeout it is when the deadline is what
// stopped it, so the run exits timeoutExitCode rather than 1. operation
// names what was being done, and begins the message. Opting in per call
// site rather than wrapping on the way out is what leaves query create's
// publish step free to report its expiry as an ordinary failure.
func timeoutOr(err error, timeout time.Duration, operation string) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s %w after %s (use --timeout to allow more time): %v", operation, errTimeout, timeout, err)
	}
	return err
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
