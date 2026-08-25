//go:build e2e

package e2e

import (
	"strconv"
	"testing"
)

// TestExecutingASavedQueryLinksItsResult covers the order that holds on
// every Redash release: Query.update_latest_result links a result when the
// saved query is executed, and always has. The reverse order — `rdsh run`
// then `rdsh query create` — depends on the backfill added in 26.3.0 and is
// deliberately not covered here; it would fail the moment the pinned tag
// moved back.
func TestExecutingASavedQueryLinksItsResult(t *testing.T) {
	created := runRdsh(t, "query", "create",
		"--name", uniqueName(t),
		"--data-source", dataSource,
		// The SQL begins with the nonce's line comment, which pflag would
		// otherwise read as a flag.
		"--", nonced(t, "SELECT count(*) AS n FROM signups"))
	created.assertExit(t, 0)
	id := queryID(t, created.stdout)

	// The nonce in the SQL is what makes this assertion mean anything: on a
	// release that backfills at save time, a query whose text an earlier run
	// already executed would arrive carrying that result, and the refresh
	// below would have nothing left to prove.
	if linked := getQuery(t, id)["latest_query_data_id"]; linked != nil {
		t.Fatalf("a query nobody has executed carries result %v, want none", linked)
	}

	refreshed := runRdsh(t, "query", "refresh", strconv.Itoa(id))
	refreshed.assertExit(t, 0)
	if got, want := refreshed.stdout, "n\n40\n"; got != want {
		t.Errorf("query refresh stdout = %q, want %q", got, want)
	}

	if getQuery(t, id)["latest_query_data_id"] == nil {
		t.Error("the executed query carries no result, want the one the refresh produced")
	}
}
