package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// chartVisualization is what the fake serves as the query's one chart. The
// options are a whole stored blob rather than the minimal delta a create
// writes, so a test can tell an edit that preserved the keys it did not
// name from one that rewrote the object.
func chartVisualization() map[string]any {
	return map[string]any{
		"id": savedVizID, "type": "CHART", "name": "daily",
		"options": map[string]any{
			"globalSeriesType": "line",
			"columnMapping":    map[string]any{"day": "x", "count": "y"},
			"legend":           map[string]any{"enabled": false},
		},
	}
}

// withChart is a fake holding one chart on the saved query, with a cached
// result whose columns that chart is drawn from.
func withChart() *fakeServer {
	return &fakeServer{
		savedQueryVisualizations: []map[string]any{chartVisualization()},
		cachedColumns:            []string{"day", "count", "team"},
	}
}

// vizOptions reads the options object out of a recorded request body.
func vizOptions(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	options, ok := body["options"].(map[string]any)
	if !ok {
		t.Fatalf("request body = %v, want an options object", body)
	}
	return options
}

// TestVisualizationCreateChartTypes pins the one thing a caller cannot
// check for itself: Redash stores the options blob without validating it,
// so a wrong globalSeriesType is a blank chart nobody is told about. All
// five types are the single CHART type on the wire, and bar is Redash's
// "column" — its own "bar" is the horizontal one.
func TestVisualizationCreateChartTypes(t *testing.T) {
	tests := []struct {
		flag string
		want string
	}{
		{flag: "line", want: "line"},
		{flag: "bar", want: "column"},
		{flag: "area", want: "area"},
		{flag: "scatter", want: "scatter"},
		{flag: "pie", want: "pie"},
	}
	for _, tt := range tests {
		t.Run(tt.flag, func(t *testing.T) {
			f := &fakeServer{cachedColumns: []string{"day", "count"}}
			srv := f.start(t)

			out, err := runRdsh(t, srv, "", "visualization", "create", "--query", "7",
				"--name", "daily", "--type", tt.flag, "--x", "day", "--y", "count")
			if err != nil {
				t.Fatalf("visualization create error = %v", err)
			}
			if want := savedQueryURL(srv) + "\n"; out != want {
				t.Errorf("output = %q, want %q and nothing else", out, want)
			}

			f.mu.Lock()
			defer f.mu.Unlock()
			if f.createdViz["query_id"] != float64(savedQueryID) {
				t.Errorf("created query_id = %v, want %d", f.createdViz["query_id"], savedQueryID)
			}
			if f.createdViz["type"] != "CHART" {
				t.Errorf("created type = %v, want CHART for every typed chart", f.createdViz["type"])
			}
			if f.createdViz["name"] != "daily" {
				t.Errorf("created name = %v, want daily", f.createdViz["name"])
			}
			// Only the two keys, because viz-lib merges what is stored over
			// its own defaults: writing more would pin settings the caller
			// never chose to whatever they happen to be today.
			want := map[string]any{
				"globalSeriesType": tt.want,
				"columnMapping":    map[string]any{"day": "x", "count": "y"},
			}
			if got := vizOptions(t, f.createdViz); !reflect.DeepEqual(got, want) {
				t.Errorf("created options = %#v, want %#v", got, want)
			}
		})
	}
}

// TestVisualizationCreateMultipleY covers a multi-series chart: several
// columns map to "y" in the same columnMapping.
func TestVisualizationCreateMultipleY(t *testing.T) {
	f := &fakeServer{cachedColumns: []string{"day", "count", "team"}}
	srv := f.start(t)

	if _, err := runRdsh(t, srv, "", "visualization", "create", "--query", "7",
		"--name", "daily", "--type", "line", "--x", "day", "--y", "count", "--y", "team"); err != nil {
		t.Fatalf("visualization create error = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	want := map[string]any{"day": "x", "count": "y", "team": "y"}
	if got := vizOptions(t, f.createdViz)["columnMapping"]; !reflect.DeepEqual(got, want) {
		t.Errorf("columnMapping = %#v, want %#v", got, want)
	}
}

// TestVisualizationCreateRejectsUnknownColumn is the check that exists
// because Redash has none: a column the result does not have is stored
// without complaint and renders as nothing. The error has to name the
// columns that do exist, since the caller cannot see the chart either.
func TestVisualizationCreateRejectsUnknownColumn(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "x", args: []string{"--x", "days", "--y", "count"}},
		{name: "y", args: []string{"--x", "day", "--y", "total"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeServer{cachedColumns: []string{"day", "count"}}
			srv := f.start(t)

			args := append([]string{"visualization", "create", "--query", "7",
				"--name", "daily", "--type", "line"}, tt.args...)
			_, err := runRdsh(t, srv, "", args...)
			if err == nil {
				t.Fatal("error = nil, want a column that does not exist refused")
			}
			for _, column := range []string{"day", "count"} {
				if !strings.Contains(err.Error(), column) {
					t.Errorf("error = %v, want it to list the column %q", err, column)
				}
			}

			f.mu.Lock()
			defer f.mu.Unlock()
			if f.createdViz != nil {
				t.Errorf("created visualization = %v, want nothing created", f.createdViz)
			}
		})
	}
}

// TestVisualizationCreateCancelsTheProbeJob covers the cache miss. The
// probe leaves the server executing, so the job is cancelled rather than
// left running, and the caller is sent to the command that fills the cache
// — creating a chart never executes a query itself.
func TestVisualizationCreateCancelsTheProbeJob(t *testing.T) {
	f := &fakeServer{} // no cachedColumns: nothing is cached
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "visualization", "create", "--query", "7",
		"--name", "daily", "--type", "line", "--x", "day", "--y", "count")
	if err == nil {
		t.Fatal("error = nil, want a query with no cached result refused")
	}
	if !strings.Contains(err.Error(), "rdsh query refresh") {
		t.Errorf("error = %v, want it to name `rdsh query refresh`", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.cancelled {
		t.Error("the probe's job was not cancelled on the server")
	}
	if f.createdViz != nil {
		t.Errorf("created visualization = %v, want nothing created", f.createdViz)
	}
}

// TestVisualizationCreateProbesWithEffectiveParameters pins that the probe
// asks for the result the caller's own parameter values produce: the stored
// defaults, overridden by --param, exactly as `rdsh query refresh` merges
// them. A prior refresh with the same values is what makes this a cache hit.
func TestVisualizationCreateProbesWithEffectiveParameters(t *testing.T) {
	f := &fakeServer{
		cachedColumns: []string{"day", "count"},
		savedQueryParameters: []map[string]any{
			{"name": "days", "type": "number", "value": 7},
			{"name": "team", "type": "text", "value": "core"},
		},
	}
	srv := f.start(t)

	if _, err := runRdsh(t, srv, "", "visualization", "create", "--query", "7",
		"--name", "daily", "--type", "line", "--x", "day", "--y", "count",
		"--param", "team=eu"); err != nil {
		t.Fatalf("visualization create error = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	want := map[string]any{"days": float64(7), "team": "eu"}
	if !reflect.DeepEqual(f.probed["parameters"], want) {
		t.Errorf("probe parameters = %#v, want %#v", f.probed["parameters"], want)
	}
	if f.refreshed != nil {
		t.Errorf("execution body = %v, want the query never executed", f.refreshed)
	}
}

// TestVisualizationCreateRawOptions covers the escape hatch: the file is
// the options blob, --type goes through verbatim so a COUNTER or any other
// Redash type is reachable, and no column validation runs — the caller has
// taken that on.
func TestVisualizationCreateRawOptions(t *testing.T) {
	file := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(file, []byte(`{"counterColName": "total", "rowNumber": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{} // nothing cached: a probe would fail the command
	srv := f.start(t)

	if _, err := runRdsh(t, srv, "", "visualization", "create", "--query", "7",
		"--name", "total", "--type", "COUNTER", "--options-file", file); err != nil {
		t.Fatalf("visualization create error = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createdViz["type"] != "COUNTER" {
		t.Errorf("created type = %v, want COUNTER passed through verbatim", f.createdViz["type"])
	}
	want := map[string]any{"counterColName": "total", "rowNumber": float64(1)}
	if got := vizOptions(t, f.createdViz); !reflect.DeepEqual(got, want) {
		t.Errorf("created options = %#v, want the file's JSON as it was", got)
	}
	if f.probed != nil {
		t.Errorf("probe body = %v, want no column validation in raw mode", f.probed)
	}
}

// TestVisualizationCreateRawOptionsConflicts covers the flags raw mode has
// no meaning for. Each is refused while cobra parses the flags, before
// anything reaches the server.
func TestVisualizationCreateRawOptionsConflicts(t *testing.T) {
	file := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(file, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, conflict := range [][]string{
		{"--x", "day"},
		{"--y", "count"},
		{"--param", "days=7"},
	} {
		t.Run(conflict[0], func(t *testing.T) {
			f := &fakeServer{}
			srv := f.start(t)

			args := append([]string{"visualization", "create", "--query", "7", "--name", "daily",
				"--type", "CHART", "--options-file", file}, conflict...)
			if _, err := runRdsh(t, srv, "", args...); err == nil {
				t.Fatalf("error = nil, want %s refused alongside --options-file", conflict[0])
			}
			assertNoVisualizationSent(t, f)
		})
	}
}

// TestVisualizationCreateRequiresFlags covers the invocations refused
// before any request, so a bad one cannot leave a chart behind.
func TestVisualizationCreateRequiresFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "no query",
			args:    []string{"--name", "daily", "--type", "line", "--x", "day", "--y", "count"},
			wantErr: "query",
		},
		{
			name:    "no name",
			args:    []string{"--query", "7", "--type", "line", "--x", "day", "--y", "count"},
			wantErr: "name",
		},
		{
			name:    "no type",
			args:    []string{"--query", "7", "--name", "daily", "--x", "day", "--y", "count"},
			wantErr: "type",
		},
		{
			name:    "no x",
			args:    []string{"--query", "7", "--name", "daily", "--type", "line", "--y", "count"},
			wantErr: "x",
		},
		{
			name:    "no y",
			args:    []string{"--query", "7", "--name", "daily", "--type", "line", "--x", "day"},
			wantErr: "y",
		},
		{
			name:    "unknown type",
			args:    []string{"--query", "7", "--name", "daily", "--type", "donut", "--x", "day", "--y", "count"},
			wantErr: "donut",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeServer{cachedColumns: []string{"day", "count"}}
			srv := f.start(t)

			_, err := runRdsh(t, srv, "", append([]string{"visualization", "create"}, tt.args...)...)
			if err == nil {
				t.Fatal("error = nil, want the invocation refused")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to name %q", err, tt.wantErr)
			}
			assertNoVisualizationSent(t, f)
		})
	}
}

// TestVisualizationCreateTakesAQueryURL covers the <id|url> the saved-query
// commands share, reached here through a flag rather than an argument.
func TestVisualizationCreateTakesAQueryURL(t *testing.T) {
	f := &fakeServer{cachedColumns: []string{"day", "count"}}
	srv := f.start(t)

	out, err := runRdsh(t, srv, "", "visualization", "create", "--query", savedQueryURL(srv),
		"--name", "daily", "--type", "line", "--x", "day", "--y", "count")
	if err != nil {
		t.Fatalf("visualization create error = %v", err)
	}
	if want := savedQueryURL(srv) + "\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

// TestVisualizationCommandAlias pins the short name, which is what an agent
// reaching for the command by its Redash label will type.
func TestVisualizationCommandAlias(t *testing.T) {
	f := &fakeServer{cachedColumns: []string{"day", "count"}}
	srv := f.start(t)

	if _, err := runRdsh(t, srv, "", "viz", "create", "--query", "7",
		"--name", "daily", "--type", "line", "--x", "day", "--y", "count"); err != nil {
		t.Fatalf("viz create error = %v", err)
	}
}

// TestVisualizationUpdateChangesOnlyWhatIsPassed pins the read-modify-write:
// Redash replaces every key the request carries, so an edit that renamed a
// chart by sending a fresh options object would silently drop the settings
// someone chose in the UI.
func TestVisualizationUpdateChangesOnlyWhatIsPassed(t *testing.T) {
	f := withChart()
	srv := f.start(t)

	out, err := runRdsh(t, srv, "", "visualization", "update", "9", "--query", "7", "--name", "renamed")
	if err != nil {
		t.Fatalf("visualization update error = %v", err)
	}
	if want := savedQueryURL(srv) + "\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updatedViz["name"] != "renamed" {
		t.Errorf("updated name = %v, want renamed", f.updatedViz["name"])
	}
	if _, ok := f.updatedViz["options"]; ok {
		t.Errorf("updated options = %v, want the key left out when no chart setting changed",
			f.updatedViz["options"])
	}
	if f.probed != nil {
		t.Errorf("probe body = %v, want no column validation when no column changed", f.probed)
	}
}

// TestVisualizationUpdateReplacesOneAxis covers the fiddly half of the
// read-modify-write: columnMapping is keyed by column name, so moving the x
// column means dropping the old key rather than adding a second one — while
// the y columns and every other stored key stay as they were.
func TestVisualizationUpdateReplacesOneAxis(t *testing.T) {
	f := withChart()
	srv := f.start(t)

	if _, err := runRdsh(t, srv, "", "visualization", "update", "9", "--query", "7",
		"--x", "team"); err != nil {
		t.Fatalf("visualization update error = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	options := vizOptions(t, f.updatedViz)
	want := map[string]any{"team": "x", "count": "y"}
	if got := options["columnMapping"]; !reflect.DeepEqual(got, want) {
		t.Errorf("columnMapping = %#v, want %#v", got, want)
	}
	// The keys the flags never mentioned come back untouched, which is what
	// keeps a chart configured in the UI working after an rdsh edit.
	if got := options["globalSeriesType"]; got != "line" {
		t.Errorf("globalSeriesType = %v, want line kept as it was", got)
	}
	if got, want := options["legend"], map[string]any{"enabled": false}; !reflect.DeepEqual(got, want) {
		t.Errorf("legend = %#v, want %#v kept as it was", got, want)
	}
}

// TestVisualizationUpdateChangesTheChartType covers the flag that rewrites
// globalSeriesType without touching the columns.
func TestVisualizationUpdateChangesTheChartType(t *testing.T) {
	f := withChart()
	srv := f.start(t)

	if _, err := runRdsh(t, srv, "", "visualization", "update", "9", "--query", "7",
		"--type", "bar"); err != nil {
		t.Fatalf("visualization update error = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	options := vizOptions(t, f.updatedViz)
	if got := options["globalSeriesType"]; got != "column" {
		t.Errorf("globalSeriesType = %v, want column", got)
	}
	want := map[string]any{"day": "x", "count": "y"}
	if got := options["columnMapping"]; !reflect.DeepEqual(got, want) {
		t.Errorf("columnMapping = %#v, want %#v kept as it was", got, want)
	}
	if f.probed != nil {
		t.Errorf("probe body = %v, want no column validation when no column changed", f.probed)
	}
}

// TestVisualizationUpdateValidatesChangedColumns pins that the check a
// create runs is not skipped by going through update instead.
func TestVisualizationUpdateValidatesChangedColumns(t *testing.T) {
	f := withChart()
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "visualization", "update", "9", "--query", "7", "--y", "totals")
	if err == nil {
		t.Fatal("error = nil, want a column that does not exist refused")
	}
	if !strings.Contains(err.Error(), "count") {
		t.Errorf("error = %v, want it to list the columns that do exist", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updatedViz != nil {
		t.Errorf("updated visualization = %v, want nothing changed", f.updatedViz)
	}
}

// TestVisualizationUpdateRejectsUnrelatedVisualization covers what --query
// is for on a command whose API call does not need it: the ID is confirmed
// against the query it was named with, so a mistyped one cannot rewrite
// some other query's chart.
func TestVisualizationUpdateRejectsUnrelatedVisualization(t *testing.T) {
	f := withChart()
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "visualization", "update", "11", "--query", "7", "--name", "renamed")
	if err == nil {
		t.Fatal("error = nil, want a visualization the query does not have refused")
	}
	if !strings.Contains(err.Error(), "11") {
		t.Errorf("error = %v, want it to name the visualization that was asked for", err)
	}
	assertNoVisualizationSent(t, f)
}

// TestVisualizationUpdateRejectsTypedFlagsOnNonChart covers the guard on
// the typed flags' one assumption: globalSeriesType and columnMapping are
// the CHART schema's keys, and writing them onto a COUNTER would store
// settings nothing reads.
func TestVisualizationUpdateRejectsTypedFlagsOnNonChart(t *testing.T) {
	f := &fakeServer{
		savedQueryVisualizations: []map[string]any{
			{"id": savedVizID, "type": "COUNTER", "name": "total", "options": map[string]any{}},
		},
		cachedColumns: []string{"day", "count"},
	}
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "visualization", "update", "9", "--query", "7", "--type", "line")
	if err == nil {
		t.Fatal("error = nil, want typed flags refused on a COUNTER")
	}
	if !strings.Contains(err.Error(), "--options-file") {
		t.Errorf("error = %v, want it to point at the raw escape hatch", err)
	}
	assertNoVisualizationSent(t, f)
}

// TestVisualizationUpdateRequiresAChange mirrors `query update`: an
// invocation with nothing to change fails without sending anything, rather
// than making a request that means nothing.
func TestVisualizationUpdateRequiresAChange(t *testing.T) {
	f := withChart()
	srv := f.start(t)

	if _, err := runRdsh(t, srv, "", "visualization", "update", "9", "--query", "7"); err == nil {
		t.Fatal("error = nil, want an update with nothing to change refused")
	}
	assertNoVisualizationSent(t, f)
}

// TestVisualizationUpdateRawOptions covers raw mode on update: the file
// replaces the whole blob rather than being merged into it, since the point
// of the escape hatch is that the caller owns the object.
func TestVisualizationUpdateRawOptions(t *testing.T) {
	file := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(file, []byte(`{"globalSeriesType": "pie"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := withChart()
	srv := f.start(t)

	if _, err := runRdsh(t, srv, "", "visualization", "update", "9", "--query", "7",
		"--options-file", file); err != nil {
		t.Fatalf("visualization update error = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	want := map[string]any{"globalSeriesType": "pie"}
	if got := vizOptions(t, f.updatedViz); !reflect.DeepEqual(got, want) {
		t.Errorf("updated options = %#v, want the file's JSON as the whole object", got)
	}
}

func TestVisualizationDelete(t *testing.T) {
	f := withChart()
	srv := f.start(t)

	out, err := runRdsh(t, srv, "", "visualization", "delete", "9", "--query", "7")
	if err != nil {
		t.Fatalf("visualization delete error = %v", err)
	}
	if want := savedQueryURL(srv) + "\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.deletedViz {
		t.Error("the delete never reached the server")
	}
	if f.probed != nil {
		t.Errorf("probe body = %v, want no column validation on delete", f.probed)
	}
}

// TestVisualizationDeleteRejectsUnrelatedVisualization pins that the
// membership check guards the destructive command too — the one where a
// mistyped ID cannot be undone.
func TestVisualizationDeleteRejectsUnrelatedVisualization(t *testing.T) {
	f := withChart()
	srv := f.start(t)

	if _, err := runRdsh(t, srv, "", "visualization", "delete", "11", "--query", "7"); err == nil {
		t.Fatal("error = nil, want a visualization the query does not have refused")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deletedViz {
		t.Error("the delete reached the server, want the ID refused first")
	}
}

// TestVisualizationDeleteRequiresQuery covers the flag that makes the
// membership check possible; without it the command cannot tell whose
// chart it is about to remove.
func TestVisualizationDeleteRequiresQuery(t *testing.T) {
	f := withChart()
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "visualization", "delete", "9")
	if err == nil {
		t.Fatal("error = nil, want --query required")
	}
	if !strings.Contains(err.Error(), "query") {
		t.Errorf("error = %v, want it to name --query", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deletedViz {
		t.Error("the delete reached the server, want the invocation refused first")
	}
}

// TestVisualizationRejectsNonNumericID covers the argument every one of the
// three subcommands shares.
func TestVisualizationRejectsNonNumericID(t *testing.T) {
	for _, sub := range []string{"update", "delete"} {
		t.Run(sub, func(t *testing.T) {
			f := withChart()
			srv := f.start(t)

			args := []string{"visualization", sub, "nine", "--query", "7"}
			if sub == "update" {
				args = append(args, "--name", "renamed")
			}
			if _, err := runRdsh(t, srv, "", args...); err == nil {
				t.Fatal("error = nil, want a non-numeric visualization ID refused")
			}
			assertNoVisualizationSent(t, f)
		})
	}
}

// TestVisualizationCreateRejectsUnreadableOptions covers the raw mode file
// checks, both of which happen before anything is sent.
func TestVisualizationCreateRejectsUnreadableOptions(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte(`{"globalSeriesType":`), 0o600); err != nil {
		t.Fatal(err)
	}
	notAnObject := filepath.Join(dir, "array.json")
	if err := os.WriteFile(notAnObject, []byte(`["line"]`), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, file := range []string{filepath.Join(dir, "missing.json"), broken, notAnObject} {
		t.Run(filepath.Base(file), func(t *testing.T) {
			f := &fakeServer{}
			srv := f.start(t)

			_, err := runRdsh(t, srv, "", "visualization", "create", "--query", "7",
				"--name", "daily", "--type", "CHART", "--options-file", file)
			if err == nil {
				t.Fatalf("error = nil, want %s refused", filepath.Base(file))
			}
			assertNoVisualizationSent(t, f)
		})
	}
}

// assertNoVisualizationSent checks that nothing was written, for the tests
// whose whole point is that a refusal happens before any change reaches the
// server.
func assertNoVisualizationSent(t *testing.T, f *fakeServer) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createdViz != nil {
		t.Errorf("created visualization = %v, want no request at all", f.createdViz)
	}
	if f.updatedViz != nil {
		t.Errorf("updated visualization = %v, want no request at all", f.updatedViz)
	}
	if f.deletedViz {
		t.Error("a delete reached the server, want no request at all")
	}
}

// TestVisualizationTimeoutIsATimeout pins the exit code contract across
// every server call the three subcommands make. Each is a metadata call
// that leaves nothing half-done — the create either stored the chart or did
// not, exactly as `query create` reports its own expiry — so re-running is
// safe, which is what 124 tells an agent to do.
func TestVisualizationTimeoutIsATimeout(t *testing.T) {
	tests := []struct {
		name   string
		server *fakeServer
		args   []string
	}{
		{
			name:   "reading the query",
			server: &fakeServer{hangGet: true},
			args: []string{"visualization", "create", "--query", "7", "--name", "daily",
				"--type", "line", "--x", "day", "--y", "count"},
		},
		{
			name:   "probing the cached result",
			server: &fakeServer{hangProbe: true},
			args: []string{"visualization", "create", "--query", "7", "--name", "daily",
				"--type", "line", "--x", "day", "--y", "count"},
		},
		{
			name:   "creating the visualization",
			server: &fakeServer{cachedColumns: []string{"day", "count"}, hangCreateViz: true},
			args: []string{"visualization", "create", "--query", "7", "--name", "daily",
				"--type", "line", "--x", "day", "--y", "count"},
		},
		{
			name: "updating the visualization",
			server: &fakeServer{
				savedQueryVisualizations: []map[string]any{chartVisualization()},
				hangUpdateViz:            true,
			},
			args: []string{"visualization", "update", "9", "--query", "7", "--name", "renamed"},
		},
		{
			name: "deleting the visualization",
			server: &fakeServer{
				savedQueryVisualizations: []map[string]any{chartVisualization()},
				hangDeleteViz:            true,
			},
			args: []string{"visualization", "delete", "9", "--query", "7"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := tt.server.start(t)

			_, err := runRdsh(t, srv, "", append(tt.args, "--timeout", "50ms")...)
			if !errors.Is(err, errTimeout) {
				t.Fatalf("error = %v, want errTimeout", err)
			}
		})
	}
}

// TestVisualizationReadsTheQueryOnce pins that the query is fetched once per
// invocation. update needs it twice over — to find the visualization, and
// for the stored parameter defaults the column check runs against — and
// asking the server again for what it just sent is a round trip that buys
// nothing.
func TestVisualizationReadsTheQueryOnce(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "create",
			args: []string{"visualization", "create", "--query", "7", "--name", "daily",
				"--type", "line", "--x", "day", "--y", "count"},
		},
		{
			name: "update moving an axis",
			args: []string{"visualization", "update", "9", "--query", "7", "--x", "team"},
		},
		{name: "delete", args: []string{"visualization", "delete", "9", "--query", "7"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := withChart()
			srv := f.start(t)

			if _, err := runRdsh(t, srv, "", tt.args...); err != nil {
				t.Fatalf("%s error = %v", tt.name, err)
			}

			f.mu.Lock()
			defer f.mu.Unlock()
			if f.fetchCount != 1 {
				t.Errorf("GET /api/queries/7 arrived %d times, want 1", f.fetchCount)
			}
		})
	}
}

// TestVisualizationRejectsColumnRoleCollisions covers the way the typed
// flags could still store a chart that renders nothing — the failure the
// column check exists to prevent. columnMapping is keyed by column name, so
// naming one column for two roles is not a conflict the map can hold: the
// second assignment replaces the first and the axis it displaced is simply
// gone. Every name involved exists in the result, so the column check
// passes and Redash stores it without complaint.
func TestVisualizationRejectsColumnRoleCollisions(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			// Both roles asked for the same column, so the mapping ends up
			// with a y and no x at all.
			name: "create with one column on both axes",
			args: []string{"visualization", "create", "--query", "7", "--name", "daily",
				"--type", "line", "--x", "day", "--y", "day"},
		},
		{
			// count is the stored y column; moving it to x leaves the chart
			// with no y column.
			name: "update moving the y column onto x",
			args: []string{"visualization", "update", "9", "--query", "7", "--x", "count"},
		},
		{
			// day is the stored x column; the mirror of the case above.
			name: "update moving the x column onto y",
			args: []string{"visualization", "update", "9", "--query", "7", "--y", "day"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := withChart()
			srv := f.start(t)

			_, err := runRdsh(t, srv, "", tt.args...)
			if err == nil {
				t.Fatal("error = nil, want a mapping that loses an axis refused")
			}
			assertNoVisualizationSent(t, f)
		})
	}
}

// TestVisualizationUpdateSwapsBothAxes is the case the check above must not
// catch: naming both axes at once moves each column to the other role, which
// leaves the chart with an x and a y and is a perfectly ordinary edit.
func TestVisualizationUpdateSwapsBothAxes(t *testing.T) {
	f := withChart()
	srv := f.start(t)

	if _, err := runRdsh(t, srv, "", "visualization", "update", "9", "--query", "7",
		"--x", "count", "--y", "day"); err != nil {
		t.Fatalf("visualization update error = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	want := map[string]any{"count": "x", "day": "y"}
	if got := vizOptions(t, f.updatedViz)["columnMapping"]; !reflect.DeepEqual(got, want) {
		t.Errorf("columnMapping = %#v, want %#v", got, want)
	}
}

// TestVisualizationUpdateRejectsIgnoredParam covers a flag that would
// otherwise be accepted and silently discarded: --param only picks which
// cached result the column check runs against, and no check runs when no
// column changes.
func TestVisualizationUpdateRejectsIgnoredParam(t *testing.T) {
	f := withChart()
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "visualization", "update", "9", "--query", "7",
		"--name", "renamed", "--param", "days=30")
	if err == nil {
		t.Fatal("error = nil, want --param refused when nothing it applies to changes")
	}
	if !strings.Contains(err.Error(), "--param") {
		t.Errorf("error = %v, want it to name --param", err)
	}
	assertNoVisualizationSent(t, f)
}

// TestVisualizationUpdateClearsOptions covers the raw-mode edit that resets
// a visualization to the front end's own defaults. An empty object is a
// value, not an absent field: dropping it would report success for a call
// that changed nothing.
func TestVisualizationUpdateClearsOptions(t *testing.T) {
	file := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(file, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := withChart()
	srv := f.start(t)

	if _, err := runRdsh(t, srv, "", "visualization", "update", "9", "--query", "7",
		"--options-file", file); err != nil {
		t.Fatalf("visualization update error = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	options, ok := f.updatedViz["options"]
	if !ok {
		t.Fatalf("updated body = %v, want an options key carrying the empty object", f.updatedViz)
	}
	if !reflect.DeepEqual(options, map[string]any{}) {
		t.Errorf("updated options = %#v, want an empty object", options)
	}
}

// TestVisualizationCacheMissQuotesParamValues covers the command the
// cache-miss error tells the caller to run. An agent reading that error is
// likely to run it as it stands, so a value with a space in it has to come
// back as one argument rather than two.
func TestVisualizationCacheMissQuotesParamValues(t *testing.T) {
	f := &fakeServer{} // nothing cached
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "visualization", "create", "--query", "7", "--name", "daily",
		"--type", "line", "--x", "day", "--y", "count",
		"--param", "team=core eu", "--param", "days=30")
	if err == nil {
		t.Fatal("error = nil, want a query with no cached result refused")
	}
	// days needs no quoting and must not get any; team does.
	if want := "--param days=30"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to carry %q unquoted", err, want)
	}
	if want := `--param 'team=core eu'`; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to carry %s", err, want)
	}
}
