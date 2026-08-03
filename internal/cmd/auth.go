package cmd

import (
	"bufio"
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

	// The masked key prompt needs a real terminal. Non-TTY callers (agents,
	// CI) are pointed at the env pair instead; a non-*os.File reader is the
	// test seam and reads answers line by line.
	stdinFile, isTTY := terminalFile(in)
	if stdinFile != nil && !isTTY {
		return errors.New("auth login is interactive and needs a terminal; set RDSH_URL and RDSH_API_KEY instead")
	}

	reader := bufio.NewReader(in)

	url, err := promptLine(reader, errOut, "Redash URL: ")
	if err != nil {
		return err
	}
	url = strings.TrimRight(url, "/")
	if url == "" {
		return errors.New("URL must not be empty")
	}

	key, err := promptAPIKey(reader, errOut, stdinFile)
	if err != nil {
		return err
	}
	if key == "" {
		return errors.New("API key must not be empty")
	}

	name, err := promptLine(reader, errOut, "Profile name [default]: ")
	if err != nil {
		return err
	}
	if name == "" {
		name = "default"
	}

	client := redash.NewClient(url, key)
	if err := client.GetSession(cmd.Context()); err != nil {
		return fmt.Errorf("verification failed, nothing saved: %w", err)
	}

	dataSource, err := promptLine(reader, errOut, "Default data source (ID or name, optional): ")
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.SetProfile(name, config.Profile{URL: url, APIKey: key, DataSource: dataSource})
	if err := cfg.Save(); err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Saved profile %q\n", name)
	return nil
}

// promptLine writes the prompt to stderr (keeping stdout clean for data)
// and reads one trimmed line.
func promptLine(reader *bufio.Reader, errOut io.Writer, prompt string) (string, error) {
	fmt.Fprint(errOut, prompt)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// promptAPIKey masks the key on a real terminal (a non-nil stdinFile is
// guaranteed to be a TTY by the guard in runAuthLogin); the test seam falls
// back to a plain line read.
func promptAPIKey(reader *bufio.Reader, errOut io.Writer, stdinFile *os.File) (string, error) {
	if stdinFile == nil {
		return promptLine(reader, errOut, "API key: ")
	}
	fmt.Fprint(errOut, "API key: ")
	key, err := term.ReadPassword(int(stdinFile.Fd()))
	fmt.Fprintln(errOut)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(key)), nil
}
