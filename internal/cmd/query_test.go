package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeServer emulates just enough of Redash for the command layer: the
// submit response already carries a finished (or never-finishing) job so
// tests do not depend on the client's poll interval.
type fakeServer struct {
	mu            sync.Mutex
	neverDone     bool // job stays PENDING forever; used by timeout tests
	rejectSession bool // GET /api/session returns 401; used by login tests
	cancelled     bool
	submitted     bool
}

func (f *fakeServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/query_results", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.submitted = true
		job := map[string]any{"id": "job-1", "status": 3, "query_result_id": 42}
		if f.neverDone {
			job = map[string]any{"id": "job-1", "status": 1}
		}
		mustJSON(w, map[string]any{"job": job})
	})
	mux.HandleFunc("GET /api/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(w, map[string]any{"job": map[string]any{"id": "job-1", "status": 1}})
	})
	mux.HandleFunc("DELETE /api/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.cancelled = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/query_results/42", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(w, map[string]any{"query_result": map[string]any{"data": map[string]any{
			"columns": []map[string]any{{"name": "n", "friendly_name": "N", "type": "integer"}},
			"rows":    []map[string]any{{"n": 1}},
		}}})
	})
	mux.HandleFunc("GET /api/data_sources", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(w, []map[string]any{{"id": 7, "name": "warehouse"}})
	})
	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		reject := f.rejectSession
		f.mu.Unlock()
		if reject {
			w.WriteHeader(http.StatusUnauthorized)
			mustJSON(w, map[string]any{"message": "Couldn't find resource. Please login and try again."})
			return
		}
		mustJSON(w, map[string]any{"id": 1})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func mustJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}

// runRdsh executes the root command with env-pair credentials pointing at
// the fake server and returns stdout and the error.
func runRdsh(t *testing.T, srv *httptest.Server, stdin string, args ...string) (string, error) {
	t.Helper()
	return runRdshIn(t, t.TempDir(), srv, stdin, args...)
}

// runRdshWithEnvSet executes the root command assuming the caller already
// arranged XDG_CONFIG_HOME and the env pair.
func runRdshWithEnvSet(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

const wantCSV = "n\n1\n"

func TestQueryFromArgument(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	out, err := runRdsh(t, srv, "", "query", "SELECT 1", "--data-source", "5")
	if err != nil {
		t.Fatalf("query error = %v", err)
	}
	if out != wantCSV {
		t.Errorf("output = %q, want %q", out, wantCSV)
	}
}

func TestQueryFromStdin(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	out, err := runRdsh(t, srv, "SELECT 1", "query", "--data-source", "5")
	if err != nil {
		t.Fatalf("query error = %v", err)
	}
	if out != wantCSV {
		t.Errorf("output = %q, want %q", out, wantCSV)
	}
}

func TestQueryFromFile(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	path := filepath.Join(t.TempDir(), "q.sql")
	if err := os.WriteFile(path, []byte("SELECT 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runRdsh(t, srv, "", "query", "-f", path, "--data-source", "5")
	if err != nil {
		t.Fatalf("query error = %v", err)
	}
	if out != wantCSV {
		t.Errorf("output = %q, want %q", out, wantCSV)
	}
}

func TestQueryArgumentAndFileConflict(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	_, err := runRdsh(t, srv, "", "query", "SELECT 1", "-f", "q.sql", "--data-source", "5")
	if err == nil || !strings.Contains(err.Error(), "-f") {
		t.Errorf("error = %v, want conflict error mentioning -f", err)
	}
}

func TestQueryEmptyStdin(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	_, err := runRdsh(t, srv, "   \n", "query", "--data-source", "5")
	if err == nil || !strings.Contains(err.Error(), "no SQL") {
		t.Errorf("error = %v, want no-SQL error", err)
	}
}

func TestQueryJSONFormat(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	out, err := runRdsh(t, srv, "", "query", "SELECT 1", "--data-source", "5", "--format", "json")
	if err != nil {
		t.Fatalf("query error = %v", err)
	}
	if want := "[{\"n\":1}]\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestQueryDataSourceByName(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	out, err := runRdsh(t, srv, "", "query", "SELECT 1", "--data-source", "warehouse")
	if err != nil {
		t.Fatalf("query error = %v", err)
	}
	if out != wantCSV {
		t.Errorf("output = %q, want %q", out, wantCSV)
	}
}

func TestQueryDataSourceNameNotFound(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	_, err := runRdsh(t, srv, "", "query", "SELECT 1", "--data-source", "nope")
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %v, want not-found error naming the data source", err)
	}
}

func TestQueryNoDataSource(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	_, err := runRdsh(t, srv, "", "query", "SELECT 1")
	if err == nil || !strings.Contains(err.Error(), "--data-source") {
		t.Errorf("error = %v, want error telling the user to pass --data-source", err)
	}
}

func TestQueryTimeoutCancelsJob(t *testing.T) {
	f := &fakeServer{neverDone: true}
	srv := f.start(t)
	_, err := runRdsh(t, srv, "", "query", "SELECT pg_sleep(600)", "--data-source", "5", "--timeout", "50ms")
	if !errors.Is(err, errTimeout) {
		t.Fatalf("error = %v, want errTimeout", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.cancelled {
		t.Error("job was not cancelled on the server")
	}
}

func TestQueryInvalidFormatFailsBeforeSubmit(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)
	_, err := runRdsh(t, srv, "", "query", "SELECT 1", "--data-source", "5", "--format", "jso")
	if err == nil || !strings.Contains(err.Error(), "jso") {
		t.Fatalf("error = %v, want unsupported-format error", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitted {
		t.Error("query was submitted to the server despite the invalid --format")
	}
}

func TestQueryNegativeTimeout(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)
	_, err := runRdsh(t, srv, "", "query", "SELECT 1", "--data-source", "5", "--timeout", "-5s")
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("error = %v, want negative-timeout error", err)
	}
}

// TestQueryProfileFlagWiring exercises the root persistent flag through a
// subcommand: --profile must win over the env pair and fail on unknown names.
func TestQueryProfileFlagWiring(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)
	_, err := runRdsh(t, srv, "", "query", "SELECT 1", "--profile", "nope", "--data-source", "5")
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %v, want unknown-profile error even though the env pair is set", err)
	}
}

func TestExitCodeMapping(t *testing.T) {
	if got := exitCode(nil); got != 0 {
		t.Errorf("exitCode(nil) = %d, want 0", got)
	}
	if got := exitCode(errTimeout); got != 124 {
		t.Errorf("exitCode(errTimeout) = %d, want 124", got)
	}
	if got := exitCode(errors.New("boom")); got != 1 {
		t.Errorf("exitCode(other) = %d, want 1", got)
	}
}
