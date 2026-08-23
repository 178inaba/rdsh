package cmd

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// savedQueryURL is where the query the fake saves is read in a browser:
// what create prints on success, and what a failed publish has to report.
func savedQueryURL(srv *httptest.Server) string {
	return fmt.Sprintf("%s/queries/%d", srv.URL, savedQueryID)
}

func TestQueryCreatePublishesAndPrintsURL(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)

	out, err := runRdsh(t, srv, "", "query", "create", "--name", "signups", "SELECT 1", "--data-source", "5")
	if err != nil {
		t.Fatalf("query create error = %v", err)
	}
	if want := savedQueryURL(srv) + "\n"; out != want {
		t.Errorf("output = %q, want %q and nothing else", out, want)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.created["name"] != "signups" || f.created["query"] != "SELECT 1" {
		t.Errorf("created query = %v, want the name and SQL that were passed", f.created)
	}
	if want := map[string]any{"is_draft": false}; fmt.Sprint(f.published) != fmt.Sprint(want) {
		t.Errorf("publish body = %v, want %v", f.published, want)
	}
}

// TestQueryCreateDraft covers the flag that keeps a query out of everyone
// else's list: the second call is what publishes, so --draft must skip it
// rather than send is_draft true (which is what the query already is).
func TestQueryCreateDraft(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)

	out, err := runRdsh(t, srv, "", "query", "create", "--name", "part", "--draft", "SELECT 1", "--data-source", "5")
	if err != nil {
		t.Fatalf("query create --draft error = %v", err)
	}
	if want := savedQueryURL(srv) + "\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.published != nil {
		t.Errorf("publish body = %v, want no publish request at all", f.published)
	}
}

// TestQueryCreateSQLChannels checks that create takes SQL through the same
// three channels as run, with the same conflict rule.
func TestQueryCreateSQLChannels(t *testing.T) {
	file := filepath.Join(t.TempDir(), "q.sql")
	if err := os.WriteFile(file, []byte("SELECT 'from file'"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		stdin   string
		args    []string
		wantSQL string
		wantErr string
	}{
		{name: "argument", args: []string{"SELECT 'from arg'"}, wantSQL: "SELECT 'from arg'"},
		{name: "file", args: []string{"-f", file}, wantSQL: "SELECT 'from file'"},
		{name: "stdin", stdin: "SELECT 'from stdin'", wantSQL: "SELECT 'from stdin'"},
		{name: "argument and file conflict", args: []string{"SELECT 1", "-f", file}, wantErr: "-f"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeServer{}
			srv := f.start(t)

			args := append([]string{"query", "create", "--name", "n"}, tt.args...)
			_, err := runRdsh(t, srv, tt.stdin, append(args, "--data-source", "5")...)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one mentioning %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("query create error = %v", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.created["query"] != tt.wantSQL {
				t.Errorf("created query = %v, want SQL %q", f.created, tt.wantSQL)
			}
		})
	}
}

func TestQueryCreateRequiresName(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "query", "create", "SELECT 1", "--data-source", "5")
	if err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("error = %v, want an error naming --name", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.created != nil {
		t.Errorf("created query = %v, want nothing saved", f.created)
	}
}

// TestQueryCreatePublishFailureReportsDraft covers the deliberate exception
// to the exit code contract: a create that saved the query but could not
// publish it must report the URL, because the query exists and re-running
// create would save a second one.
func TestQueryCreatePublishFailureReportsDraft(t *testing.T) {
	f := &fakeServer{rejectPublish: true}
	srv := f.start(t)

	out, err := runRdsh(t, srv, "", "query", "create", "--name", "signups", "SELECT 1", "--data-source", "5")
	if err == nil {
		t.Fatalf("output = %q, want a reported failure", out)
	}
	if errors.Is(err, errTimeout) {
		t.Errorf("error = %v, want an ordinary failure rather than a timeout", err)
	}
	assertUnpublishedReport(t, err, srv)
}

// TestQueryCreateTimeoutIsATimeout pins the ordinary half of the exit code
// contract for create: a deadline that stops the save itself is reported as
// a timeout, so an agent re-runs with a longer one. Nothing was saved, or
// it was and its ID never arrived, so there is nothing else to report.
func TestQueryCreateTimeoutIsATimeout(t *testing.T) {
	f := &fakeServer{hangCreate: true}
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "query", "create", "--name", "signups", "SELECT 1",
		"--data-source", "5", "--timeout", "50ms")
	if !errors.Is(err, errTimeout) {
		t.Fatalf("error = %v, want errTimeout", err)
	}
}

// TestQueryCreatePublishTimeoutIsNotATimeout pins the same exception for a
// publish the --timeout cut off: 124 would tell an agent to re-run as is,
// and re-running create saves the query twice.
func TestQueryCreatePublishTimeoutIsNotATimeout(t *testing.T) {
	f := &fakeServer{hangPublish: true}
	srv := f.start(t)

	// The create before it answers at once, so only the parked publish can
	// reach the deadline.
	out, err := runRdsh(t, srv, "", "query", "create", "--name", "signups", "SELECT 1",
		"--data-source", "5", "--timeout", "50ms")
	if err == nil {
		t.Fatalf("output = %q, want a reported failure", out)
	}
	if errors.Is(err, errTimeout) {
		t.Errorf("error = %v, want exit 1 rather than the timeout code", err)
	}
	assertUnpublishedReport(t, err, srv)
}

// assertUnpublishedReport checks what a caller needs to recover from a
// create whose publish failed: that the query exists as a draft, and where.
func assertUnpublishedReport(t *testing.T, err error, srv *httptest.Server) {
	t.Helper()
	url := savedQueryURL(srv)
	if !strings.Contains(err.Error(), url) {
		t.Errorf("error = %v, want the query URL %s", err, url)
	}
	if !strings.Contains(err.Error(), "draft") {
		t.Errorf("error = %v, want it to say the query remains a draft", err)
	}
}

// TestQueryGroupArguments covers what a group command does with something
// that is not one of its subcommands. cobra answers that by printing help
// to stdout and exiting 0 (#27), which would make the pre-rename `rdsh
// query "SELECT 1"` look like a success — and the rename deliberately left
// no alias and no migration hint, so an old call has to fail rather than
// quietly do nothing.
func TestQueryGroupArguments(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no arguments prints help", args: []string{"query"}},
		{name: "mistyped subcommand", args: []string{"query", "creat"}, wantErr: "unknown command"},
		{name: "pre-rename invocation", args: []string{"query", "SELECT 1"}, wantErr: "unknown command"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// No server: none of these resolves a connection.
			out, err := runRdsh(t, nil, "", tt.args...)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("error = %v, want the group's help", err)
				}
				if !strings.Contains(out, "create") {
					t.Errorf("output = %q, want the help listing the subcommands", out)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want one mentioning %s", err, tt.wantErr)
			}
		})
	}
}
