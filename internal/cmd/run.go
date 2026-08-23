package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/178inaba/rdsh/internal/format"
	"github.com/178inaba/rdsh/internal/redash"
)

// errTimeout marks a query aborted by --timeout expiry; Execute maps it to
// exit code 124 (GNU timeout convention) so agents can mechanically
// distinguish "retry with a longer --timeout" from other failures.
var errTimeout = errors.New("query timed out")

const defaultTimeout = 90 * time.Second

// timeoutOr reports err as the timeout it is when the deadline is what
// stopped it, so the run exits 124 rather than 1. Opting in per call site
// rather than wrapping on the way out is what leaves query create's publish
// step free to report its expiry as an ordinary failure.
func timeoutOr(err error, timeout time.Duration) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w after %s (use --timeout to allow more time): %v", errTimeout, timeout, err)
	}
	return err
}

func newRunCmd(g *globalFlags) *cobra.Command {
	var (
		file         string
		dataSource   string
		outputFormat = format.CSV
		timeout      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "run [sql]",
		Short: "Run ad-hoc SQL on Redash and print the result",
		Long: `Run ad-hoc SQL on Redash and print the result.

SQL is taken from the argument, from --file, or from stdin when neither is
given. The result cache is always bypassed.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRun(cmd, args, g, file, dataSource, outputFormat, timeout)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "read SQL from a file")
	cmd.Flags().StringVar(&dataSource, "data-source", "", "data source ID or name (default: the profile's data source)")
	cmd.Flags().Var(&outputFormat, "format", "output format: csv, tsv, or json")
	cmd.Flags().DurationVar(&timeout, "timeout", defaultTimeout, "cancel the query after this duration (0 = no limit)")
	return cmd
}

func runRun(cmd *cobra.Command, args []string, g *globalFlags, file, dataSource string, outputFormat format.Format, timeout time.Duration) error {
	// Fail on a bad --timeout before anything expensive, so it does not let
	// the query run to completion on the server first. --format needs no
	// check here: format.Format rejects a typo during flag parsing.
	if timeout < 0 {
		return fmt.Errorf("--timeout must not be negative (got %s)", timeout)
	}

	sql, err := readSQL(cmd, args, file)
	if err != nil {
		return err
	}

	conn, err := resolveConnection(g.profile)
	if err != nil {
		return err
	}
	client := redash.NewClient(conn.URL, conn.APIKey)

	ctx := cmd.Context()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	dsID, err := resolveDataSource(ctx, client, dataSource, conn.DataSource)
	if err != nil {
		return err
	}

	result, err := client.RunQuery(ctx, sql, dsID)
	if err != nil {
		return timeoutOr(err, timeout)
	}

	return format.Write(cmd.OutOrStdout(), outputFormat, result)
}

// readSQL picks the SQL source by priority: the argument and --file
// conflict when both are given; stdin is read only when neither is present
// and stdin is not a TTY. A bare "is stdin non-TTY" signal cannot count as
// a provided input because agent harnesses and go test always run with
// non-TTY stdin.
func readSQL(cmd *cobra.Command, args []string, file string) (string, error) {
	arg := ""
	if len(args) == 1 {
		arg = args[0]
	}
	sql, ok, err := sqlFromArgOrFile(arg, arg != "", file)
	if err != nil {
		return "", err
	}
	if ok {
		return sql, nil
	}

	in := cmd.InOrStdin()
	if _, isTTY := terminalFile(in); isTTY {
		return "", errors.New("no SQL provided; pass it as an argument, via -f, or on stdin")
	}
	data, err := io.ReadAll(in)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	sql = strings.TrimSpace(string(data))
	if sql == "" {
		return "", errors.New("no SQL provided on stdin; pass it as an argument, via -f, or on stdin")
	}
	return sql, nil
}

// sqlFromArgOrFile resolves the two SQL sources every command that takes
// SQL shares, and reports whether either supplied it. hasArg is passed
// separately because the commands disagree on what counts as an argument:
// an empty one is no SQL at all where SQL is required, and an explicit
// empty query where it is optional.
func sqlFromArgOrFile(arg string, hasArg bool, file string) (string, bool, error) {
	switch {
	case hasArg && file != "":
		return "", false, errors.New("SQL was given both as an argument and via -f; use only one")
	case hasArg:
		return arg, true, nil
	case file != "":
		data, err := os.ReadFile(file)
		if err != nil {
			return "", false, err
		}
		return string(data), true, nil
	}
	return "", false, nil
}

// resolveDataSource returns the data source ID to query. The flag overrides
// the profile default; an all-digit value is an ID, anything else is
// resolved as an exact name match via the API.
func resolveDataSource(ctx context.Context, client *redash.Client, flag, profileDefault string) (int, error) {
	value := flag
	if value == "" {
		value = profileDefault
	}
	if value == "" {
		return 0, errors.New("no data source configured; pass --data-source (ID or name)")
	}
	if isDigits(value) {
		return strconv.Atoi(value)
	}
	dss, err := client.ListDataSources(ctx)
	if err != nil {
		return 0, err
	}
	for _, ds := range dss {
		if ds.Name == value {
			return ds.ID, nil
		}
	}
	return 0, fmt.Errorf("data source %q not found; run `rdsh data-source list` to see available data sources", value)
}

// isDigits deliberately rejects sign prefixes: only an all-digit value is
// treated as an ID, so a data source named e.g. "-3" still goes through
// name lookup (strconv.Atoi alone would accept it as an ID).
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
