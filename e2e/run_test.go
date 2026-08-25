//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// TestRunPrintsTheSeededRows covers the whole asynchronous lifecycle from
// the outside: rdsh submits a job, polls it and prints what it produced.
//
// The counts come from scripts/redash-seed.sql, which generates its rows
// deterministically — the two move together, and CLAUDE.md says so.
func TestRunPrintsTheSeededRows(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "every seeded signup",
			sql:  "SELECT count(*) AS n FROM signups",
			want: "n\n41\n",
		},
		{
			// A proper subset rather than every row, so a filter that
			// silently matched everything would not pass.
			name: "the signups of this year",
			sql:  "SELECT count(*) AS n FROM signups WHERE signed_up_on >= '2026-01-01'",
			want: "n\n20\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runRdsh(t, "run", "--data-source", dataSource, tt.sql)
			got.assertExit(t, 0)
			if got.stdout != tt.want {
				t.Errorf("run stdout = %q, want %q", got.stdout, tt.want)
			}
		})
	}
}

// TestRunRejectedSQLReportsTheServersOwnMessage covers the shape of a
// failure: Redash answers SQL the data source rejects with a failed job
// rather than an HTTP error, and what the caller can act on is the message
// the database itself produced.
func TestRunRejectedSQLReportsTheServersOwnMessage(t *testing.T) {
	got := runRdsh(t, "run", "--data-source", dataSource, "SELECT * FROM no_such_table")
	got.assertExit(t, 1)

	if !strings.Contains(got.stderr, "no_such_table") {
		t.Errorf("stderr = %q, want the server's own message naming no_such_table", got.stderr)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want nothing on the stream results are read from", got.stdout)
	}
}

// TestRunTimeoutExitsWithTheTimeoutCodeAndCancelsTheJob covers what a
// --timeout expiry owes an agent consumer: exit code 124, so "retry with a
// longer --timeout" can be told from every other failure, and a cancellation
// the server accepted — abandonJob appends its "additionally, cancelling
// job" suffix when the DELETE fails, which is what breaks if Redash moves
// the endpoint.
//
// That the query itself stopped running is deliberately not asserted: rdsh
// prints no job ID on the success path, Redash has no endpoint that maps a
// query to one, and PostgreSQL ships with client_connection_check_interval
// at 0, so a pg_sleep backend runs to completion even after its client is
// gone.
func TestRunTimeoutExitsWithTheTimeoutCodeAndCancelsTheJob(t *testing.T) {
	// Well short of the sleep, and well beyond the round trip that submits
	// the job. Shaving it further risks the deadline expiring during the
	// submission itself, which leaves no job ID to cancel: the run would
	// still exit 124 and still carry no suffix, and the cancellation this
	// test exists for would go unexercised.
	got := runRdsh(t, "run", "--data-source", dataSource, "--timeout", "5s", "SELECT pg_sleep(60)")
	got.assertExit(t, 124)

	if !strings.Contains(got.stderr, "timed out") {
		t.Errorf("stderr = %q, want it to report the timeout", got.stderr)
	}
	if strings.Contains(got.stderr, "additionally, cancelling job") {
		t.Errorf("stderr = %q, want the cancellation to have been accepted", got.stderr)
	}
}
