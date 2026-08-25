package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/178inaba/rdsh/internal/redash"
)

// visualizationTypeChart is the API type every chart carries, whichever of
// the five --type values named it: Redash has one CHART visualization and
// tells the kinds apart by options.globalSeriesType alone.
const visualizationTypeChart = "CHART"

// The options keys a typed chart writes, and the roles columnMapping
// assigns. Named rather than spelled inline because create composes them
// and update has to find the same keys again in a stored blob.
const (
	globalSeriesTypeKey = "globalSeriesType"
	columnMappingKey    = "columnMapping"
	columnRoleX         = "x"
	columnRoleY         = "y"
)

// chartTypes is the --type values a typed chart takes, in the order the help
// and the errors list them, each paired with the value viz-lib renders from.
// Only bar differs from its own name: Redash's "bar" is the horizontal one,
// and the upright bars everyone means by the word are its "column".
//
// One ordered list rather than a list of names beside a lookup, so a type
// can never be advertised in the help and then refused as unsupported by the
// message that just listed it.
//
// Checked against getredash/redash master on 2026-08-25. The schema is open
// source but undocumented and unversioned, and the server never validates
// it, so drift shows up only as a chart that renders wrong — which is why
// the version this was read at is pinned here rather than left implicit.
var chartTypes = []struct{ name, seriesType string }{
	{name: "line", seriesType: "line"},
	{name: "bar", seriesType: "column"},
	{name: "area", seriesType: "area"},
	{name: "scatter", seriesType: "scatter"},
	{name: "pie", seriesType: "pie"},
}

// chartTypeNames is that list's names, derived rather than written out
// again, for the flag help and the error message.
var chartTypeNames = func() []string {
	names := make([]string, len(chartTypes))
	for i, t := range chartTypes {
		names[i] = t.name
	}
	return names
}()

func newVisualizationCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use: "visualization",
		// Redash calls these visualizations, so the full word is the name;
		// viz is what anyone typing one by hand reaches for.
		Aliases: []string{"viz"},
		Short:   "Manage visualizations on saved queries",
	}
	cmd.AddCommand(newVisualizationCreateCmd(g), newVisualizationUpdateCmd(g), newVisualizationDeleteCmd(g))
	return cmd
}

// visualizationFlags is the flag set create and update share. update takes
// every one of them as optional, so whether a flag was given is what
// decides what an edit touches — not whether its value is empty.
type visualizationFlags struct {
	query       string
	name        string
	chartType   string
	x           string
	y           []string
	params      []string
	optionsFile string
	timeout     timeoutFlag
}

// addVisualizationFlags registers the shared flags on cmd. create and
// update mean the same thing by each, so the wording is written once.
func addVisualizationFlags(cmd *cobra.Command, f *visualizationFlags) {
	cmd.Flags().StringVar(&f.query, "query", "", "saved query ID or URL the visualization belongs to (required)")
	cmd.Flags().StringVar(&f.name, "name", "", "name shown above the visualization")
	cmd.Flags().StringVar(&f.chartType, "type", "",
		"chart type: "+strings.Join(chartTypeNames, ", ")+" (with --options-file, the API type verbatim)")
	cmd.Flags().StringVar(&f.x, "x", "", "column plotted on the x axis")
	cmd.Flags().StringArrayVar(&f.y, "y", nil, "column plotted on the y axis, repeatable for a multi-series chart")
	cmd.Flags().StringArrayVar(&f.params, "param", nil,
		"parameter value as name=value, repeatable (default: the query's stored defaults)")
	cmd.Flags().StringVar(&f.optionsFile, "options-file", "",
		"read the raw options JSON from a file instead of composing it from --type, --x and --y")
	addTimeoutFlag(cmd, &f.timeout, serverTimeoutUsage)
	// The typed flags describe a chart rdsh composes; the file is one the
	// caller composed. Mixing them would leave it unsaid which won.
	cmd.MarkFlagsMutuallyExclusive("options-file", "x")
	cmd.MarkFlagsMutuallyExclusive("options-file", "y")
	// --param exists only to resolve the result the column check runs
	// against, and raw mode runs no column check.
	cmd.MarkFlagsMutuallyExclusive("options-file", "param")
}

// rawMode reports whether the invocation composes its options from a file
// rather than from the typed flags.
func (f *visualizationFlags) rawMode() bool { return f.optionsFile != "" }

func newVisualizationCreateCmd(g *globalFlags) *cobra.Command {
	var f visualizationFlags
	cmd := &cobra.Command{
		Use:   "create --query <id|url> --name <name> --type <type> --x <column> --y <column>",
		Short: "Add a visualization to a saved Redash query and print the query's URL",
		Long: `Add a visualization to a saved Redash query and print the query's URL.

A visualization lives on the query page, so a colleague opening the URL sees
the chart rather than raw rows. The query is named by its ID or by its URL on
the configured Redash instance.

--type takes ` + strings.Join(chartTypeNames, ", ") + `. All five are the one Redash chart
type and differ only in one stored setting, globalSeriesType; bar is the
upright one.

Everything else about the chart is left to Redash's own defaults, which is
what keeps a chart rdsh made looking like one the UI made. Three of those
defaults are worth knowing before picking a type, because the word for the
type does not imply them:

  area is not stacked. Two series are drawn over each other translucently,
  and the y axis tops out at the larger series rather than at the sum.

  pie is ordered by share, not by the row order of the result.

  the x axis type is auto-detected, so a column of date-like strings such as
  2026-01 becomes a date axis rather than evenly spaced categories.

Each of those is reachable through --options-file (series.stacking, piesort,
xAxis.type), which is also where every other chart setting lives.

--x names the column along the x axis and --y the column plotted against it.
--y is repeatable, which is how a chart gets several series.

The column names are checked before anything is created, against the result
the query already holds — Redash stores a visualization naming a column that
does not exist without complaint, and it renders as a blank chart nobody is
told about. The check needs a cached result: if the query has none, nothing
is created and the error says to run ` + "`rdsh query refresh`" + ` first. Creating a
visualization never executes a query itself.

--param name=value supplies a parameter value for that check and may be
repeated; everything after the first = is the value. A parameter left out
uses the default stored on the query. The values pick which cached result is
checked, so they should be the ones the chart will be viewed with.

--options-file reads the raw Redash options JSON from a file instead. It is
the way to reach chart settings and visualization types the typed flags do
not cover: --type is then sent verbatim as the API type (CHART, COUNTER, and
so on), --x, --y and --param are not accepted, and no column check runs — the
file is taken on trust.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVisualizationCreate(cmd, g, &f)
		},
	}
	addVisualizationFlags(cmd, &f)
	return cmd
}

func runVisualizationCreate(cmd *cobra.Command, g *globalFlags, f *visualizationFlags) error {
	// Everything judgeable from the flags alone is judged before anything is
	// sent: a create that reached the server malformed would leave a chart
	// behind to find and delete, and Redash stores one without complaint.
	if f.query == "" {
		return errors.New("--query is required")
	}
	if f.name == "" {
		return errors.New("--name is required")
	}
	if f.chartType == "" {
		return errors.New("--type is required")
	}
	seriesType := ""
	if !f.rawMode() {
		var err error
		if seriesType, err = chartSeriesType(f.chartType); err != nil {
			return err
		}
		if f.x == "" {
			return errors.New("--x is required")
		}
		if len(f.y) == 0 {
			return errors.New("--y is required")
		}
	}
	overrides, err := parseQueryParams(f.params)
	if err != nil {
		return err
	}
	// Read before the connection is even resolved, so an unreadable path
	// fails as the invocation error it is.
	rawOptions, err := readOptionsFile(f.optionsFile)
	if err != nil {
		return err
	}

	conn, err := resolveConnection(g.profile)
	if err != nil {
		return err
	}
	client := redash.NewClient(conn.URL, conn.APIKey)
	queryID, err := resolveQueryID(f.query, client)
	if err != nil {
		return err
	}

	timeout := f.timeout.Duration()
	ctx, cancel := withTimeout(cmd.Context(), timeout)
	defer cancel()

	options, apiType := rawOptions, f.chartType
	if !f.rawMode() {
		// Read for the stored parameter defaults, which decide which cached
		// result the column check is run against.
		q, err := client.GetQuery(ctx, queryID)
		if err != nil {
			return timeoutOr(err, timeout, "the query")
		}
		if err := validateChartColumns(ctx, client, queryID, q.Options.Parameters,
			overrides, timeout, f.x, f.y); err != nil {
			return err
		}
		if options, err = chartOptions(seriesType, f.x, f.y); err != nil {
			return err
		}
		apiType = visualizationTypeChart
	}

	// A create the deadline cuts off is an ordinary timeout, for the reason
	// `query create` reports one: either the visualization was never stored,
	// or it was and the answer never arrived, and there is nothing to report
	// but the expiry either way.
	if _, err := client.CreateVisualization(ctx, redash.NewVisualization{
		QueryID: queryID,
		Type:    apiType,
		Name:    f.name,
		Options: options,
	}); err != nil {
		return timeoutOr(err, timeout, "the visualization")
	}

	fmt.Fprintln(cmd.OutOrStdout(), client.QueryURL(queryID))
	return nil
}

func newVisualizationUpdateCmd(g *globalFlags) *cobra.Command {
	var f visualizationFlags
	cmd := &cobra.Command{
		Use:   "update <id> --query <id|url>",
		Short: "Edit a visualization on a saved Redash query and print the query's URL",
		Long: `Edit a visualization on a saved Redash query and print the query's URL.

The visualization is named by its ID, which ` + "`rdsh query show --format json`" + `
lists. --query is required as well: Redash has no endpoint that reads a
visualization by ID, so the one being edited is found through its query —
which also means a mistyped ID is refused rather than applied to some other
query's chart.

At least one change must be given, or the command fails without sending
anything. Only what is passed changes: the stored options are read, the keys
the flags name are replaced, and everything else — settings chosen in the
Redash UI included — is sent back as it was.

--x and --y move the axes. Because columnMapping is keyed by column name,
passing --x alone moves the x column and leaves the y columns as they are —
and moving one axis onto the column the other holds is refused rather than
dropping that other axis, so swapping them means passing both at once. Any
column passed is checked against the query's cached result exactly as on
create; passing neither runs no check, which is also why --param needs one of
them to mean anything.

The typed flags describe the Redash chart type, so they are refused on a
visualization that is not one — use --options-file for those. With
--options-file the file replaces the whole options object rather than being
merged into it, and --type is sent verbatim as the API type.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVisualizationUpdate(cmd, args, g, &f)
		},
	}
	addVisualizationFlags(cmd, &f)
	return cmd
}

func runVisualizationUpdate(cmd *cobra.Command, args []string, g *globalFlags, f *visualizationFlags) error {
	vizID, err := parseVisualizationID(args[0])
	if err != nil {
		return err
	}
	if f.query == "" {
		return errors.New("--query is required")
	}
	// An update with nothing to change fails here rather than making a
	// request that means nothing, which is what `query update` does too.
	if f.name == "" && f.chartType == "" && f.x == "" && len(f.y) == 0 && !f.rawMode() {
		return errors.New("nothing to change: pass --name, --type, --x, --y or --options-file")
	}
	// --param only picks which cached result the column check runs against,
	// and no check runs when no column changes. Accepting it and discarding
	// it would let a caller believe values reached the server that never did.
	if len(f.params) > 0 && f.x == "" && len(f.y) == 0 {
		return errors.New("--param applies only to the column check, so it needs --x or --y to do anything")
	}
	seriesType := ""
	if f.chartType != "" && !f.rawMode() {
		if seriesType, err = chartSeriesType(f.chartType); err != nil {
			return err
		}
	}
	overrides, err := parseQueryParams(f.params)
	if err != nil {
		return err
	}
	rawOptions, err := readOptionsFile(f.optionsFile)
	if err != nil {
		return err
	}

	conn, err := resolveConnection(g.profile)
	if err != nil {
		return err
	}
	client := redash.NewClient(conn.URL, conn.APIKey)
	queryID, err := resolveQueryID(f.query, client)
	if err != nil {
		return err
	}

	timeout := f.timeout.Duration()
	ctx, cancel := withTimeout(cmd.Context(), timeout)
	defer cancel()

	q, viz, err := findVisualization(ctx, client, queryID, vizID, timeout)
	if err != nil {
		return err
	}

	update := redash.VisualizationUpdate{}
	if f.rawMode() {
		// A pointer to the decoded object, empty or not: the file replaces
		// the whole options object, and an empty one says so deliberately.
		update.Options = &rawOptions
	}
	if f.name != "" {
		update.Name = &f.name
	}
	if f.rawMode() {
		if f.chartType != "" {
			update.Type = &f.chartType
		}
	} else {
		if seriesType == "" && f.x == "" && len(f.y) == 0 {
			// A rename alone: nothing touches the options, so they are left
			// out rather than sent back, which is one fewer way to lose a
			// key between the read and the write.
			return applyVisualizationUpdate(ctx, cmd, client, vizID, queryID, update, timeout)
		}
		// The typed flags write the CHART schema's own keys, so writing them
		// onto anything else would store settings nothing ever reads.
		if viz.Type != visualizationTypeChart {
			return fmt.Errorf("visualization %d is a %s, and --type, --x and --y describe a %s; "+
				"edit it with --options-file instead", vizID, viz.Type, visualizationTypeChart)
		}
		if f.x != "" || len(f.y) > 0 {
			if err := validateChartColumns(ctx, client, queryID, q.Options.Parameters,
				overrides, timeout, f.x, f.y); err != nil {
				return err
			}
		}
		patched, err := patchChartOptions(viz.Options, seriesType, f.x, f.y)
		if err != nil {
			return err
		}
		update.Options = &patched
	}
	return applyVisualizationUpdate(ctx, cmd, client, vizID, queryID, update, timeout)
}

// applyVisualizationUpdate sends the composed edit and prints the query URL.
// It takes the deadline-bearing context rather than cmd.Context(), so
// --timeout bounds this call as it bounds every other one.
func applyVisualizationUpdate(ctx context.Context, cmd *cobra.Command, client *redash.Client,
	vizID, queryID int, update redash.VisualizationUpdate, timeout time.Duration) error {
	if _, err := client.UpdateVisualization(ctx, vizID, update); err != nil {
		return timeoutOr(err, timeout, "the visualization")
	}
	fmt.Fprintln(cmd.OutOrStdout(), client.QueryURL(queryID))
	return nil
}

func newVisualizationDeleteCmd(g *globalFlags) *cobra.Command {
	var (
		query   string
		timeout timeoutFlag
	)
	cmd := &cobra.Command{
		Use:   "delete <id> --query <id|url>",
		Short: "Remove a visualization from a saved Redash query and print the query's URL",
		Long: `Remove a visualization from a saved Redash query and print the query's URL.

The visualization is named by its ID, which ` + "`rdsh query show --format json`" + `
lists. --query names the query it belongs to, and the ID is confirmed against
that query before anything is removed — a mistyped one is refused rather than
deleting some other query's chart.

The query itself and its result are untouched.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVisualizationDelete(cmd, args, g, query, timeout.Duration())
		},
	}
	cmd.Flags().StringVar(&query, "query", "", "saved query ID or URL the visualization belongs to (required)")
	addTimeoutFlag(cmd, &timeout, serverTimeoutUsage)
	return cmd
}

func runVisualizationDelete(cmd *cobra.Command, args []string, g *globalFlags, query string,
	timeout time.Duration) error {
	vizID, err := parseVisualizationID(args[0])
	if err != nil {
		return err
	}
	if query == "" {
		return errors.New("--query is required")
	}

	conn, err := resolveConnection(g.profile)
	if err != nil {
		return err
	}
	client := redash.NewClient(conn.URL, conn.APIKey)
	queryID, err := resolveQueryID(query, client)
	if err != nil {
		return err
	}

	ctx, cancel := withTimeout(cmd.Context(), timeout)
	defer cancel()

	if _, _, err := findVisualization(ctx, client, queryID, vizID, timeout); err != nil {
		return err
	}
	// Re-running after an expiry is safe: the visualization is either still
	// there, in which case the delete lands, or already gone, in which case
	// the check above names it rather than removing anything else.
	if err := client.DeleteVisualization(ctx, vizID); err != nil {
		return timeoutOr(err, timeout, "the visualization")
	}

	fmt.Fprintln(cmd.OutOrStdout(), client.QueryURL(queryID))
	return nil
}

// findVisualization reads the query and returns the visualization it holds
// under vizID. Redash has no endpoint that reads one by ID, so this is both
// the only way to reach its stored options and the check that the ID a
// caller typed belongs to the query they named.
// It returns the query as well as the visualization, so a caller that goes
// on to check columns has the stored parameter definitions without reading
// the same query a second time.
func findVisualization(ctx context.Context, client *redash.Client, queryID, vizID int,
	timeout time.Duration) (*redash.Query, *redash.Visualization, error) {
	q, err := client.GetQuery(ctx, queryID)
	if err != nil {
		return nil, nil, timeoutOr(err, timeout, "the query")
	}
	for i, v := range q.Visualizations {
		if v.ID == vizID {
			return q, &q.Visualizations[i], nil
		}
	}
	ids := make([]string, 0, len(q.Visualizations))
	for _, v := range q.Visualizations {
		ids = append(ids, fmt.Sprintf("%d (%s)", v.ID, v.Name))
	}
	if len(ids) == 0 {
		return nil, nil, fmt.Errorf("query %d has no visualization %d, and none at all", queryID, vizID)
	}
	return nil, nil, fmt.Errorf("query %d has no visualization %d; it has %s",
		queryID, vizID, strings.Join(ids, ", "))
}

// chartSeriesType maps a --type value to what viz-lib renders from, or
// reports the values that exist. A linear scan over five entries, which
// keeps the accepted set and the order the message lists it in one thing.
func chartSeriesType(name string) (string, error) {
	for _, t := range chartTypes {
		if t.name == name {
			return t.seriesType, nil
		}
	}
	return "", fmt.Errorf("unsupported chart type %q (supported: %s); "+
		"for any other Redash visualization type, use --options-file",
		name, strings.Join(chartTypeNames, ", "))
}

// chartOptions is the options blob a typed chart is created with: the two
// keys that say what is drawn and from which columns, and nothing else.
//
// Deliberately minimal. viz-lib merges what is stored over its own
// DEFAULT_OPTIONS at render time, with the stored keys winning, so every
// setting left out keeps whatever default that Redash version has — where
// writing them out would freeze today's defaults into the chart and grow the
// surface that can drift out of step with the schema.
func chartOptions(seriesType, x string, ys []string) (map[string]json.RawMessage, error) {
	mapping := make(map[string]string, len(ys)+1)
	mapping[x] = columnRoleX
	for _, y := range ys {
		mapping[y] = columnRoleY
	}
	// Nothing was there before, so both axes have to be here now.
	if err := checkAxesSurvive(nil, mapping); err != nil {
		return nil, err
	}
	// Both values are composed here from strings, so neither can fail to
	// encode and the errors json.Marshal declares cannot occur.
	seriesJSON, _ := json.Marshal(seriesType)
	mappingJSON, _ := json.Marshal(mapping)
	return map[string]json.RawMessage{
		globalSeriesTypeKey: seriesJSON,
		columnMappingKey:    mappingJSON,
	}, nil
}

// hasRole reports whether any column in the mapping carries the role.
func hasRole(mapping map[string]string, role string) bool {
	for _, assigned := range mapping {
		if assigned == role {
			return true
		}
	}
	return false
}

// checkAxesSurvive reports a composed columnMapping that has silently lost
// an axis. The map is keyed by column name, so naming one column for two
// roles is not a conflict it can hold: the second assignment replaces the
// first and the axis that column used to carry is simply gone. Every name
// involved is a real column, so the column check passes and the chart is
// stored rendering nothing — the very failure the typed flags exist to
// prevent.
//
// before is the mapping as it was stored, or nil when the visualization is
// being created and both axes are therefore required outright. An edit is
// held only to what was already there, so a chart that never had an axis is
// not made to grow one here.
func checkAxesSurvive(before, after map[string]string) error {
	for _, role := range []string{columnRoleX, columnRoleY} {
		if before != nil && !hasRole(before, role) {
			continue
		}
		if hasRole(after, role) {
			continue
		}
		return fmt.Errorf("the columns given leave the chart with no %s column, because %q is now "+
			"its %s column and a column can hold only one role; "+
			"pass --x and --y together to move both at once",
			role, columnFor(after, otherRole(role)), otherRole(role))
	}
	return nil
}

// otherRole is the axis a role displaces when the two collide.
func otherRole(role string) string {
	if role == columnRoleX {
		return columnRoleY
	}
	return columnRoleX
}

// columnFor names a column holding the role, for the error above. The
// mapping is a map, so with several columns on one axis which is named is
// arbitrary — but the collision it reports can only involve one.
func columnFor(mapping map[string]string, role string) string {
	for column, assigned := range mapping {
		if assigned == role {
			return column
		}
	}
	return ""
}

// patchChartOptions applies the typed flags to a stored options blob and
// returns the whole object to send back. Redash replaces every key the
// request carries and leaves the rest, so an edit has to hand back what it
// never meant to touch — which is why this works from what was just read
// rather than composing a fresh object the way create does.
func patchChartOptions(stored map[string]json.RawMessage, seriesType, x string,
	ys []string) (map[string]json.RawMessage, error) {
	options := make(map[string]json.RawMessage, len(stored)+2)
	for k, v := range stored {
		options[k] = v
	}
	if seriesType != "" {
		seriesJSON, err := json.Marshal(seriesType)
		if err != nil {
			return nil, err
		}
		options[globalSeriesTypeKey] = seriesJSON
	}
	if x == "" && len(ys) == 0 {
		return options, nil
	}

	before := map[string]string{}
	if raw, ok := stored[columnMappingKey]; ok {
		if err := json.Unmarshal(raw, &before); err != nil {
			return nil, fmt.Errorf("the visualization's stored %s is not a map of column names to roles, "+
				"so --x and --y cannot edit it; use --options-file: %w", columnMappingKey, err)
		}
	}
	mapping := make(map[string]string, len(before)+len(ys)+1)
	for column, role := range before {
		mapping[column] = role
	}
	// The map is keyed by column name rather than by role, so moving an axis
	// means dropping whatever held that role before. Replacing one axis
	// leaves the other's columns exactly as they were.
	replace := func(role string, columns []string) {
		for column, assigned := range mapping {
			if assigned == role {
				delete(mapping, column)
			}
		}
		for _, column := range columns {
			mapping[column] = role
		}
	}
	if x != "" {
		replace(columnRoleX, []string{x})
	}
	if len(ys) > 0 {
		replace(columnRoleY, ys)
	}
	// Both replacements are applied before this runs, so naming both axes at
	// once — swapping them — is the ordinary edit it looks like, while
	// moving one onto the other's column is caught.
	if err := checkAxesSurvive(before, mapping); err != nil {
		return nil, err
	}

	mappingJSON, err := json.Marshal(mapping)
	if err != nil {
		return nil, err
	}
	options[columnMappingKey] = mappingJSON
	return options, nil
}

// validateChartColumns is the check Redash does not do. A visualization
// naming a column the result has no column for is stored without complaint
// and renders as a blank chart, which a caller working through a shell
// cannot see — so the names are checked here, before anything is written.
//
// It runs against the result the caller's own parameter values map to,
// which is the result the chart renders from when the page is viewed with
// those values. One probe is enough because the same SQL yields the same
// columns whatever the values are; the values only decide which cached
// result answers.
func validateChartColumns(ctx context.Context, client *redash.Client, queryID int,
	stored []redash.QueryParameter, overrides map[string]string, timeout time.Duration,
	x string, ys []string) error {
	// The query's stored parameter definitions come from the caller, which
	// has already read it — update reads it to find the visualization, and
	// create to get here at all — so the check adds one round trip, the
	// probe, rather than two.
	//
	// The same merge `query refresh` applies: the stored defaults, with
	// --param on top. A parameter neither covers is reported by the server
	// from the probe, naming itself, exactly as it is there.
	values, _ := mergeParameters(stored, overrides)

	result, job, err := client.CachedQueryResult(ctx, queryID, values)
	if err != nil {
		return timeoutOr(err, timeout, "the query's cached result")
	}
	if job != nil {
		// No cached result, so the server started executing to make one.
		// Creating a chart is a metadata call and must not turn into a
		// query run behind the caller's back, so the job goes.
		client.CancelJobBestEffort(job.ID)
		return fmt.Errorf("query %d has no cached result for these parameter values, "+
			"so the columns cannot be checked; run `rdsh query refresh %d`%s first",
			queryID, queryID, refreshParamHint(overrides))
	}

	columns := make(map[string]bool, len(result.Columns))
	for _, c := range result.Columns {
		columns[c.Name] = true
	}
	var missing []string
	for _, name := range append([]string{x}, ys...) {
		if name != "" && !columns[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	available := make([]string, 0, len(result.Columns))
	for _, c := range result.Columns {
		available = append(available, c.Name)
	}
	slices.Sort(missing)
	return fmt.Errorf("the query's result has no column %s; its columns are %s",
		strings.Join(missing, ", "), strings.Join(available, ", "))
}

// refreshParamHint spells the --param flags back into the refresh command
// the error points at, so the run that fills the cache is the one this
// probe would hit.
func refreshParamHint(overrides map[string]string) string {
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, name := range names {
		// Quoted when it has to be, so the command in the message is one
		// that can be run as it stands — an agent reading this error is
		// likely to do exactly that, and a value with a space in it would
		// otherwise turn into two arguments.
		fmt.Fprintf(&b, " --param %s", shellArg(name+"="+overrides[name]))
	}
	return b.String()
}

// shellArg renders one argument for a command a caller may paste into a
// shell, single-quoting it unless every character is plainly safe. The
// conservative set is deliberate: quoting something that did not need it
// costs two characters, and not quoting something that did produces a
// command that runs and means something else.
func shellArg(arg string) string {
	needsQuoting := func(r rune) bool {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
			return false
		case r == '-', r == '_', r == '.', r == '/', r == '=', r == ':':
			return false
		}
		return true
	}
	if arg != "" && !strings.ContainsFunc(arg, needsQuoting) {
		return arg
	}
	// A single quote cannot appear inside single quotes, so it is closed,
	// escaped and reopened — the standard POSIX dance.
	return "'" + strings.ReplaceAll(arg, "'", `'''`) + "'"
}

// readOptionsFile reads the raw options JSON --options-file names. An empty
// path means the invocation is in typed mode and there is nothing to read.
//
// The content is checked as far as being a JSON object here, and no further:
// what the keys mean is the front end's business, and refusing what rdsh
// does not recognise would defeat the point of the escape hatch. Each key is
// kept as it was written, so a number keeps the text it was given.
func readOptionsFile(path string) (map[string]json.RawMessage, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var options map[string]json.RawMessage
	if err := json.Unmarshal(data, &options); err != nil {
		return nil, fmt.Errorf("--options-file %s is not a JSON object: %w", path, err)
	}
	if options == nil {
		return nil, fmt.Errorf("--options-file %s holds null, not an options object", path)
	}
	return options, nil
}

// parseVisualizationID reads the ID argument the update and delete
// subcommands take. Unlike a query, a visualization has no URL of its own —
// it is part of its query's page — so an ID is the only form.
func parseVisualizationID(arg string) (int, error) {
	if !isDigits(arg) {
		return 0, fmt.Errorf("%q is not a visualization ID; "+
			"`rdsh query show <id|url> --format json` lists the IDs a query's visualizations have", arg)
	}
	id, err := strconv.Atoi(arg)
	if err != nil {
		return 0, err
	}
	return id, nil
}
