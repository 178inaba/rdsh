package cmd

import (
	"encoding/json"
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
	cmd.AddCommand(newQueryCreateCmd(g), newQueryUpdateCmd(g), newQueryListCmd(g),
		newQueryShowCmd(g), newQueryRefreshCmd(g))
	return cmd
}

func newQueryCreateCmd(g *globalFlags) *cobra.Command {
	var (
		name        string
		description string
		file        string
		dataSource  string
		draft       bool
		params      paramFlagValues
		timeout     timeoutFlag
	)
	cmd := &cobra.Command{
		Use:   "create --name <name> [sql]",
		Short: "Save SQL as a Redash query and print its URL",
		Long: `Save SQL as a Redash query and print its URL.

SQL is taken from the argument, from --file, or from stdin when neither is
given. Run the same SQL with ` + "`rdsh run`" + ` first and Redash attaches that
result to the new query, so the URL opens with data already on it.

The query is published unless --draft is given.

A parameter the SQL uses ({{name}}) needs a stored default, or the query
page stays empty for everyone else: Redash matches a result to a query by
the hash of its SQL with the defaults substituted, so a parameter without
one can never link a result. --param-default name=value declares the
parameter and sets its default; --param-type name=type gives it a type,
which is text unless said otherwise. The types are text, text-pattern,
number, date, datetime-local and datetime-with-seconds. A text-pattern
parameter also needs --param-regex name=pattern, which implies that type on
its own. All three are repeatable and split at the first =, so a value or a
pattern may contain one.

Every parameter the SQL uses has to be covered, and defining any of them is
a one-way move: once a query carries definitions, executing it with a name
that is not among them is refused. Range, enum and dropdown-query parameters
are the Redash UI's to define.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQueryCreate(cmd, args, g, name, description, file, dataSource, draft, params, timeout.Duration())
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "name of the saved query (required)")
	cmd.Flags().StringVar(&description, "description", "", "description of the saved query")
	cmd.Flags().StringVarP(&file, "file", "f", "", "read SQL from a file")
	cmd.Flags().StringVar(&dataSource, "data-source", "", "data source ID or name (default: the profile's data source)")
	cmd.Flags().BoolVar(&draft, "draft", false, "leave the query a draft instead of publishing it")
	addParamDefinitionFlags(cmd, &params)
	addTimeoutFlag(cmd, &timeout, serverTimeoutUsage)
	return cmd
}

// addParamDefinitionFlags declares the three flags that define a saved
// query's parameters. create and update take the same set, and mean the same
// thing by it, so the wording is written once.
func addParamDefinitionFlags(cmd *cobra.Command, params *paramFlagValues) {
	cmd.Flags().StringArrayVar(&params.defaults, "param-default", nil,
		"parameter default as name=value, repeatable")
	cmd.Flags().StringArrayVar(&params.types, "param-type", nil,
		"parameter type as name=type, repeatable (default: text)")
	cmd.Flags().StringArrayVar(&params.regexes, "param-regex", nil,
		"pattern a text-pattern parameter's value must match, as name=pattern, repeatable")
}

func runQueryCreate(cmd *cobra.Command, args []string, g *globalFlags, name, description, file, dataSource string,
	draft bool, params paramFlagValues, timeout time.Duration) error {
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

	// Composed and checked before anything is sent, for the same reason
	// --name is: a create that reached the server with a broken definition
	// would leave a query behind to find and fix, and Redash stores one
	// without complaint.
	set, err := parseParamFlags(params)
	if err != nil {
		return err
	}
	definitions, err := paramDefinitions(nil, set, sql, true)
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
		return timeoutOr(err, timeout, "the data source lookup")
	}

	// The definitions travel on the create itself: Redash recomputes the
	// query hash as it saves and links the result matching it, so a query
	// born with its defaults picks up the result `rdsh run` just produced.
	// Setting them afterwards would save the query without them first.
	var options *redash.QueryOptions
	if len(definitions) > 0 {
		options = &redash.QueryOptions{Parameters: definitions}
	}

	// A create the deadline cuts off is an ordinary timeout: either the
	// query was never saved, or it was and its ID never arrived, so there
	// is nothing to report but the expiry.
	q, err := client.CreateQuery(ctx, redash.NewQuery{
		Name:         name,
		Query:        sql,
		DataSourceID: dsID,
		Description:  description,
		Options:      options,
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
		params      paramFlagValues
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
overwritten.

--param-default, --param-type and --param-regex mean what they do on create,
applied as an edit: for a parameter already defined, only the keys the flags
carry are overwritten, so a title set in the Redash UI and a type no flag
mentions survive, as does every definition not named. A parameter defined as
a range, enum or dropdown query is refused rather than rewritten.

Passing any of them, or new SQL, also holds the query to the rule that every
parameter its SQL uses is defined. Changing only the name or description
does not, so a query whose parameters have no defaults can still be renamed.

Defining a parameter on a query that had none is a one-way move: from then
on, executing it with a name that is not among its definitions is refused,
which includes an ` + "`rdsh query refresh --param`" + ` that used to work.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQueryUpdate(cmd, args, g, name, description, file, publish, draft, params, timeout.Duration())
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "rename the saved query")
	cmd.Flags().StringVar(&description, "description", "", "set the description (pass an empty string to clear it)")
	cmd.Flags().StringVarP(&file, "file", "f", "", "read the new SQL from a file")
	cmd.Flags().BoolVar(&publish, "publish", false, "publish the query, putting it in everyone's query list")
	cmd.Flags().BoolVar(&draft, "draft", false, "turn the query back into a draft, hiding it from other users")
	addParamDefinitionFlags(cmd, &params)
	addTimeoutFlag(cmd, &timeout, serverTimeoutUsage)
	cmd.MarkFlagsMutuallyExclusive("publish", "draft")
	return cmd
}

func runQueryUpdate(cmd *cobra.Command, args []string, g *globalFlags, name, description, file string,
	publish, draft bool, params paramFlagValues, timeout time.Duration) error {
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

	// Read once so a flag that cannot be read at all costs no request. The
	// rest of the parameter checking needs the query, and waits for it.
	set, err := parseParamFlags(params)
	if err != nil {
		return err
	}

	// Which fields to send comes from whether the flag was given rather
	// than from its value, so that --description "" clears the description
	// while leaving it out keeps whatever the query has.
	flags := cmd.Flags()
	var u redash.QueryUpdate
	// Tracked rather than read back off u: the parameter definitions can
	// only be composed once the query has been read, so u still looks empty
	// here for an invocation that passes nothing but a --param-* flag.
	// Whether the SQL is held to the coverage check is decided here rather
	// than beside the check itself, so the flags that follow cannot widen
	// it: renaming a query must not be what demands its parameters be
	// defined.
	definesParams := params.given()
	checkSQL := hasSQL || definesParams
	changed := checkSQL
	if hasSQL {
		u.Query = &sql
	}
	if flags.Changed("name") {
		u.Name, changed = &name, true
	}
	if flags.Changed("description") {
		u.Description, changed = &description, true
	}
	switch {
	case flags.Changed("publish"):
		isDraft := !publish
		u.IsDraft, changed = &isDraft, true
	case flags.Changed("draft"):
		u.IsDraft, changed = &draft, true
	}
	if !changed {
		return errors.New("nothing to change; pass new SQL, --name, --description, --publish, --draft, " +
			"--param-default, --param-type, or --param-regex")
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

	// The SQL the definitions are held against is the one the query will
	// have. Checking it at all is what a metadata-only edit skips: a query
	// whose parameters have no defaults is the very thing this feature
	// exists to fix, so renaming one must not be what demands they be
	// defined first.
	finalSQL := current.Query
	if hasSQL {
		finalSQL = sql
	}
	definitions, err := paramDefinitions(current.Options.Parameters, set, finalSQL, checkSQL)
	if err != nil {
		return err
	}
	if definesParams {
		// Redash replaces options wholesale, so what goes back is the whole
		// object as it was read with the definitions merged into it.
		options := current.Options
		options.Parameters = definitions
		u.Options = &options
	}

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
	addTimeoutFlag(cmd, &timeout, serverTimeoutUsage)
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
SQL: id, name, description, data_source_id, is_draft, url, query, and
visualizations — the charts on the query page, as id, type, name and their
stored options. That array is the only way to reach a visualization at all:
Redash has no endpoint that reads one by ID, so it is where both the ID that
` + "`rdsh visualization update`" + ` and ` + "`delete`" + ` take and the options an
` + "`--options-file`" + ` edit has to start from come from. json is the only value
--format takes here — a multi-line SQL body does not fit a row format.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQueryShow(cmd, args, g, outputFormat, timeout.Duration())
		},
	}
	cmd.Flags().Var(&outputFormat, "format", "output format: json (default: the SQL itself)")
	addTimeoutFlag(cmd, &timeout, serverTimeoutUsage)
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
	// queryVisualization is the visualization as this command reports it.
	// The options blob is included, and has to be: Redash registers
	// /api/visualizations/<id> but defines no GET on it, so a query's
	// visualizations array is the only place the stored options can be read
	// — and `rdsh visualization update --options-file` replaces the whole
	// object, so composing one without reading the current keys first drops
	// every setting the file does not happen to repeat.
	//
	// The keys are passed through exactly as they arrived, which is what
	// makes the round trip lossless for the ones rdsh has no name for.
	type queryVisualization struct {
		ID      int                        `json:"id"`
		Type    string                     `json:"type"`
		Name    string                     `json:"name"`
		Options map[string]json.RawMessage `json:"options"`
	}
	// Empty rather than null for a query with no charts, so a caller can
	// iterate it without a branch — the same reason the row output prints
	// [] for no rows.
	visualizations := make([]queryVisualization, 0, len(q.Visualizations))
	for _, v := range q.Visualizations {
		// An empty object rather than null for a visualization stored
		// without options, so the value is always one --options-file can
		// start from.
		options := v.Options
		if options == nil {
			options = map[string]json.RawMessage{}
		}
		visualizations = append(visualizations,
			queryVisualization{ID: v.ID, Type: v.Type, Name: v.Name, Options: options})
	}

	return format.WriteObject(w, struct {
		ID             int                  `json:"id"`
		Name           string               `json:"name"`
		Description    string               `json:"description"`
		DataSourceID   int                  `json:"data_source_id"`
		IsDraft        bool                 `json:"is_draft"`
		URL            string               `json:"url"`
		Query          string               `json:"query"`
		Visualizations []queryVisualization `json:"visualizations"`
	}{
		ID:             q.ID,
		Name:           q.Name,
		Description:    q.Description,
		DataSourceID:   q.DataSourceID,
		IsDraft:        q.IsDraft,
		URL:            url,
		Query:          q.Query,
		Visualizations: visualizations,
	})
}

func newQueryRefreshCmd(g *globalFlags) *cobra.Command {
	var (
		params       []string
		outputFormat = format.CSV
		timeout      timeoutFlag
	)
	cmd := &cobra.Command{
		Use:   "refresh <id|url>",
		Short: "Execute a saved Redash query and print the fresh result",
		Long: `Execute a saved Redash query and print the fresh result.

The query is named by its ID or by its URL on the configured Redash
instance. Unlike ` + "`rdsh query show`" + ` piped into ` + "`rdsh run`" + `, this is the
execution the query page itself makes: the result Redash shows everyone —
and any chart drawn from it — is refreshed by the same call that prints the
rows here. The output is the same as ` + "`rdsh run`" + `'s, csv by default.

--param name=value supplies a parameter value and may be repeated;
everything after the first = is the value. A parameter left out is executed
with the default stored on the query, and one with neither a stored default
nor a --param fails the command, naming itself.

The result everyone else sees only advances when the query runs with its
own stored defaults. Override one with --param, or cover a parameter that
has no stored default, and the execution still succeeds and still prints
the rows, but the query page keeps showing what it showed before; a line
saying so goes to stderr.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQueryRefresh(cmd, args, g, params, outputFormat, timeout.Duration())
		},
	}
	cmd.Flags().StringArrayVar(&params, "param", nil,
		"parameter value as name=value, repeatable (default: the query's stored defaults)")
	cmd.Flags().Var(&outputFormat, "format", "output format: csv, tsv, or json")
	addTimeoutFlag(cmd, &timeout, jobTimeoutUsage)
	return cmd
}

// staleCacheNotice is what a run that could not advance the query page's
// result reports. It is a note rather than a failure: the rows on stdout
// are what was asked for, and stderr is where the listing's truncation note
// goes for the same reason.
const staleCacheNotice = "executed with parameter values that differ from the query's stored defaults, " +
	"so the query page still shows its previous result"

func runQueryRefresh(cmd *cobra.Command, args []string, g *globalFlags, params []string,
	outputFormat format.Format, timeout time.Duration) error {
	// Parsed before anything is sent, so a --param that binds to no name
	// cannot reach the server as a parameter set that is quietly missing
	// one. --format and --timeout are both checked as the flags are parsed.
	overrides, err := parseQueryParams(params)
	if err != nil {
		return err
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

	// Read first for the stored defaults: Redash substitutes only what the
	// request gives it, so a default the execution leaves out counts as a
	// missing parameter rather than as the value the query holds.
	//
	// Both calls report a deadline as an ordinary timeout. A refresh is an
	// execution, and the expiry cancels its job, so re-running the command
	// as it stands is safe — which is what exit code 124 tells an agent to
	// do.
	q, err := client.GetQuery(ctx, id)
	if err != nil {
		return timeoutOr(err, timeout, "query")
	}

	values, matchesDefaults := mergeParameters(q.Options.Parameters, overrides)
	result, err := client.RefreshQuery(ctx, id, values)
	if err != nil {
		return timeoutOr(err, timeout, "query")
	}

	if err := format.Write(cmd.OutOrStdout(), outputFormat, result); err != nil {
		return err
	}
	if !matchesDefaults {
		fmt.Fprintln(cmd.ErrOrStderr(), staleCacheNotice)
	}
	return nil
}

// parseQueryParams reads the --param values into the overrides the
// execution applies. A name given twice takes its last value, as a flag
// bound to a single variable would — unlike the --param-* flags that define
// a parameter, where a repeat is refused rather than resolved.
func parseQueryParams(params []string) (map[string]string, error) {
	overrides := make(map[string]string, len(params))
	for _, p := range params {
		name, value, err := splitParamToken("--param", p)
		if err != nil {
			return nil, err
		}
		overrides[name] = value
	}
	return overrides, nil
}

// splitParamToken reads one name=value token of a parameter flag. The split
// is at the first =, so everything after it is the value and needs no
// escaping of its own: a Redash parameter name cannot contain one, and a
// pattern very well may.
func splitParamToken(flag, token string) (string, string, error) {
	name, value, ok := strings.Cut(token, "=")
	if !ok || name == "" {
		return "", "", fmt.Errorf("%s %q has no parameter name to bind to; pass it as name=value", flag, token)
	}
	return name, value, nil
}

// mergeParameters returns the values to execute the saved query with, and
// reports whether they are the query's own stored defaults — which is what
// decides whether the result the query page shows everyone advances.
//
// A parameter defined without a stored default is left out entirely:
// sending its null earns a refusal that names no parameter, where leaving
// it out earns the server's own "missing parameter value for" — which is
// what tells a caller which one to pass. An override the query knows
// nothing about is sent as it is, since a query saved by `rdsh query
// create` carries no parameter definitions at all and its placeholders are
// reachable no other way.
func mergeParameters(stored []redash.QueryParameter, overrides map[string]string) (map[string]any, bool) {
	values := make(map[string]any, len(stored)+len(overrides))
	for _, p := range stored {
		if p.Value == nil {
			continue
		}
		values[p.Name] = p.Value
	}

	matchesDefaults := true
	for name, override := range overrides {
		// Reading what is already there is reading the stored default:
		// each name is a key of overrides, so nothing has replaced it yet.
		// An override that spells out that default is not one — the stored
		// value stays, so the request is the one the query page makes down
		// to the JSON types, and the cached result still moves.
		if current, ok := values[name]; ok && parameterText(current) == override {
			continue
		}
		values[name] = override
		matchesDefaults = false
	}
	return values, matchesDefaults
}

// parameterText renders a stored default for comparison with a --param
// value, which is always a string. Numbers arrive as json.Number and
// compare by the text they were written with; a default that is not a
// scalar at all — a date range is an object — never equals one, which is
// what makes the notice fire for the parameter kinds --param cannot
// express.
func parameterText(value any) string { return fmt.Sprint(value) }

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
