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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
	updateRequest      = "update"
	getQueryRequest    = "get query"
	listQueriesRequest = "list queries"
)

// savedQueryID is the ID the fake gives every query it is asked to save,
// and savedQueryVersion the version it serves for it.
const (
	savedQueryID      = 7
	savedQueryVersion = 3
)

// defaultSavedQuerySQL is the SQL the fake serves for that query unless a
// test asks for its own.
const defaultSavedQuerySQL = "SELECT 1"

// fakeServer emulates just enough of Redash for the command layer: the
// submit response already carries a finished (or never-finishing) job so
// tests do not depend on the client's poll interval. The hang* fields make
// an endpoint never answer, standing in for a wedged Redash; they are set
// before start and never written afterwards.
type fakeServer struct {
	mu              sync.Mutex
	neverDone       bool // job stays PENDING forever; used by timeout tests
	rejectSession   bool // GET /api/session returns 401; used by login tests
	hangCancel      bool // DELETE /api/jobs/... never answers
	hangList        bool // GET /api/data_sources never answers
	hangCreate      bool // POST /api/queries never answers
	rejectUpdate    bool // POST /api/queries/<id> returns 400
	hangUpdate      bool // POST /api/queries/<id> never answers
	hangGet         bool // GET /api/queries/<id> never answers
	conflict        bool // POST /api/queries/<id> returns 409
	hangListQueries bool // GET /api/queries never answers
	savedQueries    int  // queries the listing endpoints hold
	// savedQuerySQL is the SQL GET /api/queries/<id> serves; empty means
	// defaultSavedQuerySQL. A test that reads the SQL back sets it, which is
	// what makes the show command's newline handling observable.
	savedQuerySQL string
	cancelled     bool
	submitted     bool
	fetched       bool           // GET /api/queries/<id> arrived
	created       map[string]any // body of POST /api/queries
	// listed is the query string of every listing request that arrived, in
	// order, so a test can check both the endpoint and how it was called.
	listed []*url.URL
	// updated is the body of POST /api/queries/<id>, which serves both
	// create's publishing step and every query update.
	updated map[string]any

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
	mux.HandleFunc(fmt.Sprintf("GET /api/queries/%d", savedQueryID), func(w http.ResponseWriter, r *http.Request) {
		f.reach(getQueryRequest)
		f.mu.Lock()
		f.fetched = true
		f.mu.Unlock()
		if f.hangGet {
			f.park(r)
			return
		}
		sql := f.savedQuerySQL
		if sql == "" {
			sql = defaultSavedQuerySQL
		}
		mustJSON(w, map[string]any{
			"id": savedQueryID, "name": "saved", "description": "what it is for", "query": sql,
			"data_source_id": 5, "is_draft": true, "version": savedQueryVersion,
		})
	})
	mux.HandleFunc(fmt.Sprintf("POST /api/queries/%d", savedQueryID), func(w http.ResponseWriter, r *http.Request) {
		f.reach(updateRequest)
		f.mu.Lock()
		if err := json.NewDecoder(r.Body).Decode(&f.updated); err != nil {
			f.mu.Unlock()
			return
		}
		f.mu.Unlock()
		switch {
		case f.hangUpdate:
			f.park(r)
		case f.rejectUpdate:
			w.WriteHeader(http.StatusBadRequest)
			mustJSON(w, map[string]any{"message": "publishing is not allowed"})
		case f.conflict:
			w.WriteHeader(http.StatusConflict)
			mustJSON(w, map[string]any{"message": "Query version conflict"})
		default:
			mustJSON(w, map[string]any{"id": savedQueryID, "is_draft": false})
		}
	})
	mux.HandleFunc("GET /api/queries", f.handleQueryList)
	mux.HandleFunc("GET /api/queries/my", f.handleQueryList)
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

// handleQueryList serves both listing endpoints with savedQueries rows,
// paginated the way the real ones are. The redash package's own fake is
// where the walk's edge cases are pinned; this one only has to be faithful
// enough for the command layer's columns, flags and limit.
func (f *fakeServer) handleQueryList(w http.ResponseWriter, r *http.Request) {
	f.reach(listQueriesRequest)
	f.mu.Lock()
	f.listed = append(f.listed, r.URL)
	f.mu.Unlock()
	if f.hangListQueries {
		f.park(r)
		return
	}

	values := r.URL.Query()
	page, _ := strconv.Atoi(values.Get("page"))
	pageSize, _ := strconv.Atoi(values.Get("page_size"))
	results := []map[string]any{}
	for i := (page - 1) * pageSize; i < f.savedQueries && len(results) < pageSize; i++ {
		results = append(results, map[string]any{
			"id": i + 1, "name": fmt.Sprintf("query %d", i+1), "is_draft": i%2 == 0,
		})
	}
	mustJSON(w, map[string]any{
		"count": f.savedQueries, "page": page, "page_size": pageSize, "results": results,
	})
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
// one produced the text has to assert the whole of it, or use runRdshSplit.
func runRdshWithStdin(t *testing.T, ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runRdshInto(t, ctx, stdin, &out, &out, args...)
	return out.String(), err
}

// runRdshSplit is runRdsh with the two output streams kept apart. The
// listing is the one command that writes to both — rows to stdout, the
// truncation note to stderr — and a shared buffer would count the note as
// an extra row.
func runRdshSplit(t *testing.T, srv *httptest.Server, args ...string) (string, string, error) {
	t.Helper()
	setRdshEnv(t, t.TempDir(), srv)

	var out, errOut bytes.Buffer
	err := runRdshInto(t, context.Background(), strings.NewReader(""), &out, &errOut, args...)
	return out.String(), errOut.String(), err
}

// runRdshInto executes the root command with the given streams. The command
// runs in a goroutine because an interrupted prompt is supposed to return on
// its own; called directly, a run that did not would hang the package until
// the test binary panics. Nothing may call t.Fatal from that goroutine — it
// only ends the goroutine it runs on.
func runRdshInto(t *testing.T, ctx context.Context, stdin io.Reader, out, errOut io.Writer, args ...string) error {
	t.Helper()
	root, report := newRootCmd()
	root.SetOut(out)
	root.SetErr(errOut)
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
		// The same merge Execute does, so a stray argument to a group
		// command reaches a caller here as the error it is rather than as
		// the nil cobra returns for it.
		if err == nil {
			err = report.err
		}
		return err
	case <-timer.C:
		t.Fatal("the command did not return")
		return nil // unreachable: t.Fatal ends this goroutine
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

// TestTimeoutFlagRejectsNegative covers the guard every command used to
// carry itself, now that it runs while the flags are parsed. The message
// carries no flag name because pflag prefixes one.
func TestTimeoutFlagRejectsNegative(t *testing.T) {
	var f timeoutFlag
	if err := f.Set("-5s"); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("Set(-5s) error = %v, want it to refuse a negative duration", err)
	}
	if err := f.Set("3s"); err != nil {
		t.Fatalf("Set(3s) error = %v", err)
	}
	if got := f.Duration(); got != 3*time.Second {
		t.Errorf("Duration() = %s, want 3s", got)
	}
}

// TestWithTimeout pins what --timeout 0 means: no deadline at all, rather
// than one that has already expired.
func TestWithTimeout(t *testing.T) {
	ctx, cancel := withTimeout(context.Background(), 0)
	defer cancel()
	if deadline, ok := ctx.Deadline(); ok {
		t.Errorf("a zero timeout set a deadline of %s, want none", deadline)
	}

	bounded, cancelBounded := withTimeout(context.Background(), time.Minute)
	defer cancelBounded()
	if _, ok := bounded.Deadline(); !ok {
		t.Error("a positive timeout set no deadline")
	}
}
