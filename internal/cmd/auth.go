package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

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
	var timeout timeoutFlag
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Register a Redash user API key as a named profile",
		Long: `Register a Redash user API key as a named profile.

The URL and key are verified against the server before anything is saved.
Redash has no browser OAuth flow, so key registration is the only method.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuthLogin(cmd, timeout.Duration())
		},
	}
	addTimeoutFlag(cmd, &timeout, "give up on verifying the URL and key after this duration (0 = no limit)")
	return cmd
}

func runAuthLogin(cmd *cobra.Command, timeout time.Duration) error {
	in := cmd.InOrStdin()
	errOut := cmd.ErrOrStderr()

	// The signal context, so a prompt gives up on Ctrl-C rather than waiting
	// for a keystroke the user is no longer going to type. --timeout is
	// derived from it around the verification alone and nowhere else: a
	// prompt that inherited that deadline would abandon a user who was still
	// typing, which is not what the flag is for.
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

	// Cancelled here rather than deferred: the data source prompt and the
	// config write below it are the rest of this function, and neither is
	// what --timeout bounds.
	verifyCtx, cancel := withTimeout(ctx, timeout)
	err = client.GetSession(verifyCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("verification failed, nothing saved: %w", timeoutOr(err, timeout, "the key check"))
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
// saved. Its job is to unwind runAuthLogin from whichever prompt was
// waiting; the text is for a reader of a stack of wrapped errors, not for
// the user, since Execute discards the error of a signalled run rather
// than reporting it.
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
// "^C", and nothing at all at the masked prompt — so without this whatever
// the shell writes next would continue the half-written prompt line.
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
	}
	// Nothing has ended the prompt line at this point either way:
	// ReadPassword leaves the newline the user typed unechoed, and on the
	// cancelled path ECHO is still cleared, so not even "^C" appeared.
	fmt.Fprintln(errOut)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(key)), nil
}
