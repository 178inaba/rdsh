package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/178inaba/rdsh/internal/redash"
)

func newQueryCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "query",
		Short: "Manage saved queries",
		// A group command is not runnable, and cobra answers an argument
		// that is not one of its subcommands by printing help to stdout and
		// returning nil — a success, on the stream results are read from.
		// A RunE makes the group runnable, which is what gets cobra as far
		// as validating the arguments; NoArgs then reports the stray one.
		// #27 fixes the same hole for every group at the root, and this can
		// go when it lands.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newQueryCreateCmd(g), newQueryUpdateCmd(g))
	return cmd
}

func newQueryCreateCmd(g *globalFlags) *cobra.Command {
	var (
		name        string
		description string
		file        string
		dataSource  string
		draft       bool
		timeout     time.Duration
	)
	cmd := &cobra.Command{
		Use:   "create --name <name> [sql]",
		Short: "Save SQL as a Redash query and print its URL",
		Long: `Save SQL as a Redash query and print its URL.

SQL is taken from the argument, from --file, or from stdin when neither is
given. Run the same SQL with ` + "`rdsh run`" + ` first and Redash attaches that
result to the new query, so the URL opens with data already on it.

The query is published unless --draft is given.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQueryCreate(cmd, args, g, name, description, file, dataSource, draft, timeout)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "name of the saved query (required)")
	cmd.Flags().StringVar(&description, "description", "", "description of the saved query")
	cmd.Flags().StringVarP(&file, "file", "f", "", "read SQL from a file")
	cmd.Flags().StringVar(&dataSource, "data-source", "", "data source ID or name (default: the profile's data source)")
	cmd.Flags().BoolVar(&draft, "draft", false, "leave the query a draft instead of publishing it")
	cmd.Flags().DurationVar(&timeout, "timeout", defaultTimeout, "give up on the server after this duration (0 = no limit)")
	return cmd
}

func runQueryCreate(cmd *cobra.Command, args []string, g *globalFlags, name, description, file, dataSource string, draft bool, timeout time.Duration) error {
	// Both checks come before anything that talks to the server, so a bad
	// invocation cannot leave a query behind.
	if timeout < 0 {
		return fmt.Errorf("--timeout must not be negative (got %s)", timeout)
	}
	if name == "" {
		return errors.New("--name is required")
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

	// A create the deadline cuts off is an ordinary timeout: either the
	// query was never saved, or it was and its ID never arrived, so there
	// is nothing to report but the expiry.
	q, err := client.CreateQuery(ctx, redash.NewQuery{
		Name:         name,
		Query:        sql,
		DataSourceID: dsID,
		Description:  description,
	})
	if err != nil {
		return timeoutOr(err, timeout)
	}

	url := client.QueryURL(q.ID)
	if !draft {
		// Redash saves every new query as a draft, so publishing is this
		// second call rather than a field on the create.
		isDraft := false
		if _, err := client.UpdateQuery(ctx, q.ID, redash.QueryUpdate{IsDraft: &isDraft}); err != nil {
			// Deliberately not reported as a timeout even when the deadline
			// is what stopped it: 124 tells an agent to re-run the command
			// as it stands, and a second create saves a second query. The
			// URL is what recovering from this needs instead.
			return fmt.Errorf("created %s but publishing it failed, so the query remains a draft: %w", url, err)
		}
	}

	fmt.Fprintln(cmd.OutOrStdout(), url)
	return nil
}

func newQueryUpdateCmd(g *globalFlags) *cobra.Command {
	var (
		name        string
		description string
		file        string
		publish     bool
		draft       bool
		timeout     time.Duration
	)
	cmd := &cobra.Command{
		Use:   "update <id|url> [sql]",
		Short: "Edit a saved Redash query and print its URL",
		Long: `Edit a saved Redash query and print its URL.

The query is named by its ID or by its URL on the configured Redash
instance. At least one change must be given, or the command fails without
sending anything.

New SQL is taken from the argument or from --file. Unlike ` + "`rdsh run`" + ` and
` + "`rdsh query create`" + `, stdin is never read: SQL is optional here, so falling
back to it would turn whatever a caller happened to pipe in into the query.

The update carries the version the query had when it was read, so an edit
made in the Redash UI in the meantime fails the command instead of being
overwritten.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQueryUpdate(cmd, args, g, name, description, file, publish, draft, timeout)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "rename the saved query")
	cmd.Flags().StringVar(&description, "description", "", "set the description (pass an empty string to clear it)")
	cmd.Flags().StringVarP(&file, "file", "f", "", "read the new SQL from a file")
	cmd.Flags().BoolVar(&publish, "publish", false, "publish the query, putting it in everyone's query list")
	cmd.Flags().BoolVar(&draft, "draft", false, "turn the query back into a draft, hiding it from other users")
	cmd.Flags().DurationVar(&timeout, "timeout", defaultTimeout, "give up on the server after this duration (0 = no limit)")
	cmd.MarkFlagsMutuallyExclusive("publish", "draft")
	return cmd
}

func runQueryUpdate(cmd *cobra.Command, args []string, g *globalFlags, name, description, file string, publish, draft bool, timeout time.Duration) error {
	if timeout < 0 {
		return fmt.Errorf("--timeout must not be negative (got %s)", timeout)
	}

	// The SQL argument is recognised by being there at all, not by being
	// non-empty as it is in create: SQL is optional here, so an empty one
	// has to stay distinguishable from none at all rather than silently
	// falling through to --file.
	sql, hasSQL := "", len(args) == 2
	switch {
	case hasSQL && file != "":
		return errors.New("SQL was given both as an argument and via -f; use only one")
	case hasSQL:
		sql = args[1]
	case file != "":
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		sql, hasSQL = string(data), true
	}

	// Which fields to send comes from whether the flag was given rather
	// than from its value, so that --description "" clears the description
	// while leaving it out keeps whatever the query has.
	flags := cmd.Flags()
	var u redash.QueryUpdate
	if flags.Changed("name") {
		u.Name = &name
	}
	if flags.Changed("description") {
		u.Description = &description
	}
	if hasSQL {
		u.Query = &sql
	}
	if flags.Changed("publish") || flags.Changed("draft") {
		// The two flags are mutually exclusive, so at most one of them
		// decided this; --publish means the opposite of a draft.
		isDraft := draft
		if flags.Changed("publish") {
			isDraft = !publish
		}
		u.IsDraft = &isDraft
	}
	if u == (redash.QueryUpdate{}) {
		return errors.New("nothing to change; pass new SQL, --name, --description, --publish, or --draft")
	}

	conn, err := resolveConnection(g.profile)
	if err != nil {
		return err
	}
	id, err := resolveQueryID(args[0], conn.URL)
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

	// Read first for the version the update sends back. Both calls report a
	// deadline as an ordinary timeout, unlike create: an update is a single
	// write, so whichever call the deadline cut off, the query is as it was
	// and re-running the command as it stands is safe.
	current, err := client.GetQuery(ctx, id)
	if err != nil {
		return timeoutOr(err, timeout)
	}
	u.Version = &current.Version

	if _, err := client.UpdateQuery(ctx, id, u); err != nil {
		if errors.Is(err, redash.ErrQueryVersionConflict) {
			// Re-running this command unchanged would keep failing, so the
			// failure has to say what recovering from it takes.
			return fmt.Errorf("%w; read it again and re-run the update", err)
		}
		return timeoutOr(err, timeout)
	}

	fmt.Fprintln(cmd.OutOrStdout(), client.QueryURL(id))
	return nil
}

// resolveQueryID reads the <id|url> argument the saved-query commands take:
// an all-digit value is an ID, anything else has to be a query URL on the
// instance the command is talking to. What a browser adds on the way to the
// clipboard — a trailing segment such as /source, a query string, a
// fragment — is tolerated.
//
// The prefix match runs through /queries/ rather than stopping at the base
// URL, so a host that merely starts the same way, such as
// redash.example.com.evil.test, cannot pass as the configured instance and
// send the update somewhere else.
func resolveQueryID(arg, baseURL string) (int, error) {
	if isDigits(arg) {
		return strconv.Atoi(arg)
	}
	prefix := strings.TrimRight(baseURL, "/") + "/queries/"
	rest, ok := strings.CutPrefix(arg, prefix)
	if !ok {
		return 0, fmt.Errorf("%q is neither a query ID nor a URL beginning %s", arg, prefix)
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if !isDigits(rest) {
		return 0, fmt.Errorf("%q carries no query ID after %s", arg, prefix)
	}
	return strconv.Atoi(rest)
}

// timeoutOr reports err as the timeout it is when the deadline is what
// stopped it, so the run exits 124 rather than 1. Opting in per call site
// rather than wrapping on the way out is what leaves create's publish step
// free to report its expiry as an ordinary failure. #28 folds this and
// run's identical wrapping into one helper.
func timeoutOr(err error, timeout time.Duration) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w after %s (use --timeout to allow more time): %v", errTimeout, timeout, err)
	}
	return err
}
