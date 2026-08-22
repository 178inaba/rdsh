package cmd

import (
	"context"
	"errors"
	"fmt"
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
	cmd.AddCommand(newQueryCreateCmd(g))
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
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w after %s (use --timeout to allow more time): %v", errTimeout, timeout, err)
		}
		return err
	}

	url := client.QueryURL(q.ID)
	if !draft {
		// Redash saves every new query as a draft, so publishing is this
		// second call rather than a field on the create.
		draft := false
		if _, err := client.UpdateQuery(ctx, q.ID, redash.QueryUpdate{IsDraft: &draft}); err != nil {
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
