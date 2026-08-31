//go:build e2e

package e2e

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// planPattern is the pattern the text-pattern parameter below is defined
// with, written once so the flag and the definition read back cannot drift.
const planPattern = "^(free|team|enterprise)$"

// TestParameterDefinitionsSurviveARoundTrip covers what the three --param-*
// flags write: Redash stores an options object without validating it, so a
// definition that is subtly wrong is stored without complaint and shows up
// only as a query page whose result never links.
func TestParameterDefinitionsSurviveARoundTrip(t *testing.T) {
	id := createParametrizedQuery(t)

	want := wantParameters("since", "2026-01-01")
	if got := storedParameters(t, id); !reflect.DeepEqual(got, want) {
		t.Errorf("options.parameters = %+v, want %+v", got, want)
	}
}

// TestQueryUpdateKeepsWhatNoFlagNames covers the other half of the round
// trip. An update replaces the query's whole options object, so everything
// the invocation says nothing about has to be composed back into it from
// what was read — and what is on the query was not necessarily put there by
// rdsh.
func TestQueryUpdateKeepsWhatNoFlagNames(t *testing.T) {
	id := createParametrizedQuery(t)

	// Both of these are written straight through the API, because the point
	// is that rdsh carries back keys it was never told about: a title is
	// what the Redash UI sets on every definition it writes and no rdsh flag
	// can, and apply_auto_limit is a key of the options object itself rather
	// than of any parameter.
	const title = "Signed up since"
	options, version := storedOptions(t, id)
	parametersOf(t, options)[0]["title"] = title
	options["apply_auto_limit"] = true
	writeOptions(t, id, options, version)

	runRdsh(t, "query", "update", strconv.Itoa(id), "--param-default", "since=2026-02-01").assertExit(t, 0)

	updated, _ := storedOptions(t, id)
	want := wantParameters(title, "2026-02-01")
	if got := parametersOf(t, updated); !reflect.DeepEqual(got, want) {
		t.Errorf("options.parameters = %+v, want %+v", got, want)
	}
	if got := updated["apply_auto_limit"]; got != true {
		t.Errorf("options.apply_auto_limit = %v, want it left as it was", got)
	}
}

// TestQueryUpdateRefusesAParameterTypeItCannotWrite covers the refusal that
// keeps rdsh from destroying a definition it cannot express: an enum's whole
// definition is more than a name, a type and a scalar default, so rewriting
// one through the --param-* flags would silently drop the enumOptions that
// make it work.
func TestQueryUpdateRefusesAParameterTypeItCannotWrite(t *testing.T) {
	id := createParametrizedQuery(t)

	options, version := storedOptions(t, id)
	plan := parametersOf(t, options)[2]
	plan["type"] = "enum"
	plan["enumOptions"] = "free\nteam\nenterprise"
	delete(plan, "regex")
	writeOptions(t, id, options, version)
	before := storedParametersJSON(t, id)

	got := runRdsh(t, "query", "update", strconv.Itoa(id), "--param-default", "plan=team")
	got.assertExit(t, 1)
	if want := "plan is defined as enum, which rdsh cannot rewrite"; !strings.Contains(got.stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", got.stderr, want)
	}

	// The refusal is only worth anything if nothing was sent: a refusal that
	// still saved the query would be the silent rewrite it exists to prevent.
	if after := storedParametersJSON(t, id); after != before {
		t.Errorf("options.parameters = %s, want them left at %s", after, before)
	}
}

// createParametrizedQuery saves a query carrying one definition of every
// shape the three --param-* flags can write, and returns its ID. They go
// together because every placeholder the SQL uses has to be covered or the
// create is refused.
func createParametrizedQuery(t *testing.T) int {
	t.Helper()

	sql := nonced(t, "SELECT * FROM signups "+
		"WHERE signed_up_on >= '{{since}}' AND seats >= {{seats}} AND plan = '{{plan}}'")
	created := runRdsh(t, "query", "create",
		"--name", uniqueName(t),
		"--data-source", dataSource,
		"--param-default", "since=2026-01-01", "--param-type", "since=date",
		"--param-default", "seats=5", "--param-type", "seats=number",
		// No --param-type for this one: --param-regex implies text-pattern.
		"--param-default", "plan=free", "--param-regex", "plan="+planPattern,
		// The SQL begins with the nonce's line comment, which pflag would
		// otherwise read as a flag.
		"--", sql)
	created.assertExit(t, 0)
	return queryID(t, created.stdout)
}

// storedOptions reads the query's options object as Redash holds it, along
// with the version an update of it has to carry. Both come from one read
// because they belong to each other: a version fetched separately would not
// be the one the options were read at.
//
// The options have to come from the API at all because writeQueryDetail
// prints a query's SQL and metadata and not its options, so no rdsh output
// carries the definitions.
func storedOptions(t *testing.T, id int) (map[string]any, any) {
	t.Helper()
	query := getQuery(t, id)
	options, ok := query["options"].(map[string]any)
	if !ok {
		t.Fatalf("query %d carries no options object", id)
	}
	return options, query["version"]
}

func storedParameters(t *testing.T, id int) []map[string]any {
	t.Helper()
	options, _ := storedOptions(t, id)
	return parametersOf(t, options)
}

// storedParametersJSON is the definitions as the JSON Redash holds them,
// read without decoding on the way. The assertions about what an update left
// alone go through here rather than through storedParameters because a
// decoded number is a float64: a default past 2^53 that rdsh had rewritten
// would round to the same float64 as the one it replaced, and the comparison
// meant to catch exactly that would pass.
func storedParametersJSON(t *testing.T, id int) string {
	t.Helper()
	var query struct {
		Options struct {
			Parameters jsontext.Value `json:"parameters"`
		} `json:"options"`
	}
	if err := json.Unmarshal(redashRaw(t, queryPath(id)), &query); err != nil {
		t.Fatalf("reading query %d: %v", id, err)
	}
	return string(query.Options.Parameters)
}

// wantParameters is what createParametrizedQuery writes, with the one entry
// the update below touches left to the caller. Written once so that what an
// update has to leave alone is the same literal on both sides of it.
func wantParameters(sinceTitle, sinceValue string) []map[string]any {
	return []map[string]any{
		{"name": "since", "title": sinceTitle, "type": "date", "value": sinceValue},
		// A number default is stored as JSON's number rather than as the text
		// it was typed as, which is how the Redash UI writes one — and the
		// query hashes to the same text either tool saved it. A JSON number
		// decodes to a float64, which is what tells it from a string here.
		{"name": "seats", "title": "seats", "type": "number", "value": float64(5)},
		{"name": "plan", "title": "plan", "type": "text-pattern", "value": "free", "regex": planPattern},
	}
}

// parametersOf returns the definitions in the order they are stored. The
// entries are the maps the options object holds, so a caller may edit one in
// place and write the object back.
func parametersOf(t *testing.T, options map[string]any) []map[string]any {
	t.Helper()

	entries, ok := options["parameters"].([]any)
	if !ok {
		t.Fatalf("the options object carries no parameters: %+v", options)
	}
	params := make([]map[string]any, len(entries))
	for i, entry := range entries {
		p, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("parameter %d is %T, want an object", i, entry)
		}
		params[i] = p
	}
	return params
}

// writeOptions replaces the query's whole options object through Redash's
// own API, which is how a definition rdsh cannot write gets onto a query in
// the first place. The whole object has to be sent: Redash replaces it
// rather than merging into it, so a partial write would delete the very
// definitions these tests go on to check.
func writeOptions(t *testing.T, id int, options map[string]any, version any) {
	t.Helper()
	redashDo(t, http.MethodPost, queryPath(id), map[string]any{
		"options": options,
		"version": version,
	})
}
