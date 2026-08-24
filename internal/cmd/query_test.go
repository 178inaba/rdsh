package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/178inaba/rdsh/internal/redash"
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
	if want := map[string]any{"is_draft": false}; fmt.Sprint(f.updated) != fmt.Sprint(want) {
		t.Errorf("publish body = %v, want %v", f.updated, want)
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
	if f.updated != nil {
		t.Errorf("publish body = %v, want no publish request at all", f.updated)
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
	f := &fakeServer{rejectUpdate: true}
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

// TestQueryCreateDataSourceLookupTimeout covers create's half of the gap
// run's lookup had: resolving a --data-source name is a server call, so a
// deadline that expires in it is a timeout. Only the code is checked here —
// the message and the untouched ID path are pinned in
// TestRunDataSourceLookupTimeout, since both commands resolve the name
// through the same call.
func TestQueryCreateDataSourceLookupTimeout(t *testing.T) {
	f := &fakeServer{hangList: true}
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "query", "create", "--name", "signups", "SELECT 1",
		"--data-source", "warehouse", "--timeout", "50ms")
	if !errors.Is(err, errTimeout) {
		t.Fatalf("error = %v, want errTimeout", err)
	}
}

// TestQueryCreatePublishTimeoutIsNotATimeout pins the same exception for a
// publish the --timeout cut off: 124 would tell an agent to re-run as is,
// and re-running create saves the query twice.
func TestQueryCreatePublishTimeoutIsNotATimeout(t *testing.T) {
	f := &fakeServer{hangUpdate: true}
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

// TestQueryGroupArguments covers what this group does with something that
// is not one of its subcommands: the root's help override reports it, and
// the failure reaches a caller as an error rather than as the nil cobra
// returns for it. What makes it worth pinning here as well as in
// execute_test.go is the pre-rename `rdsh query "SELECT 1"` — the rename
// deliberately left no alias and no migration hint, so an old call has to
// fail rather than quietly do nothing.
func TestQueryGroupArguments(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr []string // every one of these appears in the error
	}{
		{name: "no arguments prints help", args: []string{"query"}},
		{
			name:    "mistyped subcommand",
			args:    []string{"query", "creat"},
			wantErr: []string{"unknown command", "Did you mean this?", "create"},
		},
		{
			// Nothing this far from a subcommand name yields a candidate.
			name:    "pre-rename invocation",
			args:    []string{"query", "SELECT 1"},
			wantErr: []string{"unknown command"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// No server: none of these resolves a connection.
			out, err := runRdsh(t, nil, "", tt.args...)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("error = %v, want the group's help", err)
				}
				if !strings.Contains(out, "create") {
					t.Errorf("output = %q, want the help listing the subcommands", out)
				}
				return
			}
			if err == nil {
				t.Fatalf("error = nil, want one mentioning %s", tt.wantErr[0])
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want one mentioning %s", err, want)
				}
			}
		})
	}
}

// TestResolveQueryID covers the argument both update and show take: an ID,
// or the URL a browser copied from the address bar. The prefix has to
// match through /queries/ rather than the base URL alone, or a lookalike
// host would send an update to another instance.
func TestResolveQueryID(t *testing.T) {
	const base = "https://redash.example.com"
	tests := []struct {
		name    string
		arg     string
		base    string
		want    int
		wantErr bool
	}{
		{name: "id", arg: "7", want: 7},
		{name: "url", arg: base + "/queries/7", want: 7},
		{name: "url with trailing segment", arg: base + "/queries/7/source", want: 7},
		{name: "url with query string and fragment", arg: base + "/queries/7?p_x=1#fragment", want: 7},
		{name: "base URL with a trailing slash", arg: base + "/queries/7", base: base + "/", want: 7},
		{name: "another instance", arg: "https://redash.other.test/queries/7", wantErr: true},
		{name: "lookalike host", arg: base + ".evil.test/queries/7", wantErr: true},
		{name: "another resource", arg: base + "/dashboards/7", wantErr: true},
		{name: "non-numeric id", arg: base + "/queries/mine", wantErr: true},
		{name: "no id at all", arg: base + "/queries/", wantErr: true},
		{name: "empty", arg: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.base
			if b == "" {
				b = base
			}
			got, err := resolveQueryID(tt.arg, redash.NewClient(b, "k"))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveQueryID(%q) = %d, want an error", tt.arg, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveQueryID(%q) error = %v", tt.arg, err)
			}
			if got != tt.want {
				t.Errorf("resolveQueryID(%q) = %d, want %d", tt.arg, got, tt.want)
			}
		})
	}
}

// TestQueryUpdateFields checks that each field can be changed on its own and
// in combination, and that the request carries nothing else: an update that
// also sent the fields it was not asked to change would overwrite an edit
// made in the Redash UI since the query was read. The version comes with
// every one of them, which is what makes such an edit fail the update
// instead of being lost.
func TestQueryUpdateFields(t *testing.T) {
	file := filepath.Join(t.TempDir(), "q.sql")
	if err := os.WriteFile(file, []byte("SELECT 'from file'"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		args []string
		want map[string]any
	}{
		{name: "name", args: []string{"--name", "renamed"}, want: map[string]any{"name": "renamed"}},
		{
			name: "description",
			args: []string{"--description", "why it exists"},
			want: map[string]any{"description": "why it exists"},
		},
		{
			name: "description cleared",
			args: []string{"--description", ""},
			want: map[string]any{"description": ""},
		},
		{name: "sql from an argument", args: []string{"SELECT 2"}, want: map[string]any{"query": "SELECT 2"}},
		{name: "sql from a file", args: []string{"-f", file}, want: map[string]any{"query": "SELECT 'from file'"}},
		{name: "publish", args: []string{"--publish"}, want: map[string]any{"is_draft": false}},
		{name: "draft", args: []string{"--draft"}, want: map[string]any{"is_draft": true}},
		{
			name: "several at once",
			args: []string{"SELECT 2", "--name", "renamed", "--publish"},
			want: map[string]any{"query": "SELECT 2", "name": "renamed", "is_draft": false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeServer{}
			srv := f.start(t)

			out, err := runRdsh(t, srv, "", append([]string{"query", "update", "7"}, tt.args...)...)
			if err != nil {
				t.Fatalf("query update error = %v", err)
			}
			if want := savedQueryURL(srv) + "\n"; out != want {
				t.Errorf("output = %q, want %q and nothing else", out, want)
			}

			want := map[string]any{"version": float64(savedQueryVersion)}
			for k, v := range tt.want {
				want[k] = v
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if !reflect.DeepEqual(f.updated, want) {
				t.Errorf("update body = %v, want %v", f.updated, want)
			}
		})
	}
}

// TestQueryUpdateTarget covers the URL form end to end: the ID reaches the
// server, and a URL on another instance is refused before anything is sent.
// The parsing itself is covered by TestResolveQueryID.
func TestQueryUpdateTarget(t *testing.T) {
	t.Run("url", func(t *testing.T) {
		f := &fakeServer{}
		srv := f.start(t)

		if _, err := runRdsh(t, srv, "", "query", "update", savedQueryURL(srv), "--name", "renamed"); err != nil {
			t.Fatalf("query update error = %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.updated["name"] != "renamed" {
			t.Errorf("update body = %v, want the new name", f.updated)
		}
	})

	t.Run("another instance", func(t *testing.T) {
		f := &fakeServer{}
		srv := f.start(t)

		_, err := runRdsh(t, srv, "", "query", "update", "https://redash.other.test/queries/7", "--name", "renamed")
		if err == nil {
			t.Fatal("error = nil, want a URL on another instance refused")
		}
		if errors.Is(err, errTimeout) {
			t.Errorf("error = %v, want an ordinary failure", err)
		}
		assertNothingSent(t, f)
	})
}

// TestQueryUpdateWithoutChanges covers the invocation that has nothing to
// do: it has to fail rather than send an empty update, which Redash would
// accept as a no-op and which would read as a successful edit.
func TestQueryUpdateWithoutChanges(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "query", "update", "7")
	if err == nil {
		t.Fatal("error = nil, want an update with nothing to change refused")
	}
	assertNothingSent(t, f)
}

// TestQueryUpdateSQLChannels pins the two ways SQL is passed and the one
// that deliberately is not. Unlike run and create, update does not fall back
// to stdin: SQL is optional here, and an agent's stdin is always non-TTY, so
// that fallback would turn whatever happened to be piped in into the query.
func TestQueryUpdateSQLChannels(t *testing.T) {
	file := filepath.Join(t.TempDir(), "q.sql")
	if err := os.WriteFile(file, []byte("SELECT 'from file'"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("argument and file conflict", func(t *testing.T) {
		f := &fakeServer{}
		srv := f.start(t)

		_, err := runRdsh(t, srv, "", "query", "update", "7", "SELECT 2", "-f", file)
		if err == nil || !strings.Contains(err.Error(), "-f") {
			t.Fatalf("error = %v, want one mentioning -f", err)
		}
		assertNothingSent(t, f)
	})

	t.Run("stdin is ignored", func(t *testing.T) {
		f := &fakeServer{}
		srv := f.start(t)

		if _, err := runRdsh(t, srv, "SELECT 'from stdin'", "query", "update", "7", "--name", "renamed"); err != nil {
			t.Fatalf("query update error = %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.updated["query"]; ok {
			t.Errorf("update body = %v, want no query key: stdin is not a SQL source here", f.updated)
		}
	})
}

// TestQueryUpdateRejectsEmptyValues covers the two fields where sending an
// empty value would destroy something rather than edit it. An unset shell
// variable is all it takes to ask for that by accident, so both are refused
// before anything reaches the server. --description is deliberately not
// among them: clearing it is a real edit, covered by TestQueryUpdateFields.
func TestQueryUpdateRejectsEmptyValues(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty.sql")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "empty name", args: []string{"--name", ""}, wantErr: "--name"},
		{name: "empty sql argument", args: []string{""}, wantErr: "empty"},
		{name: "empty sql file", args: []string{"-f", empty}, wantErr: "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeServer{}
			srv := f.start(t)

			_, err := runRdsh(t, srv, "", append([]string{"query", "update", "7"}, tt.args...)...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one mentioning %s", err, tt.wantErr)
			}
			assertNothingSent(t, f)
		})
	}
}

// TestQueryUpdatePublishAndDraftConflict covers the two flags that set the
// same field in opposite directions.
func TestQueryUpdatePublishAndDraftConflict(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "query", "update", "7", "--publish", "--draft")
	if err == nil {
		t.Fatal("error = nil, want --publish and --draft together refused")
	}
	for _, flag := range []string{"publish", "draft"} {
		if !strings.Contains(err.Error(), flag) {
			t.Errorf("error = %v, want it to name --%s", err, flag)
		}
	}
	assertNothingSent(t, f)
}

// TestQueryUpdateVersionConflict covers a query edited elsewhere between the
// read and the write: the update is refused rather than overwriting that
// edit, and the failure has to say so, because re-running the same command
// unchanged would keep failing.
func TestQueryUpdateVersionConflict(t *testing.T) {
	f := &fakeServer{conflict: true}
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "query", "update", "7", "--name", "renamed")
	if err == nil {
		t.Fatal("error = nil, want the conflict reported")
	}
	if errors.Is(err, errTimeout) {
		t.Errorf("error = %v, want an ordinary failure rather than a timeout", err)
	}
	if !strings.Contains(err.Error(), "changed on the server") {
		t.Errorf("error = %v, want it to say the query changed on the server", err)
	}
}

// TestQueryUpdateTimeoutIsATimeout pins update against create's exception:
// an update is a single write, so a deadline that cuts off either call
// leaves the query as it was and re-running is safe — which is exactly what
// exit code 124 tells an agent to do.
func TestQueryUpdateTimeoutIsATimeout(t *testing.T) {
	tests := []struct {
		name   string
		server *fakeServer
	}{
		{name: "reading the query", server: &fakeServer{hangGet: true}},
		{name: "writing the update", server: &fakeServer{hangUpdate: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := tt.server.start(t)

			_, err := runRdsh(t, srv, "", "query", "update", "7", "--name", "renamed", "--timeout", "50ms")
			if !errors.Is(err, errTimeout) {
				t.Fatalf("error = %v, want errTimeout", err)
			}
		})
	}
}

// assertNothingSent checks that an invocation refused before any request was
// made left the saved query untouched.
func assertNothingSent(t *testing.T, f *fakeServer) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetched {
		t.Error("the query was read, want the invocation refused before any request")
	}
	if f.updated != nil {
		t.Errorf("update body = %v, want no request at all", f.updated)
	}
}

// wantListHeader is the column order the listing prints. It is part of the
// agent-facing contract, so the tests pin the header rather than only the
// set of columns.
const wantListHeader = "id,name,is_draft,url"

// listRows splits a CSV listing into its header and data rows.
func listRows(t *testing.T, out string) (string, []string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) == 0 {
		t.Fatalf("output = %q, want at least a header", out)
	}
	return lines[0], lines[1:]
}

// assertNoNote checks stderr for a listing that showed everything: nothing
// must suggest there is more to see.
func assertNoNote(t *testing.T, errOut string) {
	t.Helper()
	if errOut != "" {
		t.Errorf("stderr = %q, want nothing when the listing was not truncated", errOut)
	}
}

// TestQueryList covers the default listing: the fixed column order, the
// default cap of 30 rows, and the URL column built from the ID.
func TestQueryList(t *testing.T) {
	f := &fakeServer{savedQueries: 3}
	srv := f.start(t)

	out, errOut, err := runRdshSplit(t, srv, "query", "list")
	if err != nil {
		t.Fatalf("query list error = %v", err)
	}
	assertNoNote(t, errOut)

	header, rows := listRows(t, out)
	if header != wantListHeader {
		t.Errorf("header = %q, want %q", header, wantListHeader)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %v, want 3", rows)
	}
	if want := fmt.Sprintf("1,query 1,true,%s/queries/1", srv.URL); rows[0] != want {
		t.Errorf("rows[0] = %q, want %q", rows[0], want)
	}
	if want := fmt.Sprintf("2,query 2,false,%s/queries/2", srv.URL); rows[1] != want {
		t.Errorf("rows[1] = %q, want %q", rows[1], want)
	}
}

// TestQueryListDefaultLimit pins the gh-style default of 30 rows, and that
// exceeding it is not an error.
func TestQueryListDefaultLimit(t *testing.T) {
	f := &fakeServer{savedQueries: 45}
	srv := f.start(t)

	out, _, err := runRdshSplit(t, srv, "query", "list")
	if err != nil {
		t.Fatalf("query list error = %v", err)
	}
	if _, rows := listRows(t, out); len(rows) != 30 {
		t.Errorf("rows = %d, want the default limit of 30", len(rows))
	}
}

// TestQueryListLimit covers raising and lowering the cap, including a limit
// above what the server holds.
func TestQueryListLimit(t *testing.T) {
	tests := []struct {
		name     string
		limit    string
		wantRows int
	}{
		{name: "below the default", limit: "2", wantRows: 2},
		{name: "above the default", limit: "40", wantRows: 40},
		{name: "above what the server holds", limit: "500", wantRows: 45},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeServer{savedQueries: 45}
			srv := f.start(t)

			out, _, err := runRdshSplit(t, srv, "query", "list", "--limit", tt.limit)
			if err != nil {
				t.Fatalf("query list error = %v", err)
			}
			if _, rows := listRows(t, out); len(rows) != tt.wantRows {
				t.Errorf("rows = %d, want %d", len(rows), tt.wantRows)
			}
		})
	}
}

// TestQueryListTarget covers what the positional argument and --mine change
// about the request: the search term becomes the API's q, and --mine picks
// the endpoint that serves only the caller's own queries.
func TestQueryListTarget(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantPath   string
		wantSearch string
	}{
		{name: "no filter", wantPath: "/api/queries"},
		{name: "search", args: []string{"signups"}, wantPath: "/api/queries", wantSearch: "signups"},
		{name: "mine", args: []string{"--mine"}, wantPath: "/api/queries/my"},
		{
			name:       "mine with a search",
			args:       []string{"signups", "--mine"},
			wantPath:   "/api/queries/my",
			wantSearch: "signups",
		},
		// An unset shell variable expands to an empty argument; listing
		// everything is the same answer the server gives for an empty q, and
		// unlike update's empty --name nothing is destroyed by it.
		{name: "empty search", args: []string{""}, wantPath: "/api/queries"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeServer{savedQueries: 2}
			srv := f.start(t)

			if _, err := runRdsh(t, srv, "", append([]string{"query", "list"}, tt.args...)...); err != nil {
				t.Fatalf("query list error = %v", err)
			}

			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.listed) != 1 {
				t.Fatalf("listing requests = %d, want 1", len(f.listed))
			}
			if got := f.listed[0].Path; got != tt.wantPath {
				t.Errorf("path = %q, want %q", got, tt.wantPath)
			}
			if got := f.listed[0].Query().Get("q"); got != tt.wantSearch {
				t.Errorf("q = %q, want %q", got, tt.wantSearch)
			}
		})
	}
}

// TestQueryListJSON covers the machine-readable format: an array of row
// objects carrying the same four fields, with the ID a number and the draft
// flag a boolean rather than the strings the CSV rendering produces.
func TestQueryListJSON(t *testing.T) {
	f := &fakeServer{savedQueries: 2}
	srv := f.start(t)

	out, _, err := runRdshSplit(t, srv, "query", "list", "--format", "json")
	if err != nil {
		t.Fatalf("query list error = %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	want := map[string]any{
		"id": float64(1), "name": "query 1", "is_draft": true,
		"url": srv.URL + "/queries/1",
	}
	if !reflect.DeepEqual(rows[0], want) {
		t.Errorf("rows[0] = %v, want %v", rows[0], want)
	}
}

// TestQueryListNoMatches covers the empty listing, which is a successful
// answer rather than a failure: the header alone in CSV, an empty array in
// JSON.
func TestQueryListNoMatches(t *testing.T) {
	tests := []struct {
		format string
		want   string
	}{
		{format: "csv", want: wantListHeader + "\n"},
		{format: "json", want: "[]\n"},
	}
	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			f := &fakeServer{}
			srv := f.start(t)

			out, errOut, err := runRdshSplit(t, srv, "query", "list", "nothing matches", "--format", tt.format)
			if err != nil {
				t.Fatalf("query list error = %v", err)
			}
			assertNoNote(t, errOut)
			if out != tt.want {
				t.Errorf("output = %q, want %q", out, tt.want)
			}
		})
	}
}

// TestQueryListTruncationNote covers the note a truncated listing carries.
// It has to be a note rather than a failure — the rows are still what was
// asked for — so the command succeeds, and it goes to stderr so stdout
// stays parseable as the listing alone. That it reaches the process's own
// stderr, and that the run still exits 0, is pinned end to end in
// execute_test.go.
func TestQueryListTruncationNote(t *testing.T) {
	f := &fakeServer{savedQueries: 45}
	srv := f.start(t)

	out, errOut, err := runRdshSplit(t, srv, "query", "list", "--limit", "10")
	if err != nil {
		t.Fatalf("query list error = %v", err)
	}
	if _, rows := listRows(t, out); len(rows) != 10 {
		t.Errorf("stdout rows = %d, want the 10 asked for and no note among them", len(rows))
	}
	// Both counts, so the note says how much was left rather than only that
	// something was.
	for _, want := range []string{"10", "45"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr = %q, want the note to carry %s", errOut, want)
		}
	}
}

// TestQueryListNoTruncationNote is the other half: a listing that showed
// everything must not suggest there is more to see.
func TestQueryListNoTruncationNote(t *testing.T) {
	f := &fakeServer{savedQueries: 3}
	srv := f.start(t)

	out, errOut, err := runRdshSplit(t, srv, "query", "list", "--limit", "3")
	if err != nil {
		t.Fatalf("query list error = %v", err)
	}
	assertNoNote(t, errOut)
	if _, rows := listRows(t, out); len(rows) != 3 {
		t.Errorf("output = %q, want the three rows and nothing else", out)
	}
}

// TestQueryListRejectsBadLimit covers the limits the server would refuse
// as a page_size: they are caught before anything is sent.
func TestQueryListRejectsBadLimit(t *testing.T) {
	for _, limit := range []string{"0", "-1"} {
		t.Run(limit, func(t *testing.T) {
			f := &fakeServer{savedQueries: 3}
			srv := f.start(t)

			_, err := runRdsh(t, srv, "", "query", "list", "--limit", limit)
			if err == nil || !strings.Contains(err.Error(), "--limit") {
				t.Fatalf("error = %v, want one naming --limit", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.listed != nil {
				t.Errorf("listing requests = %v, want the invocation refused before any request", f.listed)
			}
		})
	}
}

// TestQueryListTimeoutIsATimeout pins the exit code contract: a listing is
// a read, so a deadline that cuts it off leaves nothing behind and
// re-running is safe — which is what 124 tells an agent to do.
func TestQueryListTimeoutIsATimeout(t *testing.T) {
	f := &fakeServer{hangListQueries: true}
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "query", "list", "--timeout", "50ms")
	if !errors.Is(err, errTimeout) {
		t.Fatalf("error = %v, want errTimeout", err)
	}
}

// TestQueryShowPrintsTheSQL covers the default output, which is the whole
// point of the command: the stored SQL and nothing else, so that
// `rdsh query show <id> > q.sql` produces a file `rdsh run -f` can take.
// The trailing newline is normalised to exactly one either way, since a
// query stored with one and a query stored without must give the same file.
func TestQueryShowPrintsTheSQL(t *testing.T) {
	const sql = "SELECT id\nFROM users\nWHERE created_at > now() - interval '7 days'"
	tests := []struct {
		name  string
		saved string
	}{
		{name: "stored without a trailing newline", saved: sql},
		{name: "stored with one", saved: sql + "\n"},
		{name: "stored with several", saved: sql + "\n\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeServer{savedQuerySQL: tt.saved}
			srv := f.start(t)

			out, errOut, err := runRdshSplit(t, srv, "query", "show", "7")
			if err != nil {
				t.Fatalf("query show error = %v", err)
			}
			if want := sql + "\n"; out != want {
				t.Errorf("output = %q, want %q and nothing else", out, want)
			}
			if errOut != "" {
				t.Errorf("stderr = %q, want nothing alongside the SQL", errOut)
			}
		})
	}
}

// TestQueryShowTarget covers the forms the argument takes end to end: the
// ID, the URL, and the URL a browser copies from the query's source view.
// The parsing itself is covered by TestResolveQueryID; what this pins is
// that show reaches the same query through all three.
func TestQueryShowTarget(t *testing.T) {
	tests := []struct {
		name   string
		target string // %s stands in for the query's URL on the fake
	}{
		{name: "id", target: "7"},
		{name: "url", target: "%s"},
		{name: "url copied from the source view", target: "%s/source"},
		{name: "url with a query string and fragment", target: "%s?p_x=1#fragment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeServer{}
			srv := f.start(t)

			target := strings.Replace(tt.target, "%s", savedQueryURL(srv), 1)
			out, err := runRdsh(t, srv, "", "query", "show", target)
			if err != nil {
				t.Fatalf("query show error = %v", err)
			}
			if want := defaultSavedQuerySQL + "\n"; out != want {
				t.Errorf("output = %q, want %q", out, want)
			}
		})
	}
}

// TestQueryShowAnotherInstance covers the URL-argument rule from the other
// side: a URL on some other Redash is refused before the API key is sent
// anywhere, and it is an ordinary failure rather than a timeout.
func TestQueryShowAnotherInstance(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "query", "show", "https://redash.other.test/queries/7")
	if err == nil {
		t.Fatal("error = nil, want a URL on another instance refused")
	}
	if errors.Is(err, errTimeout) {
		t.Errorf("error = %v, want an ordinary failure", err)
	}
	assertNothingSent(t, f)
}

// TestQueryShowJSON covers the machine-readable form: one object rather
// than the array of rows the other commands print, carrying the metadata
// the SQL alone cannot. The SQL keeps the > that the encoder escapes,
// which is what the rest of the package's JSON output does too.
//
// It is also the SQL exactly as stored, trailing newline and all: the
// normalisation the default output does exists so that a redirect produces
// the same file either way, and applying it here would hand back something
// other than what Redash holds.
func TestQueryShowJSON(t *testing.T) {
	const sql = "SELECT 1 WHERE 2 > 1\n"
	f := &fakeServer{savedQuerySQL: sql}
	srv := f.start(t)

	out, err := runRdsh(t, srv, "", "query", "show", "7", "--format", "json")
	if err != nil {
		t.Fatalf("query show error = %v", err)
	}
	if strings.Contains(out, ">") {
		t.Errorf("output = %q, want the > escaped as the other JSON output escapes it", out)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	want := map[string]any{
		"id": float64(savedQueryID), "name": "saved", "description": "what it is for",
		"data_source_id": float64(5), "is_draft": true, "url": savedQueryURL(srv), "query": sql,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("output = %v, want %v", got, want)
	}
}

// TestQueryShowRejectsRowFormats covers the one place show's --format
// differs from every other command's: a multi-line SQL body does not fit a
// row format, so json is the only value, and anything else fails while
// cobra parses the flags — before the query is even read.
func TestQueryShowRejectsRowFormats(t *testing.T) {
	for _, f := range []string{"csv", "tsv", "jso"} {
		t.Run(f, func(t *testing.T) {
			srvFake := &fakeServer{}
			srv := srvFake.start(t)

			_, err := runRdsh(t, srv, "", "query", "show", "7", "--format", f)
			if err == nil {
				t.Fatalf("error = nil, want --format %s refused", f)
			}
			if !strings.Contains(err.Error(), "json") {
				t.Errorf("error = %v, want it to name json as the only value", err)
			}
			assertNothingSent(t, srvFake)
		})
	}
}

// TestQueryShowMissingQuery covers a query that is not there: the API's own
// failure surfaces, and it is an ordinary failure rather than a timeout, so
// re-running with a longer --timeout is not suggested.
func TestQueryShowMissingQuery(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "query", "show", "999")
	if err == nil {
		t.Fatal("error = nil, want the API failure reported")
	}
	if errors.Is(err, errTimeout) {
		t.Errorf("error = %v, want an ordinary failure", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %v, want it to carry the server's status", err)
	}
}

// TestQueryShowTimeoutIsATimeout pins the exit code contract: a show is a
// read, so a deadline that cuts it off leaves the query as it was and
// re-running is safe — which is what 124 tells an agent to do.
func TestQueryShowTimeoutIsATimeout(t *testing.T) {
	f := &fakeServer{hangGet: true}
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "query", "show", "7", "--timeout", "50ms")
	if !errors.Is(err, errTimeout) {
		t.Fatalf("error = %v, want errTimeout", err)
	}
}
