package cmd

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/178inaba/rdsh/internal/format"
	"github.com/178inaba/rdsh/internal/redash"
)

func newQueryCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "query",
		Short: "Manage saved queries",
	}
	cmd.AddCommand(newQueryCreateCmd(g), newQueryUpdateCmd(g), newQueryListCmd(g), newQueryShowCmd(g))
	return cmd
}

func newQueryCreateCmd(g *globalFlags) *cobra.Command {
	var (
		name        string
		description string
		file        string
		dataSource  string
		draft       bool
		timeout     timeoutFlag
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
			return runQueryCreate(cmd, args, g, name, description, file, dataSource, draft, timeout.Duration())
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "name of the saved query (required)")
	cmd.Flags().StringVar(&description, "description", "", "description of the saved query")
	cmd.Flags().StringVarP(&file, "file", "f", "", "read SQL from a file")
	cmd.Flags().StringVar(&dataSource, "data-source", "", "data source ID or name (default: the profile's data source)")
	cmd.Flags().BoolVar(&draft, "draft", false, "leave the query a draft instead of publishing it")
	addTimeoutFlag(cmd, &timeout, "give up on the server after this duration (0 = no limit)")
	return cmd
}

func runQueryCreate(cmd *cobra.Command, args []string, g *globalFlags, name, description, file, dataSource string, draft bool, timeout time.Duration) error {
	// Checked before anything that talks to the server, so a bad invocation
	// cannot leave a query behind. --timeout needs no check of its own:
	// timeoutFlag refuses a negative duration as the flags are parsed.
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

	ctx, cancel := withTimeout(cmd.Context(), timeout)
	defer cancel()

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
		return timeoutOr(err, timeout, "query")
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
		timeout     timeoutFlag
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
			return runQueryUpdate(cmd, args, g, name, description, file, publish, draft, timeout.Duration())
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "rename the saved query")
	cmd.Flags().StringVar(&description, "description", "", "set the description (pass an empty string to clear it)")
	cmd.Flags().StringVarP(&file, "file", "f", "", "read the new SQL from a file")
	cmd.Flags().BoolVar(&publish, "publish", false, "publish the query, putting it in everyone's query list")
	cmd.Flags().BoolVar(&draft, "draft", false, "turn the query back into a draft, hiding it from other users")
	addTimeoutFlag(cmd, &timeout, "give up on the server after this duration (0 = no limit)")
	cmd.MarkFlagsMutuallyExclusive("publish", "draft")
	return cmd
}

func runQueryUpdate(cmd *cobra.Command, args []string, g *globalFlags, name, description, file string, publish, draft bool, timeout time.Duration) error {
	// The SQL argument counts as given by being there at all rather than by
	// being non-empty as it is in create, so that `update <id> "" -f file`
	// is the conflict it looks like instead of quietly using the file.
	// Nothing falls back to stdin either — see the command's help for why.
	arg, hasArg := "", len(args) == 2
	if hasArg {
		arg = args[1]
	}
	sql, hasSQL, err := sqlFromArgOrFile(arg, hasArg, file)
	if err != nil {
		return err
	}
	// An empty value is refused rather than sent, for both of the fields
	// where it would destroy something: a saved query with no SQL or no
	// name is of no use to anyone, and an unset shell variable is all it
	// takes to ask for one by accident. --description is the exception —
	// clearing it is a real edit, which is why it goes through as given.
	if hasSQL && sql == "" {
		return errors.New("the new SQL is empty; pass the SQL to save, or leave it out to keep the query's own")
	}
	if cmd.Flags().Changed("name") && name == "" {
		return errors.New("--name must not be empty")
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
	switch {
	case flags.Changed("publish"):
		isDraft := !publish
		u.IsDraft = &isDraft
	case flags.Changed("draft"):
		u.IsDraft = &draft
	}
	if u == (redash.QueryUpdate{}) {
		return errors.New("nothing to change; pass new SQL, --name, --description, --publish, or --draft")
	}

	conn, err := resolveConnection(g.profile)
	if err != nil {
		return err
	}
	client := redash.NewClient(conn.URL, conn.APIKey)
	id, err := resolveQueryID(args[0], client)
	if err != nil {
		return err
	}

	ctx, cancel := withTimeout(cmd.Context(), timeout)
	defer cancel()

	// Read first for the version the update sends back. Both calls report a
	// deadline as an ordinary timeout, unlike create: an update is a single
	// write, so whichever call the deadline cut off, the query is as it was
	// and re-running the command as it stands is safe.
	current, err := client.GetQuery(ctx, id)
	if err != nil {
		return timeoutOr(err, timeout, "query")
	}
	u.Version = &current.Version

	if _, err := client.UpdateQuery(ctx, id, u); err != nil {
		if errors.Is(err, redash.ErrQueryVersionConflict) {
			// Re-running this command unchanged would keep failing, so the
			// failure has to say what recovering from it takes.
			return fmt.Errorf("%w; read it again and re-run the update", err)
		}
		return timeoutOr(err, timeout, "query")
	}

	fmt.Fprintln(cmd.OutOrStdout(), client.QueryURL(id))
	return nil
}

// defaultQueryListLimit caps the listing unless --limit says otherwise. It
// is the same 30 gh uses: enough to find a query by eye, short enough that
// an agent reading stdout is not handed an unbounded page.
const defaultQueryListLimit = 30

func newQueryListCmd(g *globalFlags) *cobra.Command {
	var (
		mine         bool
		limit        int
		outputFormat = format.CSV
		timeout      timeoutFlag
	)
	cmd := &cobra.Command{
		Use:   "list [search]",
		Short: "List saved Redash queries and their IDs",
		Long: `List saved Redash queries and their IDs.

The columns are the query's ID, its name, whether it is still a draft, and
its URL. The ID is what a Query Results data source needs for a ` + "`query_<id>`" + `
reference, and what ` + "`rdsh query update`" + ` takes.

Without an argument the queries you can see are listed newest first. The
argument is a full-text search, which the server answers in its own
relevance order rather than by age.

Only the first 30 are printed unless --limit says otherwise; when there are
more, a note saying so goes to stderr, leaving stdout to the rows alone.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQueryList(cmd, args, g, mine, limit, outputFormat, timeout.Duration())
		},
	}
	cmd.Flags().BoolVar(&mine, "mine", false, "list only the queries you created")
	cmd.Flags().IntVar(&limit, "limit", defaultQueryListLimit, "maximum number of queries to print")
	cmd.Flags().Var(&outputFormat, "format", "output format: csv, tsv, or json")
	addTimeoutFlag(cmd, &timeout, "give up on the server after this duration (0 = no limit)")
	return cmd
}

func runQueryList(cmd *cobra.Command, args []string, g *globalFlags, mine bool, limit int,
	outputFormat format.Format, timeout time.Duration) error {
	// The client documents this as a precondition rather than enforcing it,
	// the way every other method there leaves argument checking to this
	// layer. Checking it here names the flag the caller actually passed,
	// and nothing is sent: the server would refuse the page size a smaller
	// limit asks for anyway.
	if limit < 1 {
		return fmt.Errorf("--limit must be at least 1 (got %d)", limit)
	}

	// An empty argument counts as no search, the way an empty one counts as
	// no SQL in run and create. Listing everything is what the server does
	// with an empty q in any case, and nothing is lost by it.
	search := ""
	if len(args) == 1 {
		search = args[0]
	}

	conn, err := resolveConnection(g.profile)
	if err != nil {
		return err
	}
	client := redash.NewClient(conn.URL, conn.APIKey)

	ctx, cancel := withTimeout(cmd.Context(), timeout)
	defer cancel()

	// A listing reads nothing into existence, so a deadline that cuts it off
	// leaves the server as it was and re-running is safe — which is what
	// exit code 124 tells an agent to do.
	queries, total, err := client.ListQueries(ctx, redash.QueryListOptions{
		Search: search,
		Mine:   mine,
		Limit:  limit,
	})
	if err != nil {
		return timeoutOr(err, timeout, "query")
	}

	if err := format.Write(cmd.OutOrStdout(), outputFormat, queryListResult(client, queries)); err != nil {
		return err
	}
	if total > len(queries) {
		// A note rather than a failure: the rows are what was asked for. It
		// goes to stderr so stdout stays parseable as the listing alone.
		fmt.Fprintf(cmd.ErrOrStderr(),
			"showing %d of %d matching queries; raise --limit or narrow the search to see the rest\n",
			len(queries), total)
	}
	return nil
}

// queryListResult shapes the listing as the row set internal/format
// renders, so list shares one output contract with run rather than growing
// a second renderer. The URL is built by the client, which is also what
// reads one back, so the two directions cannot drift apart.
func queryListResult(client *redash.Client, queries []redash.Query) *redash.QueryResult {
	result := &redash.QueryResult{
		Columns: []redash.Column{{Name: "id"}, {Name: "name"}, {Name: "is_draft"}, {Name: "url"}},
		Rows:    make([]map[string]any, len(queries)),
	}
	for i, q := range queries {
		result.Rows[i] = map[string]any{
			"id":       q.ID,
			"name":     q.Name,
			"is_draft": q.IsDraft,
			"url":      client.QueryURL(q.ID),
		}
	}
	return result
}

// showFormat is the --format value `rdsh query show` accepts. The shared
// format.Format is deliberately not reused: its zero value is csv, and the
// row formats it offers cannot carry a multi-line SQL body. This one has no
// name for the default output, so leaving --format out is what asks for the
// SQL itself.
//
// pflag.Value, like format.Format, so an unsupported value is refused while
// cobra parses the flags rather than after the query has been read.
type showFormat string

// showFormatJSON is the only value --format takes on show.
const showFormatJSON showFormat = "json"

func (f showFormat) String() string { return string(f) }

func (f *showFormat) Set(s string) error {
	if showFormat(s) != showFormatJSON {
		return fmt.Errorf("unsupported format %q (the only one is %s; without --format the SQL itself is printed)",
			s, showFormatJSON)
	}
	*f = showFormatJSON
	return nil
}

// Type names the value shown in the --format help line, for the same reason
// format.Format reports "string": the help text is part of the agent-facing
// contract.
func (showFormat) Type() string { return "string" }

func newQueryShowCmd(g *globalFlags) *cobra.Command {
	var (
		outputFormat showFormat
		timeout      timeoutFlag
	)
	cmd := &cobra.Command{
		Use:   "show <id|url>",
		Short: "Print a saved Redash query's SQL",
		Long: `Print a saved Redash query's SQL.

The query is named by its ID or by its URL on the configured Redash
instance. What is printed is the query's own SQL and nothing else, so

    rdsh query show <id|url> > q.sql
    rdsh run -f q.sql

is how a query someone shared is run for a fresh result of its own.

--format json prints one object carrying the query's metadata alongside the
SQL: id, name, description, data_source_id, is_draft, url and query. json is
the only value it takes — a multi-line SQL body does not fit a row format.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQueryShow(cmd, args, g, outputFormat, timeout.Duration())
		},
	}
	cmd.Flags().Var(&outputFormat, "format", "output format: json (default: the SQL itself)")
	addTimeoutFlag(cmd, &timeout, "give up on the server after this duration (0 = no limit)")
	return cmd
}

func runQueryShow(cmd *cobra.Command, args []string, g *globalFlags, outputFormat showFormat,
	timeout time.Duration) error {
	// No pre-flight check of its own: --format and --timeout are both
	// validated as the flags are parsed.

	conn, err := resolveConnection(g.profile)
	if err != nil {
		return err
	}
	client := redash.NewClient(conn.URL, conn.APIKey)
	id, err := resolveQueryID(args[0], client)
	if err != nil {
		return err
	}

	ctx, cancel := withTimeout(cmd.Context(), timeout)
	defer cancel()

	// A read leaves the server as it was, so a deadline that cuts it off is
	// an ordinary timeout: re-running the command as it stands is safe,
	// which is what exit code 124 tells an agent to do.
	q, err := client.GetQuery(ctx, id)
	if err != nil {
		return timeoutOr(err, timeout, "query")
	}

	out := cmd.OutOrStdout()
	if outputFormat == showFormatJSON {
		return writeQueryDetail(out, q, client.QueryURL(q.ID))
	}
	// Exactly one trailing newline whether or not the stored SQL carries
	// any, so that the redirected output is the same file either way.
	fmt.Fprintln(out, strings.TrimRight(q.Query, "\n"))
	return nil
}

// writeQueryDetail prints the saved query as one JSON object. The shape is
// this command's own — format.Write renders the row sets run and the listing
// share — but the encoding goes through internal/format like every other
// JSON stream rdsh prints. The SQL is printed as it is stored; normalising
// the trailing newline is a property of the raw stream above, not of the
// query.
func writeQueryDetail(w io.Writer, q *redash.Query, url string) error {
	return format.WriteObject(w, struct {
		ID           int    `json:"id"`
		Name         string `json:"name"`
		Description  string `json:"description"`
		DataSourceID int    `json:"data_source_id"`
		IsDraft      bool   `json:"is_draft"`
		URL          string `json:"url"`
		Query        string `json:"query"`
	}{
		ID:           q.ID,
		Name:         q.Name,
		Description:  q.Description,
		DataSourceID: q.DataSourceID,
		IsDraft:      q.IsDraft,
		URL:          url,
		Query:        q.Query,
	})
}

// resolveQueryID reads the <id|url> argument the saved-query commands take:
// an all-digit value is an ID, anything else has to be a query URL on the
// instance the command is talking to. What a browser adds on the way to the
// clipboard — a trailing segment such as /source, a query string, a
// fragment — is tolerated.
//
// The prefix comes from the client so that reading a URL and printing one
// stay the same fact. It runs through /queries/ rather than stopping at the
// base URL, so a host that merely starts the same way, such as
// redash.example.com.evil.test, cannot pass as the configured instance and
// send the update somewhere else.
func resolveQueryID(arg string, client *redash.Client) (int, error) {
	if isDigits(arg) {
		return strconv.Atoi(arg)
	}
	prefix := client.QueryURLPrefix()
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
