package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Requests an interrupt test waits for before signalling, so the signal
// lands with the command in the place the test is about.
const (
	pollRequest        = "poll"
	cancelRequest      = "cancel"
	dataSourcesRequest = "data sources"
	publishRequest     = "publish"
)

// savedQueryID is the ID the fake gives every query it is asked to save.
const savedQueryID = 7

// fakeServer emulates just enough of Redash for the command layer: the
// submit response already carries a finished (or never-finishing) job so
// tests do not depend on the client's poll interval. The hang* fields make
// an endpoint never answer, standing in for a wedged Redash; they are set
// before start and never written afterwards.
type fakeServer struct {
	mu            sync.Mutex
	neverDone     bool // job stays PENDING forever; used by timeout tests
	rejectSession bool // GET /api/session returns 401; used by login tests
	hangCancel    bool // DELETE /api/jobs/... never answers
	hangList      bool // GET /api/data_sources never answers
	hangCreate    bool // POST /api/queries never answers
	rejectPublish bool // POST /api/queries/<id> returns 400
	hangPublish   bool // POST /api/queries/<id> never answers
	cancelled     bool
	submitted     bool
	created       map[string]any // body of POST /api/queries
	published     map[string]any // body of POST /api/queries/<id>

	reached chan string   // requests that arrived, for waitFor
	release chan struct{} // closed on cleanup, freeing parked handlers
}

func (f *fakeServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	f.reached = make(chan string, 8)
	f.release = make(chan struct{})
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
		f.reach(pollRequest)
		mustJSON(w, map[string]any{"job": map[string]any{"id": "job-1", "status": 1}})
	})
	mux.HandleFunc("DELETE /api/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		f.reach(cancelRequest)
		f.mu.Lock()
		f.cancelled = true
		f.mu.Unlock()
		if f.hangCancel {
			f.park(r)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/query_results/42", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(w, map[string]any{"query_result": map[string]any{"data": map[string]any{
			"columns": []map[string]any{{"name": "n", "friendly_name": "N", "type": "integer"}},
			"rows":    []map[string]any{{"n": 1}},
		}}})
	})
	mux.HandleFunc("GET /api/data_sources", func(w http.ResponseWriter, r *http.Request) {
		f.reach(dataSourcesRequest)
		if f.hangList {
			f.park(r)
			return
		}
		mustJSON(w, []map[string]any{{"id": 7, "name": "warehouse"}})
	})
	mux.HandleFunc("POST /api/queries", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if err := json.NewDecoder(r.Body).Decode(&f.created); err != nil {
			t.Errorf("decode created query: %v", err)
		}
		f.mu.Unlock()
		if f.hangCreate {
			f.park(r)
			return
		}
		mustJSON(w, map[string]any{"id": savedQueryID, "is_draft": true})
	})
	mux.HandleFunc(fmt.Sprintf("POST /api/queries/%d", savedQueryID), func(w http.ResponseWriter, r *http.Request) {
		f.reach(publishRequest)
		f.mu.Lock()
		if err := json.NewDecoder(r.Body).Decode(&f.published); err != nil {
			f.mu.Unlock()
			return
		}
		f.mu.Unlock()
		switch {
		case f.hangPublish:
			f.park(r)
		case f.rejectPublish:
			w.WriteHeader(http.StatusBadRequest)
			mustJSON(w, map[string]any{"message": "publishing is not allowed"})
		default:
			mustJSON(w, map[string]any{"id": savedQueryID, "is_draft": false})
		}
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
	// Registered after srv.Close so it runs before it: cleanups are LIFO,
	// and Close waits for the very handlers this releases.
	t.Cleanup(func() { close(f.release) })
	return srv
}

// park holds a request until the test's cleanup releases it or the client
// gives up, standing in for an endpoint that never answers.
func (f *fakeServer) park(r *http.Request) {
	select {
	case <-f.release:
	case <-r.Context().Done():
	}
}

// reach announces that a request arrived. The send never blocks: a test
// waits for the one request it cares about and ignores the rest.
func (f *fakeServer) reach(name string) {
	select {
	case f.reached <- name:
	default:
	}
}

// waitFor blocks until the named request has reached the server, so a test
// signals a command that has actually got there.
func (f *fakeServer) waitFor(t *testing.T, name string) {
	t.Helper()
	timer := time.NewTimer(commandReturnTimeout)
	defer timer.Stop()
	for {
		select {
		case got := <-f.reached:
			if got == name {
				return
			}
		case <-timer.C:
			t.Fatalf("the %s request never reached the server", name)
		}
	}
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
	return runRdshWithStdin(t, context.Background(), strings.NewReader(stdin), args...)
}

// commandReturnTimeout bounds how long a test waits for a command to
// return. Nothing here is supposed to block indefinitely, so this only ever
// fires on a regression; it is generous so a loaded CI machine does not
// trip it.
const commandReturnTimeout = 10 * time.Second

// runRdshWithStdin is runRdshWithEnvSet with an explicit context and an
// io.Reader stdin, so a test can cancel a command mid-prompt or hand it a
// real *os.File. Both streams share one buffer, so a test that cares which
// one produced the text has to assert the whole of it.
//
// The command runs in a goroutine because an interrupted prompt is supposed
// to return on its own; called directly, a run that did not would hang the
// package until the test binary panics. Nothing may call t.Fatal from that
// goroutine — it only ends the goroutine it runs on.
func runRdshWithStdin(t *testing.T, ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(stdin)
	root.SetArgs(args)

	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	// A timer rather than time.After: nearly every test in the package runs
	// through here, and time.After would leave each one's timer parked in
	// the runtime for the full ten seconds after the command had returned.
	timer := time.NewTimer(commandReturnTimeout)
	defer timer.Stop()

	select {
	case err := <-done:
		return out.String(), err
	case <-timer.C:
		t.Fatal("the command did not return")
		return "", nil // unreachable: t.Fatal ends this goroutine
	}
}

const wantCSV = "n\n1\n"

func TestRunFromArgument(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	out, err := runRdsh(t, srv, "", "run", "SELECT 1", "--data-source", "5")
	if err != nil {
		t.Fatalf("query error = %v", err)
	}
	if out != wantCSV {
		t.Errorf("output = %q, want %q", out, wantCSV)
	}
}

func TestRunFromStdin(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	out, err := runRdsh(t, srv, "SELECT 1", "run", "--data-source", "5")
	if err != nil {
		t.Fatalf("query error = %v", err)
	}
	if out != wantCSV {
		t.Errorf("output = %q, want %q", out, wantCSV)
	}
}

func TestRunFromFile(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	path := filepath.Join(t.TempDir(), "q.sql")
	if err := os.WriteFile(path, []byte("SELECT 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runRdsh(t, srv, "", "run", "-f", path, "--data-source", "5")
	if err != nil {
		t.Fatalf("query error = %v", err)
	}
	if out != wantCSV {
		t.Errorf("output = %q, want %q", out, wantCSV)
	}
}

func TestRunArgumentAndFileConflict(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	_, err := runRdsh(t, srv, "", "run", "SELECT 1", "-f", "q.sql", "--data-source", "5")
	if err == nil || !strings.Contains(err.Error(), "-f") {
		t.Errorf("error = %v, want conflict error mentioning -f", err)
	}
}

func TestRunEmptyStdin(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	_, err := runRdsh(t, srv, "   \n", "run", "--data-source", "5")
	if err == nil || !strings.Contains(err.Error(), "no SQL") {
		t.Errorf("error = %v, want no-SQL error", err)
	}
}

func TestRunJSONFormat(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	out, err := runRdsh(t, srv, "", "run", "SELECT 1", "--data-source", "5", "--format", "json")
	if err != nil {
		t.Fatalf("query error = %v", err)
	}
	if want := "[{\"n\":1}]\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestRunDataSourceByName(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	out, err := runRdsh(t, srv, "", "run", "SELECT 1", "--data-source", "warehouse")
	if err != nil {
		t.Fatalf("query error = %v", err)
	}
	if out != wantCSV {
		t.Errorf("output = %q, want %q", out, wantCSV)
	}
}

func TestRunDataSourceNameNotFound(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	_, err := runRdsh(t, srv, "", "run", "SELECT 1", "--data-source", "nope")
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %v, want not-found error naming the data source", err)
	}
}

func TestRunNoDataSource(t *testing.T) {
	srv := (&fakeServer{}).start(t)
	_, err := runRdsh(t, srv, "", "run", "SELECT 1")
	if err == nil || !strings.Contains(err.Error(), "--data-source") {
		t.Errorf("error = %v, want error telling the user to pass --data-source", err)
	}
}

func TestRunTimeoutCancelsJob(t *testing.T) {
	f := &fakeServer{neverDone: true}
	srv := f.start(t)
	_, err := runRdsh(t, srv, "", "run", "SELECT pg_sleep(600)", "--data-source", "5", "--timeout", "50ms")
	if !errors.Is(err, errTimeout) {
		t.Fatalf("error = %v, want errTimeout", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.cancelled {
		t.Error("job was not cancelled on the server")
	}
}

func TestRunInvalidFormatFailsBeforeSubmit(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)
	_, err := runRdsh(t, srv, "", "run", "SELECT 1", "--data-source", "5", "--format", "jso")
	if err == nil || !strings.Contains(err.Error(), "jso") {
		t.Fatalf("error = %v, want unsupported-format error", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitted {
		t.Error("query was submitted to the server despite the invalid --format")
	}
}

func TestRunNegativeTimeout(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)
	_, err := runRdsh(t, srv, "", "run", "SELECT 1", "--data-source", "5", "--timeout", "-5s")
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("error = %v, want negative-timeout error", err)
	}
}

// TestRunProfileFlagWiring exercises the root persistent flag through a
// subcommand: --profile must win over the env pair and fail on unknown names.
func TestRunProfileFlagWiring(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)
	_, err := runRdsh(t, srv, "", "run", "SELECT 1", "--profile", "nope", "--data-source", "5")
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
