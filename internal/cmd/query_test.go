package cmd

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// TestResolveQueryID covers the argument both update and a future show take:
// an ID, or the URL a browser copied from the address bar. The prefix has to
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
			got, err := resolveQueryID(tt.arg, b)
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
