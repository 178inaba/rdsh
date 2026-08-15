package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/178inaba/rdsh/internal/config"
	"github.com/178inaba/rdsh/internal/redash"
)

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage authentication",
	}
	cmd.AddCommand(newAuthLoginCmd())
	return cmd
}

func newAuthLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Register a Redash user API key as a named profile",
		Long: `Register a Redash user API key as a named profile.

The URL and key are verified against the server before anything is saved.
Redash has no browser OAuth flow, so key registration is the only method.`,
		Args: cobra.NoArgs,
		RunE: runAuthLogin,
	}
}

func runAuthLogin(cmd *cobra.Command, _ []string) error {
	in := cmd.InOrStdin()
	errOut := cmd.ErrOrStderr()

	// The signal context, so a prompt gives up on Ctrl-C rather than waiting
	// for a keystroke the user is no longer going to type. auth login takes
	// no globalFlags and builds its own client, so there is no
	// --timeout-derived context here that the prompts could pick up by
	// mistake.
	ctx := cmd.Context()

	// The masked key prompt needs a real terminal. Non-TTY callers (agents,
	// CI) are pointed at the env pair instead; a non-*os.File reader is the
	// test seam and reads answers line by line.
	stdinFile, isTTY := terminalFile(in)
	if stdinFile != nil && !isTTY {
		return errors.New("auth login is interactive and needs a terminal; set RDSH_URL and RDSH_API_KEY instead")
	}

	reader := bufio.NewReader(in)

	url, err := promptLine(ctx, reader, errOut, "Redash URL: ")
	if err != nil {
		return err
	}
	url = strings.TrimRight(url, "/")
	if url == "" {
		return errors.New("URL must not be empty")
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("URL %q must start with http:// or https://", url)
	}

	key, err := promptAPIKey(ctx, reader, errOut, stdinFile)
	if err != nil {
		return err
	}
	if key == "" {
		return errors.New("API key must not be empty")
	}

	name, err := promptLine(ctx, reader, errOut, "Profile name [default]: ")
	if err != nil {
		return err
	}
	if name == "" {
		name = "default"
	}

	client := redash.NewClient(url, key)
	if err := client.GetSession(ctx); err != nil {
		return fmt.Errorf("verification failed, nothing saved: %w", err)
	}

	dataSource, err := promptLine(ctx, reader, errOut, "Default data source (ID or name, optional): ")
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.SetProfile(name, config.Profile{URL: url, APIKey: key, DataSource: dataSource})
	// A signal can still land between the last prompt and the write. The
	// prompts aborting is the primary mechanism; this closes what is left of
	// the window.
	if ctx.Err() != nil {
		return errInterrupted
	}
	if err := cfg.Save(); err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Saved profile %q\n", name)
	return nil
}

// errInterrupted reports a run that a signal ended before anything was
// saved. rdsh classifies no other context error, so this message has to
// read as an interruption on its own: Execute prints it as it is and
// exitCode falls through to 1.
var errInterrupted = errors.New("interrupted, nothing saved")

// readCancellable runs a blocking read and gives up on it as soon as ctx is
// done, so a signal ends the command instead of waiting for the keystroke
// the user is no longer going to type.
//
// The read runs in a goroutine because one that has already blocked cannot
// be unblocked without closing the descriptor. On the cancel branch that
// goroutine stays parked in the read for the rest of the process's life,
// which is short by then: the channel is buffered so its send does not
// block once nobody is receiving, and no later prompt can race it because
// every caller returns as soon as this one is cancelled.
func readCancellable[T any](ctx context.Context, read func() (T, error)) (T, error) {
	type result struct {
		value T
		err   error
	}

	done := make(chan result, 1)
	go func() {
		value, err := read()
		done <- result{value: value, err: err}
	}()

	select {
	case <-ctx.Done():
		var zero T
		return zero, errInterrupted
	case res := <-done:
		return res.value, res.err
	}
}

// readLine reads one trimmed line. A cancelled read also ends the prompt
// line: the interrupt does not end it itself — the terminal echoes at most
// "^C" — so without this the error Execute prints would continue the
// prompt rather than start its own line.
//
// The cancellation is checked before the io.EOF case below, which reports a
// prompt the input simply ran out on as an empty answer; letting the
// sentinel through there would make the interrupt invisible at exactly the
// prompts it has to end.
func readLine(ctx context.Context, reader *bufio.Reader, errOut io.Writer) (string, error) {
	line, err := readCancellable(ctx, func() (string, error) {
		return reader.ReadString('\n')
	})
	if errors.Is(err, errInterrupted) {
		fmt.Fprintln(errOut)
		return "", err
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// promptLine writes the prompt to stderr (keeping stdout clean for data)
// and reads one trimmed line.
func promptLine(ctx context.Context, reader *bufio.Reader, errOut io.Writer, prompt string) (string, error) {
	fmt.Fprint(errOut, prompt)
	return readLine(ctx, reader, errOut)
}

// promptAPIKey masks the key on a real terminal (a non-nil stdinFile is
// guaranteed to be a TTY by the guard in runAuthLogin); the test seam falls
// back to a plain line read.
func promptAPIKey(ctx context.Context, reader *bufio.Reader, errOut io.Writer, stdinFile *os.File) (string, error) {
	fmt.Fprint(errOut, "API key: ")
	if stdinFile == nil {
		return readLine(ctx, reader, errOut)
	}

	// ReadPassword clears ECHO and restores the terminal from its own
	// deferred call, which only runs once its read returns — never, on the
	// cancelled path below. Capturing the state here is what lets this
	// function put echo back instead of leaving the user's shell silent.
	fd := int(stdinFile.Fd())
	state, err := term.GetState(fd)
	if err != nil {
		return "", err
	}

	key, err := readCancellable(ctx, func() ([]byte, error) {
		return term.ReadPassword(fd)
	})
	if errors.Is(err, errInterrupted) {
		if restoreErr := term.Restore(fd, state); restoreErr != nil {
			return "", restoreErr
		}
		// With ECHO cleared not even "^C" appears, so end the prompt line
		// here as readLine does on the same path.
		fmt.Fprintln(errOut)
		return "", err
	}
	// ReadPassword leaves the newline the user typed unechoed, so emit one
	// to keep whatever comes next off the prompt line.
	fmt.Fprintln(errOut)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(key)), nil
}
