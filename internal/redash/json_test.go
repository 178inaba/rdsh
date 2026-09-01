package redash_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/178inaba/rdsh/internal/redash"
)

// rawServer serves the given bodies verbatim. The other tests in this package
// build their responses with an encoder, which cannot produce the two things
// these tests are about — a pre-escaped \uXXXX sequence and object members out
// of order — because an encoder normalises both on the way out.
func rawServer(t *testing.T, bodies map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			t.Errorf("unexpected request for %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGetQueryResultNormalisesRows pins the normalisation the rows go through
// on the way in. Rows arrive as raw JSON so that a number keeps the text the
// warehouse wrote, but the bytes rdsh prints are a contract (see CLAUDE.md),
// and before the move to raw values they were whatever an encoder produced
// from decoded Go values. Normalising here is what keeps that output the same;
// without it a pre-escaped sequence would reach the terminal unexpanded and an
// object's members would keep the server's order.
func TestGetQueryResultNormalisesRows(t *testing.T) {
	srv := rawServer(t, map[string]string{
		"/api/query_results/42": `{"query_result":{"data":{
			"columns":[{"name":"n"},{"name":"note"},{"name":"obj"},{"name":"nul"}],
			"rows":[
				{"n":9007199254740993,"note":"caf\u00e9","obj":{"z":1,"a":2},"nul":null},
				{"n":2}
			]}}}`,
	})

	got, err := newTestClient(srv, "k").GetQueryResult(t.Context(), 42)
	if err != nil {
		t.Fatalf("GetQueryResult: %v", err)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(got.Rows))
	}

	for _, tt := range []struct {
		column string
		want   string
	}{
		// The integer is past what a float64 holds exactly, so its text is
		// the only way it survives; normalising must not rewrite it.
		{"n", "9007199254740993"},
		// Expanded, because that is what the pre-raw output printed.
		{"note", `"café"`},
		// Sorted, for the same reason.
		{"obj", `{"a":2,"z":1}`},
		// An explicit null stays a value: it is distinguishable from the
		// column being absent, which the second row exercises.
		{"nul", "null"},
	} {
		if got := string(got.Rows[0][tt.column]); got != tt.want {
			t.Errorf("row 0 %s = %s, want %s", tt.column, got, tt.want)
		}
	}

	// A column the row does not carry reads back as the zero value, which is
	// what tells it apart from a column carrying null.
	if v, ok := got.Rows[1]["note"]; ok || v != nil {
		t.Errorf("row 1 note = %q (present %t), want absent", v, ok)
	}
}

// TestQueryOptionsKeepUnknownMembersVerbatim pins the round trip an update
// depends on: Redash replaces the whole options object, so every key rdsh has
// no field for has to survive being read and written back exactly.
func TestQueryOptionsKeepUnknownMembersVerbatim(t *testing.T) {
	srv := rawServer(t, map[string]string{
		"/api/queries/7": `{"id":7,"options":{
			"apply_auto_limit":true,
			"deep":{"b":[{"c":"x<y"}]},
			"esc":"\u65e5\u672c",
			"parameters":[
				{"name":"id","type":"number","value":9007199254740993,"nested":{"z":1,"a":2}},
				{"name":"since","type":"date","value":null}
			]}}`,
	})

	q, err := newTestClient(srv, "k").GetQuery(t.Context(), 7)
	if err != nil {
		t.Fatalf("GetQuery: %v", err)
	}

	for key, want := range map[string]string{
		"apply_auto_limit": "true",
		// Nested values and pre-escaped text are held as they arrived: rdsh
		// does not interpret them, so it must not rewrite them either.
		"deep": `{"b":[{"c":"x<y"}]}`,
		"esc":  `"\u65e5\u672c"`,
	} {
		if got := string(q.Options.Extra[key]); got != want {
			t.Errorf("Extra[%q] = %s, want %s", key, got, want)
		}
	}
	if _, ok := q.Options.Extra["parameters"]; ok {
		t.Error("Extra carries parameters, which QueryOptions has a field for")
	}

	if len(q.Options.Parameters) != 2 {
		t.Fatalf("parameters = %d, want 2", len(q.Options.Parameters))
	}
	// The default keeps the text Redash hashed the query with, which is the
	// whole reason it is not decoded into a number.
	if got, want := string(q.Options.Parameters[0].Value), "9007199254740993"; got != want {
		t.Errorf("parameters[0].Value = %s, want %s", got, want)
	}
	if got, want := string(q.Options.Parameters[0].Extra["nested"]), `{"z":1,"a":2}`; got != want {
		t.Errorf("parameters[0].Extra[nested] = %s, want %s", got, want)
	}
	// An explicit null is a value here too. What it means — no default — is
	// decided where the default is read, not on the way in.
	if got, want := string(q.Options.Parameters[1].Value), "null"; got != want {
		t.Errorf("parameters[1].Value = %s, want %s", got, want)
	}
}

// TestGetQueryNullOptionsIsZero pins that a null options object reads as no
// options rather than as a failure. It reaches a struct field where a plain
// decode would skip it, so it is worth stating even though nothing in rdsh
// spells the behaviour out any more.
func TestGetQueryNullOptionsIsZero(t *testing.T) {
	srv := rawServer(t, map[string]string{
		"/api/queries/7": `{"id":7,"options":null}`,
	})

	q, err := newTestClient(srv, "k").GetQuery(t.Context(), 7)
	if err != nil {
		t.Fatalf("GetQuery: %v", err)
	}
	if want := (redash.QueryOptions{}); !reflect.DeepEqual(q.Options, want) {
		t.Errorf("Options = %+v, want %+v", q.Options, want)
	}
}

// TestQueryOptionsRejectStrictViolations pins the defaults the move to v2 was
// for: a response with a duplicate member or invalid UTF-8 is an error rather
// than something quietly repaired.
func TestQueryOptionsRejectStrictViolations(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate member": `{"id":7,"options":{"a":1,"a":2}}`,
		"invalid UTF-8":    "{\"id\":7,\"name\":\"a\xffb\"}",
	} {
		t.Run(name, func(t *testing.T) {
			srv := rawServer(t, map[string]string{"/api/queries/7": body})
			if _, err := newTestClient(srv, "k").GetQuery(t.Context(), 7); err == nil {
				t.Error("GetQuery succeeded, want an error")
			}
		})
	}
}
