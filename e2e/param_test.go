//go:build e2e

package e2e

import (
	"encoding/json"
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

	want := []map[string]any{
		{"name": "since", "title": "since", "type": "date", "value": "2026-01-01"},
		// A number default is stored as JSON's number rather than as the
		// text it was typed as, which is how the Redash UI writes one — and
		// the query hashes to the same text either tool saved it.
		{"name": "seats", "title": "seats", "type": "number", "value": json.Number("5")},
		{"name": "plan", "title": "plan", "type": "text-pattern", "value": "free", "regex": planPattern},
	}
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
	options := storedOptions(t, id)
	parametersOf(t, options)[0]["title"] = title
	options["apply_auto_limit"] = true
	writeOptions(t, id, options)

	runRdsh(t, "query", "update", strconv.Itoa(id), "--param-default", "since=2026-02-01").assertExit(t, 0)

	updated := storedOptions(t, id)
	want := []map[string]any{
		{"name": "since", "title": title, "type": "date", "value": "2026-02-01"},
		{"name": "seats", "title": "seats", "type": "number", "value": json.Number("5")},
		{"name": "plan", "title": "plan", "type": "text-pattern", "value": "free", "regex": planPattern},
	}
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

	options := storedOptions(t, id)
	plan := parametersOf(t, options)[2]
	plan["type"] = "enum"
	plan["enumOptions"] = "free\nteam\nenterprise"
	delete(plan, "regex")
	writeOptions(t, id, options)
	before := storedParameters(t, id)

	got := runRdsh(t, "query", "update", strconv.Itoa(id), "--param-default", "plan=team")
	got.assertExit(t, 1)
	if want := "plan is defined as enum, which rdsh cannot rewrite"; !strings.Contains(got.stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", got.stderr, want)
	}

	// The refusal is only worth anything if nothing was sent: a refusal that
	// still saved the query would be the silent rewrite it exists to prevent.
	if after := storedParameters(t, id); !reflect.DeepEqual(after, before) {
		t.Errorf("options.parameters = %+v, want them left at %+v", after, before)
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

// storedOptions reads the query's options object as Redash holds it. It has
// to come from the API: writeQueryDetail prints a query's SQL and metadata
// and not its options, so no rdsh output carries the definitions.
func storedOptions(t *testing.T, id int) map[string]any {
	t.Helper()
	options, ok := getQuery(t, id)["options"].(map[string]any)
	if !ok {
		t.Fatalf("query %d carries no options object", id)
	}
	return options
}

func storedParameters(t *testing.T, id int) []map[string]any {
	t.Helper()
	return parametersOf(t, storedOptions(t, id))
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
func writeOptions(t *testing.T, id int, options map[string]any) {
	t.Helper()
	redashDo(t, http.MethodPost, queryPath(id), map[string]any{
		"options": options,
		"version": getQuery(t, id)["version"],
	})
}
