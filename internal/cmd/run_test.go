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
	pollRequest         = "poll"
	cancelRequest       = "cancel"
	dataSourcesRequest  = "data sources"
	updateRequest       = "update"
	getQueryRequest     = "get query"
	listQueriesRequest  = "list queries"
	createVizRequest    = "create visualization"
	storedResultRequest = "stored result"
)

// savedQueryID is the ID the fake gives every query it is asked to save,
// and savedQueryVersion the version it serves for it.
const (
	savedQueryID      = 7
	savedQueryVersion = 3
	// savedVizID is the ID the fake gives the visualization it holds, and
	// the one it accepts an update or a delete for.
	savedVizID = 9
	// storedResultID is the result the saved query links to when the fake is
	// set up with stored columns.
	storedResultID = 77
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
	mu               sync.Mutex
	neverDone        bool // job stays PENDING forever; used by timeout tests
	rejectSession    bool // GET /api/session returns 401; used by login tests
	hangSession      bool // GET /api/session never answers
	hangCancel       bool // DELETE /api/jobs/... never answers
	hangList         bool // GET /api/data_sources never answers
	hangCreate       bool // POST /api/queries never answers
	rejectUpdate     bool // POST /api/queries/<id> returns 400
	hangUpdate       bool // POST /api/queries/<id> never answers
	hangGet          bool // GET /api/queries/<id> never answers
	conflict         bool // POST /api/queries/<id> returns 409
	hangListQueries  bool // GET /api/queries never answers
	hangStoredResult bool // GET /api/query_results/<stored> never answers
	hangCreateViz    bool // POST /api/visualizations never answers
	hangUpdateViz    bool // POST /api/visualizations/<id> never answers
	hangDeleteViz    bool // DELETE /api/visualizations/<id> never answers
	savedQueries     int  // queries the listing endpoints hold
	// savedQuerySQL is the SQL GET /api/queries/<id> serves; empty means
	// defaultSavedQuerySQL. A test that reads the SQL back sets it, which is
	// what makes the show command's newline handling observable.
	savedQuerySQL string
	// savedQueryParameters is what GET /api/queries/<id> serves as
	// options.parameters; nil serves a query carrying no options at all,
	// which is what `rdsh query create` saves.
	savedQueryParameters []map[string]any
	// savedQueryOptions serves the whole options object instead, for the
	// tests that need the keys beside the parameters. It wins over
	// savedQueryParameters when both are set.
	savedQueryOptions map[string]any
	// savedQueryVisualizations is what GET /api/queries/<id> serves as its
	// visualizations array; nil leaves the key out, which is what a query
	// with no charts on it reads back as.
	savedQueryVisualizations []map[string]any
	// storedColumns makes the query report a linked result carrying these
	// column names, served from GET /api/query_results/<id>. Nil leaves
	// latest_query_data_id at zero, which is how a query nobody has
	// executed reads back.
	storedColumns []string
	// createLinksResult makes POST /api/queries answer with a result
	// already linked, which is what a Redash that backfills the link as the
	// query is saved returns. Left false the create answers without the
	// key, as every older version does.
	createLinksResult bool
	// missingParameter makes the execution endpoint refuse the way Redash
	// refuses a placeholder it was given no value for: a failed job rather
	// than a message, and an HTTP 400.
	missingParameter bool
	cancelled        bool
	submitted        bool
	fetched          bool           // GET /api/queries/<id> arrived
	fetchCount       int            // how many times it arrived
	listedSources    bool           // GET /api/data_sources arrived
	created          map[string]any // body of POST /api/queries
	// storedResultReads counts GET /api/query_results/<stored>, so a test
	// can assert that a command ran no column check at all.
	storedResultReads int
	// refreshed is the body of POST /api/queries/<id>/results, the one
	// endpoint that executes a saved query. A command that must not execute
	// anything is checked by this staying nil.
	refreshed map[string]any
	// createdViz is the body of POST /api/visualizations, updatedViz that
	// of POST /api/visualizations/<id>, and deletedViz records that the
	// DELETE arrived.
	createdViz map[string]any
	updatedViz map[string]any
	deletedViz bool
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
	mux.HandleFunc("POST /api/query_results", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.submitted = true
		job := map[string]any{"id": "job-1", "status": 3, "query_result_id": 42}
		if f.neverDone {
			job = map[string]any{"id": "job-1", "status": 1}
		}
		mustJSON(w, map[string]any{"job": job})
	})
	mux.HandleFunc("GET /api/jobs/job-1", func(w http.ResponseWriter, _ *http.Request) {
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
	mux.HandleFunc("GET /api/query_results/42", func(w http.ResponseWriter, _ *http.Request) {
		mustJSON(w, map[string]any{"query_result": map[string]any{"data": map[string]any{
			"columns": []map[string]any{{"name": "n", "friendly_name": "N", "type": "integer"}},
			"rows":    []map[string]any{{"n": 1}},
		}}})
	})
	mux.HandleFunc("GET /api/data_sources", func(w http.ResponseWriter, r *http.Request) {
		f.reach(dataSourcesRequest)
		f.mu.Lock()
		f.listedSources = true
		f.mu.Unlock()
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
		created := map[string]any{"id": savedQueryID, "is_draft": true}
		if f.createLinksResult {
			created["latest_query_data_id"] = storedResultID
		}
		mustJSON(w, created)
	})
	mux.HandleFunc(fmt.Sprintf("GET /api/queries/%d", savedQueryID), func(w http.ResponseWriter, r *http.Request) {
		f.reach(getQueryRequest)
		f.mu.Lock()
		f.fetched = true
		f.fetchCount++
		f.mu.Unlock()
		if f.hangGet {
			f.park(r)
			return
		}
		sql := f.savedQuerySQL
		if sql == "" {
			sql = defaultSavedQuerySQL
		}
		query := map[string]any{
			"id": savedQueryID, "name": "saved", "description": "what it is for", "query": sql,
			"data_source_id": 5, "is_draft": true, "version": savedQueryVersion,
		}
		if f.storedColumns != nil {
			query["latest_query_data_id"] = storedResultID
		}
		switch {
		case f.savedQueryOptions != nil:
			query["options"] = f.savedQueryOptions
		case f.savedQueryParameters != nil:
			query["options"] = map[string]any{"parameters": f.savedQueryParameters}
		}
		if f.savedQueryVisualizations != nil {
			query["visualizations"] = f.savedQueryVisualizations
		}
		mustJSON(w, query)
	})
	mux.HandleFunc(fmt.Sprintf("POST /api/queries/%d/results", savedQueryID),
		func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			if err := json.NewDecoder(r.Body).Decode(&f.refreshed); err != nil {
				t.Errorf("decode executed query: %v", err)
			}
			f.mu.Unlock()
			if f.missingParameter {
				w.WriteHeader(http.StatusBadRequest)
				mustJSON(w, map[string]any{"job": map[string]any{
					"status": 4, "error": "Missing parameter value for: since",
				}})
				return
			}
			job := map[string]any{"id": "job-1", "status": 3, "query_result_id": 42}
			if f.neverDone {
				job = map[string]any{"id": "job-1", "status": 1}
			}
			mustJSON(w, map[string]any{"job": job})
		})
	mux.HandleFunc(fmt.Sprintf("GET /api/query_results/%d", storedResultID),
		func(w http.ResponseWriter, r *http.Request) {
			f.reach(storedResultRequest)
			f.mu.Lock()
			f.storedResultReads++
			f.mu.Unlock()
			if f.hangStoredResult {
				f.park(r)
				return
			}
			columns := make([]map[string]any, len(f.storedColumns))
			for i, name := range f.storedColumns {
				columns[i] = map[string]any{"name": name, "friendly_name": name, "type": "string"}
			}
			mustJSON(w, map[string]any{"query_result": map[string]any{"data": map[string]any{
				"columns": columns, "rows": []map[string]any{},
			}}})
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
	mux.HandleFunc("POST /api/visualizations", func(w http.ResponseWriter, r *http.Request) {
		f.reach(createVizRequest)
		f.mu.Lock()
		if err := json.NewDecoder(r.Body).Decode(&f.createdViz); err != nil {
			t.Errorf("decode created visualization: %v", err)
		}
		f.mu.Unlock()
		if f.hangCreateViz {
			f.park(r)
			return
		}
		mustJSON(w, map[string]any{"id": savedVizID, "type": f.createdViz["type"], "name": f.createdViz["name"]})
	})
	mux.HandleFunc(fmt.Sprintf("POST /api/visualizations/%d", savedVizID),
		func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			if err := json.NewDecoder(r.Body).Decode(&f.updatedViz); err != nil {
				t.Errorf("decode updated visualization: %v", err)
			}
			f.mu.Unlock()
			if f.hangUpdateViz {
				f.park(r)
				return
			}
			mustJSON(w, map[string]any{"id": savedVizID, "type": "CHART", "name": "daily"})
		})
	mux.HandleFunc(fmt.Sprintf("DELETE /api/visualizations/%d", savedVizID),
		func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			f.deletedViz = true
			f.mu.Unlock()
			if f.hangDeleteViz {
				f.park(r)
				return
			}
			w.WriteHeader(http.StatusOK)
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
		if f.hangSession {
			f.park(r)
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
	return runRdshWithStdin(context.Background(), t, strings.NewReader(stdin), args...)
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
func runRdshWithStdin(ctx context.Context, t *testing.T, stdin io.Reader, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runRdshInto(ctx, t, stdin, &out, &out, args...)
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
	err := runRdshInto(context.Background(), t, strings.NewReader(""), &out, &errOut, args...)
	return out.String(), errOut.String(), err
}

// runRdshInto executes the root command with the given streams. The command
// runs in a goroutine because an interrupted prompt is supposed to return on
// its own; called directly, a run that did not would hang the package until
// the test binary panics. Nothing may call t.Fatal from that goroutine — it
// only ends the goroutine it runs on.
func runRdshInto(ctx context.Context, t *testing.T, stdin io.Reader, out, errOut io.Writer, args ...string) error {
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

// TestRunDataSourceLookupTimeout covers the deadline expiring while a
// --data-source name is resolved to an ID. The lookup is a server call like
// any other, so it has to end the run as a timeout rather than report a bare
// context error as an ordinary failure — 1 tells an agent that a longer
// --timeout will not help, which is the one recovery that would.
func TestRunDataSourceLookupTimeout(t *testing.T) {
	f := &fakeServer{hangList: true}
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "run", "SELECT 1", "--data-source", "warehouse", "--timeout", "50ms")
	if !errors.Is(err, errTimeout) {
		t.Fatalf("error = %v, want errTimeout", err)
	}
	if got := exitCode(err); got != timeoutExitCode {
		t.Errorf("exitCode(%v) = %d, want %d", err, got, timeoutExitCode)
	}
	// Not "data source", which data-source list's own message carries as
	// well: the wording has to tell the lookup apart from the listing.
	if !strings.Contains(err.Error(), "lookup") {
		t.Errorf("error = %v, want it to name the lookup that timed out", err)
	}
}

// TestRunDataSourceByIDSkipsLookup pins what the lookup's new timeout must
// not change: an all-digit --data-source still resolves without asking the
// server, so the only expiry it can reach is the query's.
func TestRunDataSourceByIDSkipsLookup(t *testing.T) {
	f := &fakeServer{neverDone: true}
	srv := f.start(t)

	_, err := runRdsh(t, srv, "", "run", "SELECT 1", "--data-source", "5", "--timeout", "50ms")
	if !errors.Is(err, errTimeout) {
		t.Fatalf("error = %v, want errTimeout", err)
	}
	if !strings.Contains(err.Error(), "query timed out") {
		t.Errorf("error = %v, want the query's timeout rather than the lookup's", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listedSources {
		t.Error("the data sources were listed for an all-digit --data-source")
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

// TestTimeoutFlagContract walks the tree so that every command talking to
// Redash is held to one --timeout: the same name, type and 90 s default,
// applied without a caller passing anything. The profile commands only read
// and write the config file, so a flag there would silently do nothing.
func TestTimeoutFlagContract(t *testing.T) {
	bounded := [][]string{
		{"run"},
		{"query", "create"},
		{"query", "update"},
		{"query", "list"},
		{"query", "show"},
		{"query", "refresh"},
		{"data-source", "list"},
		{"auth", "login"},
	}
	unbounded := [][]string{{"profile", "list"}, {"profile", "use"}}

	root, _ := newRootCmd()
	for _, path := range bounded {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Errorf("Find(%v) error = %v", path, err)
			continue
		}
		flag := cmd.Flags().Lookup("timeout")
		if flag == nil {
			t.Errorf("%s has no --timeout", cmd.CommandPath())
			continue
		}
		if want := defaultTimeout.String(); flag.DefValue != want {
			t.Errorf("%s --timeout default = %s, want %s", cmd.CommandPath(), flag.DefValue, want)
		}
		// The type rather than Type(), which DurationVar also reports as
		// "duration": registered that way the flag would look identical
		// here and take a negative duration, which withTimeout reads as no
		// limit at all.
		if _, ok := flag.Value.(*timeoutFlag); !ok {
			t.Errorf("%s --timeout is a %T, want a *timeoutFlag", cmd.CommandPath(), flag.Value)
		}
	}
	for _, path := range unbounded {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Errorf("Find(%v) error = %v", path, err)
			continue
		}
		if cmd.Flags().Lookup("timeout") != nil {
			t.Errorf("%s has a --timeout, which would do nothing", cmd.CommandPath())
		}
	}
}
