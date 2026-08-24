package redash_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/178inaba/rdsh/internal/redash"
)

// fakeRedash simulates the Redash job lifecycle. Response shapes follow the
// real API: POST /api/query_results returns {"job": {...}}, job status is
// 1 PENDING / 2 STARTED / 3 SUCCESS / 4 FAILURE / 5 CANCELLED, and results
// come wrapped in {"query_result": {"data": {"columns": [...], "rows":
// [...]}}} with columns as objects, not strings.
type fakeRedash struct {
	t *testing.T

	mu          sync.Mutex
	jobStatuses []int  // consecutive GET /api/jobs responses
	jobError    string // error field served with status 4
	pollCount   int
	// submitRefusal is the message POST /api/query_results refuses the
	// submission with, as a failed job rather than as a message field.
	submitRefusal string
	cancelled     bool
	cancelStatus  int // HTTP status for DELETE, default 200
	submittedBody map[string]any
	refreshBody   map[string]any // body of POST /api/queries/7/results
	queryOptions  map[string]any // options served by GET /api/queries/7
	createdQuery  map[string]any // body of POST /api/queries
	updatedQuery  map[string]any // body of POST /api/queries/7
	queryVersion  int            // version served by GET /api/queries/7
	updateStatus  int            // HTTP status for POST /api/queries/7, default 200
	savedQueries  int            // queries the listing endpoints hold
	listRequests  []listRequest  // what reached the listing endpoints, in order
	gotAuth       []string
}

// listRequest is one request to a listing endpoint, kept so a test can
// check both which endpoint answered and how the walk was parameterised.
type listRequest struct {
	path   string
	values url.Values
}

func (f *fakeRedash) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/query_results", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.gotAuth = append(f.gotAuth, r.Header.Get("Authorization"))
		if err := json.NewDecoder(r.Body).Decode(&f.submittedBody); err != nil {
			f.t.Errorf("decode submitted body: %v", err)
		}
		if f.submitRefusal != "" {
			// The shape run_query refuses with, which is not the
			// {"message": ...} the rest of the API uses.
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"job": map[string]any{"status": 4, "error": f.submitRefusal}})
			return
		}
		writeJSON(w, map[string]any{"job": map[string]any{"id": "job-1", "status": 1}})
	})
	mux.HandleFunc("GET /api/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.gotAuth = append(f.gotAuth, r.Header.Get("Authorization"))
		i := f.pollCount
		if i >= len(f.jobStatuses) {
			i = len(f.jobStatuses) - 1
		}
		f.pollCount++
		job := map[string]any{"id": "job-1", "status": f.jobStatuses[i]}
		switch f.jobStatuses[i] {
		case 3:
			job["query_result_id"] = 42
		case 4:
			job["error"] = f.jobError
		}
		writeJSON(w, map[string]any{"job": job})
	})
	mux.HandleFunc("DELETE /api/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.cancelled = true
		status := f.cancelStatus
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	})
	mux.HandleFunc("GET /api/query_results/42", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"query_result": map[string]any{"data": map[string]any{
			"columns": []map[string]any{
				{"name": "id", "friendly_name": "ID", "type": "integer"},
				{"name": "name", "friendly_name": nil, "type": nil},
			},
			"rows": []map[string]any{
				{"id": 1, "name": "alice"},
				{"id": 2, "name": nil},
			},
		}}})
	})
	mux.HandleFunc("GET /api/data_sources", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{
			{"id": 1, "name": "warehouse"},
			{"id": 2, "name": "logs"},
		})
	})
	mux.HandleFunc("POST /api/queries", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&f.createdQuery); err != nil {
			f.t.Errorf("decode created query: %v", err)
		}
		// Redash forces every new query to be a draft, whatever the request
		// said, so the fake serves is_draft back as true.
		writeJSON(w, map[string]any{
			"id": 7, "name": f.createdQuery["name"], "query": f.createdQuery["query"],
			"data_source_id": f.createdQuery["data_source_id"], "is_draft": true,
		})
	})
	mux.HandleFunc("GET /api/queries/7", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		query := map[string]any{
			"id": 7, "name": "saved", "query": "SELECT 1", "data_source_id": 3,
			"is_draft": true, "version": f.queryVersion,
		}
		if f.queryOptions != nil {
			query["options"] = f.queryOptions
		}
		writeJSON(w, query)
	})
	mux.HandleFunc("POST /api/queries/7/results", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&f.refreshBody); err != nil {
			f.t.Errorf("decode refresh body: %v", err)
		}
		writeJSON(w, map[string]any{"job": map[string]any{"id": "job-1", "status": 1}})
	})
	mux.HandleFunc("POST /api/queries/7", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&f.updatedQuery); err != nil {
			f.t.Errorf("decode updated query: %v", err)
		}
		if f.updateStatus != 0 && f.updateStatus != http.StatusOK {
			w.WriteHeader(f.updateStatus)
			writeJSON(w, map[string]any{"message": "Query version conflict"})
			return
		}
		writeJSON(w, map[string]any{"id": 7, "name": "saved", "is_draft": false})
	})
	mux.HandleFunc("GET /api/queries", f.handleQueryList)
	mux.HandleFunc("GET /api/queries/my", f.handleQueryList)
	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Key good-key" {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]any{"message": "Couldn't find resource. Please login and try again."})
			return
		}
		writeJSON(w, map[string]any{"id": 1})
	})
	return httptest.NewServer(mux)
}

// handleQueryList serves both listing endpoints the way the real ones do,
// including the two refusals a client walking the pages has to stay clear
// of: Redash's paginate() answers a page past the end with 400 "Page is out
// of range" rather than an empty page, and bounds page_size at 1-250. The
// fake refuses them too, so a walk that oversteps fails a test here instead
// of only against a real server.
func (f *fakeRedash) handleQueryList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	values := r.URL.Query()
	f.listRequests = append(f.listRequests, listRequest{path: r.URL.Path, values: values})

	page, err := strconv.Atoi(values.Get("page"))
	if err != nil {
		f.t.Errorf("page = %q: %v", values.Get("page"), err)
	}
	pageSize, err := strconv.Atoi(values.Get("page_size"))
	if err != nil {
		f.t.Errorf("page_size = %q: %v", values.Get("page_size"), err)
	}

	if pageSize < 1 || pageSize > 250 {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"message": "Page size is out of range (1-250)."})
		return
	}
	if (page-1)*pageSize+1 > f.savedQueries && f.savedQueries > 0 {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"message": "Page is out of range."})
		return
	}

	results := []map[string]any{}
	for i := (page - 1) * pageSize; i < f.savedQueries && len(results) < pageSize; i++ {
		results = append(results, map[string]any{
			"id": i + 1, "name": fmt.Sprintf("query %d", i+1), "is_draft": i%2 == 0,
		})
	}
	writeJSON(w, map[string]any{
		"count": f.savedQueries, "page": page, "page_size": pageSize, "results": results,
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}

func newTestClient(srv *httptest.Server, key string) *redash.Client {
	c := redash.NewClient(srv.URL, key)
	c.PollInterval = time.Millisecond
	return c
}

func TestRunQuerySuccess(t *testing.T) {
	f := &fakeRedash{t: t, jobStatuses: []int{1, 2, 3}}
	srv := f.server()
	defer srv.Close()

	c := newTestClient(srv, "k")
	got, err := c.RunQuery(context.Background(), "SELECT 1", 5)
	if err != nil {
		t.Fatalf("RunQuery() error = %v", err)
	}

	if len(got.Columns) != 2 || got.Columns[0].Name != "id" || got.Columns[1].Name != "name" {
		t.Errorf("Columns = %+v, want id, name", got.Columns)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("len(Rows) = %d, want 2", len(got.Rows))
	}
	if got.Rows[1]["name"] != nil {
		t.Errorf("Rows[1][name] = %v, want nil", got.Rows[1]["name"])
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submittedBody["query"] != "SELECT 1" {
		t.Errorf("submitted query = %v", f.submittedBody["query"])
	}
	if f.submittedBody["data_source_id"] != float64(5) {
		t.Errorf("submitted data_source_id = %v, want 5", f.submittedBody["data_source_id"])
	}
	if maxAge, ok := f.submittedBody["max_age"]; !ok || maxAge != float64(0) {
		t.Errorf("submitted max_age = %v (present=%v), want 0", maxAge, ok)
	}
	for _, a := range f.gotAuth {
		if a != "Key k" {
			t.Errorf("Authorization = %q, want %q", a, "Key k")
		}
	}
}

// TestRunQueryRefusalCarriesItsMessage pins that a refusal Redash reports
// as a failed job rather than as a message still reaches the caller as one.
// run_query answers a paused data source, and a saved query missing a
// parameter value, with {"job": {"status": 4, "error": ...}} and an HTTP
// 400; reading the message field alone would leave the caller the status
// and nothing to act on.
func TestRunQueryRefusalCarriesItsMessage(t *testing.T) {
	f := &fakeRedash{t: t, submitRefusal: "warehouse is paused. Please try later."}
	srv := f.server()
	defer srv.Close()

	_, err := newTestClient(srv, "k").RunQuery(context.Background(), "SELECT 1", 5)
	if err == nil {
		t.Fatal("RunQuery() error = nil, want the server's refusal")
	}
	if !strings.Contains(err.Error(), "warehouse is paused") {
		t.Errorf("RunQuery() error = %v, want it to carry the server's message", err)
	}
}

func TestRunQueryJobFailure(t *testing.T) {
	f := &fakeRedash{t: t, jobStatuses: []int{2, 4}, jobError: "syntax error at or near \"SELEC\""}
	srv := f.server()
	defer srv.Close()

	_, err := newTestClient(srv, "k").RunQuery(context.Background(), "SELEC 1", 5)
	if err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("RunQuery() error = %v, want the job error surfaced", err)
	}
}

func TestRunQueryJobCancelledServerSide(t *testing.T) {
	f := &fakeRedash{t: t, jobStatuses: []int{5}}
	srv := f.server()
	defer srv.Close()

	_, err := newTestClient(srv, "k").RunQuery(context.Background(), "SELECT 1", 5)
	if err == nil || !strings.Contains(err.Error(), "cancel") {
		t.Errorf("RunQuery() error = %v, want cancelled-job error", err)
	}
}

func TestRunQueryContextExpiryCancelsJob(t *testing.T) {
	f := &fakeRedash{t: t, jobStatuses: []int{1}} // never finishes
	srv := f.server()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := newTestClient(srv, "k").RunQuery(ctx, "SELECT pg_sleep(600)", 5)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunQuery() error = %v, want DeadlineExceeded", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.cancelled {
		t.Error("job was not cancelled on the server after context expiry")
	}
}

func TestRunQueryCancelFailureIsBestEffort(t *testing.T) {
	f := &fakeRedash{t: t, jobStatuses: []int{1}, cancelStatus: http.StatusInternalServerError}
	srv := f.server()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := newTestClient(srv, "k").RunQuery(ctx, "SELECT 1", 5)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("RunQuery() error = %v, want DeadlineExceeded even when cancel fails", err)
	}
}

func TestListDataSources(t *testing.T) {
	f := &fakeRedash{t: t}
	srv := f.server()
	defer srv.Close()

	dss, err := newTestClient(srv, "k").ListDataSources(context.Background())
	if err != nil {
		t.Fatalf("ListDataSources() error = %v", err)
	}
	if len(dss) != 2 || dss[0].ID != 1 || dss[0].Name != "warehouse" {
		t.Errorf("ListDataSources() = %+v", dss)
	}
}

func TestNewClientTrimsTrailingSlash(t *testing.T) {
	f := &fakeRedash{t: t}
	srv := f.server()
	defer srv.Close()

	c := redash.NewClient(srv.URL+"/", "good-key")
	if err := c.GetSession(context.Background()); err != nil {
		t.Errorf("GetSession() with trailing-slash base URL error = %v", err)
	}
}

func TestGetSession(t *testing.T) {
	f := &fakeRedash{t: t}
	srv := f.server()
	defer srv.Close()

	if err := newTestClient(srv, "good-key").GetSession(context.Background()); err != nil {
		t.Errorf("GetSession() with valid key error = %v", err)
	}

	err := newTestClient(srv, "bad-key").GetSession(context.Background())
	if err == nil {
		t.Fatal("GetSession() with invalid key: want error")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "login") {
		t.Errorf("GetSession() error = %v, want HTTP status and server message included", err)
	}
}

func TestCreateQuery(t *testing.T) {
	f := &fakeRedash{t: t}
	srv := f.server()
	defer srv.Close()

	q, err := newTestClient(srv, "k").CreateQuery(context.Background(), redash.NewQuery{
		Name: "saved", Query: "SELECT 1", DataSourceID: 3, Description: "why it exists",
	})
	if err != nil {
		t.Fatalf("CreateQuery() error = %v", err)
	}
	if q.ID != 7 || !q.IsDraft {
		t.Errorf("CreateQuery() = %+v, want ID 7 and the draft Redash forces on creation", q)
	}
	want := map[string]any{
		"name": "saved", "query": "SELECT 1", "data_source_id": float64(3),
		"description": "why it exists",
	}
	if !reflect.DeepEqual(f.createdQuery, want) {
		t.Errorf("created query body = %v, want %v", f.createdQuery, want)
	}
}

// TestCreateQueryOmitsEmptyDescription keeps an unset --description from
// being sent as an empty one, which would overwrite nothing on creation but
// makes the request say something the caller did not.
func TestCreateQueryOmitsEmptyDescription(t *testing.T) {
	f := &fakeRedash{t: t}
	srv := f.server()
	defer srv.Close()

	if _, err := newTestClient(srv, "k").CreateQuery(context.Background(), redash.NewQuery{
		Name: "saved", Query: "SELECT 1", DataSourceID: 3,
	}); err != nil {
		t.Fatalf("CreateQuery() error = %v", err)
	}
	if _, ok := f.createdQuery["description"]; ok {
		t.Errorf("created query body = %v, want no description key", f.createdQuery)
	}
}

// TestUpdateQuery checks that only the fields the caller set are sent: an
// update that also carried the fields it left alone would overwrite a query
// edited in the Redash UI since it was read.
func TestUpdateQuery(t *testing.T) {
	f := &fakeRedash{t: t}
	srv := f.server()
	defer srv.Close()

	draft := false
	q, err := newTestClient(srv, "k").UpdateQuery(context.Background(), 7, redash.QueryUpdate{IsDraft: &draft})
	if err != nil {
		t.Fatalf("UpdateQuery() error = %v", err)
	}
	if q.ID != 7 || q.IsDraft {
		t.Errorf("UpdateQuery() = %+v, want ID 7 and is_draft false", q)
	}
	if want := map[string]any{"is_draft": false}; !reflect.DeepEqual(f.updatedQuery, want) {
		t.Errorf("updated query body = %v, want %v", f.updatedQuery, want)
	}
}

// TestGetQuery covers the read an update needs: the version, which is what
// makes the following write fail rather than overwrite a query someone else
// edited in between.
func TestGetQuery(t *testing.T) {
	f := &fakeRedash{t: t, queryVersion: 4}
	srv := f.server()
	defer srv.Close()

	q, err := newTestClient(srv, "k").GetQuery(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetQuery() error = %v", err)
	}
	if q.ID != 7 || q.Version != 4 || q.Query != "SELECT 1" || !q.IsDraft {
		t.Errorf("GetQuery() = %+v, want ID 7, version 4, the saved SQL and is_draft true", q)
	}
}

// TestGetQueryOptions covers the half of the read a saved-query execution
// needs: the stored parameter defaults, which are both what the execution
// sends and what tells it whether the query page's cached result will
// advance. The default is kept as it was decoded, so a number stays a
// number on the way back out.
func TestGetQueryOptions(t *testing.T) {
	f := &fakeRedash{t: t, queryOptions: map[string]any{
		"apply_auto_limit": true,
		"parameters": []map[string]any{
			{"name": "days", "type": "number", "value": 7},
			{"name": "team", "type": "text", "value": "core"},
			{"name": "since", "type": "date", "value": nil},
		},
	}}
	srv := f.server()
	defer srv.Close()

	q, err := newTestClient(srv, "k").GetQuery(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetQuery() error = %v", err)
	}
	want := []redash.QueryParameter{
		{Name: "days", Value: json.Number("7")},
		{Name: "team", Value: "core"},
		{Name: "since", Value: nil},
	}
	if !reflect.DeepEqual(q.Options.Parameters, want) {
		t.Errorf("Options.Parameters = %+v, want %+v", q.Options.Parameters, want)
	}
}

// TestGetQueryWithoutOptions pins that a query carrying no options at all —
// which is every query `rdsh query create` saves — reads back as one with
// no parameters rather than failing.
func TestGetQueryWithoutOptions(t *testing.T) {
	f := &fakeRedash{t: t}
	srv := f.server()
	defer srv.Close()

	q, err := newTestClient(srv, "k").GetQuery(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetQuery() error = %v", err)
	}
	if len(q.Options.Parameters) != 0 {
		t.Errorf("Options.Parameters = %+v, want none", q.Options.Parameters)
	}
}

func TestRefreshQuery(t *testing.T) {
	f := &fakeRedash{t: t, jobStatuses: []int{1, 3}}
	srv := f.server()
	defer srv.Close()

	got, err := newTestClient(srv, "k").RefreshQuery(context.Background(), 7,
		map[string]any{"days": json.Number("7"), "team": "core"})
	if err != nil {
		t.Fatalf("RefreshQuery() error = %v", err)
	}
	if len(got.Rows) != 2 {
		t.Errorf("len(Rows) = %d, want 2", len(got.Rows))
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// A number default has to arrive as a number: the hash Redash compares
	// the execution against is taken over the query rendered with the
	// stored defaults, so a stringified one would run a different text.
	wantParams := map[string]any{"days": float64(7), "team": "core"}
	if !reflect.DeepEqual(f.refreshBody["parameters"], wantParams) {
		t.Errorf("submitted parameters = %#v, want %#v", f.refreshBody["parameters"], wantParams)
	}
	if maxAge, ok := f.refreshBody["max_age"]; !ok || maxAge != float64(0) {
		t.Errorf("submitted max_age = %v (present=%v), want 0", maxAge, ok)
	}
	// Absent rather than false: with the key missing the server falls back
	// to the query's own options.apply_auto_limit, which is the value its
	// stored hash was taken with. Sending false would run a different text
	// on any query that has it on.
	if _, ok := f.refreshBody["apply_auto_limit"]; ok {
		t.Errorf("submitted apply_auto_limit = %v, want the key left out",
			f.refreshBody["apply_auto_limit"])
	}
}

// TestRefreshQueryWithoutParameters pins that a query with nothing to
// substitute sends an empty object rather than null: Redash reads the field
// with a default that a present null defeats, and then fails on it.
func TestRefreshQueryWithoutParameters(t *testing.T) {
	f := &fakeRedash{t: t, jobStatuses: []int{3}}
	srv := f.server()
	defer srv.Close()

	if _, err := newTestClient(srv, "k").RefreshQuery(context.Background(), 7, nil); err != nil {
		t.Fatalf("RefreshQuery() error = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	params, ok := f.refreshBody["parameters"]
	if !ok {
		t.Fatal("submitted body carries no parameters field")
	}
	if !reflect.DeepEqual(params, map[string]any{}) {
		t.Errorf("submitted parameters = %#v, want an empty object", params)
	}
}

// TestRefreshQueryContextExpiryCancelsJob pins that an execution submitted
// by ID reaches the same abandonment RunQuery gets, rather than leaving the
// job running on the server.
func TestRefreshQueryContextExpiryCancelsJob(t *testing.T) {
	f := &fakeRedash{t: t, jobStatuses: []int{1}} // never finishes
	srv := f.server()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := newTestClient(srv, "k").RefreshQuery(ctx, 7, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RefreshQuery() error = %v, want DeadlineExceeded", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.cancelled {
		t.Error("job was not cancelled on the server")
	}
}

// TestUpdateQuerySendsVersion pins the conflict handling's request half: the
// version read beforehand travels with the update, which is what lets the
// server reject a write based on a stale read.
func TestUpdateQuerySendsVersion(t *testing.T) {
	f := &fakeRedash{t: t}
	srv := f.server()
	defer srv.Close()

	name, version := "renamed", 4
	if _, err := newTestClient(srv, "k").UpdateQuery(context.Background(), 7, redash.QueryUpdate{
		Name: &name, Version: &version,
	}); err != nil {
		t.Fatalf("UpdateQuery() error = %v", err)
	}
	want := map[string]any{"name": "renamed", "version": float64(4)}
	if !reflect.DeepEqual(f.updatedQuery, want) {
		t.Errorf("updated query body = %v, want %v", f.updatedQuery, want)
	}
}

// TestUpdateQueryVersionConflict pins the response half: the 409 a stale
// version earns is recognisable, so a caller can say what to do about it
// rather than pass an HTTP status on.
func TestUpdateQueryVersionConflict(t *testing.T) {
	f := &fakeRedash{t: t, updateStatus: http.StatusConflict}
	srv := f.server()
	defer srv.Close()

	version := 4
	_, err := newTestClient(srv, "k").UpdateQuery(context.Background(), 7, redash.QueryUpdate{Version: &version})
	if !errors.Is(err, redash.ErrQueryVersionConflict) {
		t.Fatalf("UpdateQuery() error = %v, want ErrQueryVersionConflict", err)
	}
}

// TestUpdateQueryConflictWithoutVersionIsNotAConflict keeps the sentinel
// tied to what it means. An update that sent no version cannot have lost a
// version check, so a 409 there is some other refusal and must not be
// reported as a query that changed underneath the caller.
func TestUpdateQueryConflictWithoutVersionIsNotAConflict(t *testing.T) {
	f := &fakeRedash{t: t, updateStatus: http.StatusConflict}
	srv := f.server()
	defer srv.Close()

	draft := false
	_, err := newTestClient(srv, "k").UpdateQuery(context.Background(), 7, redash.QueryUpdate{IsDraft: &draft})
	if err == nil {
		t.Fatal("UpdateQuery() error = nil, want the refusal reported")
	}
	if errors.Is(err, redash.ErrQueryVersionConflict) {
		t.Errorf("UpdateQuery() error = %v, want an ordinary failure rather than a version conflict", err)
	}
}

func TestQueryURL(t *testing.T) {
	if got := redash.NewClient("https://redash.example.com/", "k").QueryURL(7); got != "https://redash.example.com/queries/7" {
		t.Errorf("QueryURL(7) = %q, want the trailing slash trimmed", got)
	}
}

// listQueryIDs reduces a listing to the IDs it returned, which is all the
// walking tests need to check about the rows themselves.
func listQueryIDs(queries []redash.Query) []int {
	ids := make([]int, len(queries))
	for i, q := range queries {
		ids[i] = q.ID
	}
	return ids
}

// pagesRequested reports the page numbers the listing endpoints were asked
// for, in order. The walking tests assert on this rather than only on the
// rows: a client that asks for one page too many gets a 400 from the fake,
// but one that asks for pages it did not need would otherwise pass unseen.
func pagesRequested(reqs []listRequest) []string {
	pages := make([]string, len(reqs))
	for i, req := range reqs {
		pages[i] = req.values.Get("page")
	}
	return pages
}

func TestListQueries(t *testing.T) {
	f := &fakeRedash{t: t, savedQueries: 3}
	srv := f.server()
	defer srv.Close()

	queries, count, err := newTestClient(srv, "k").ListQueries(context.Background(), redash.QueryListOptions{Limit: 30})
	if err != nil {
		t.Fatalf("ListQueries() error = %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
	if want := []int{1, 2, 3}; !reflect.DeepEqual(listQueryIDs(queries), want) {
		t.Errorf("IDs = %v, want %v", listQueryIDs(queries), want)
	}
	if queries[0].Name != "query 1" || !queries[0].IsDraft {
		t.Errorf("queries[0] = %+v, want the name and draft flag the server served", queries[0])
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if want := []string{"1"}; !reflect.DeepEqual(pagesRequested(f.listRequests), want) {
		t.Errorf("pages requested = %v, want %v", pagesRequested(f.listRequests), want)
	}
	if got := f.listRequests[0].path; got != "/api/queries" {
		t.Errorf("path = %q, want /api/queries", got)
	}
	if _, ok := f.listRequests[0].values["q"]; ok {
		t.Errorf("query string = %v, want no q on an unfiltered listing", f.listRequests[0].values)
	}
}

// TestListQueriesWalksPages covers a limit larger than one page: the client
// keeps asking until it has the limit, and the rows arrive in server order
// rather than page-interleaved.
func TestListQueriesWalksPages(t *testing.T) {
	f := &fakeRedash{t: t, savedQueries: 300}
	srv := f.server()
	defer srv.Close()

	queries, count, err := newTestClient(srv, "k").ListQueries(context.Background(), redash.QueryListOptions{Limit: 250})
	if err != nil {
		t.Fatalf("ListQueries() error = %v", err)
	}
	if count != 300 {
		t.Errorf("count = %d, want the server's total 300 even though fewer rows were returned", count)
	}
	if len(queries) != 250 {
		t.Fatalf("len(queries) = %d, want the limit 250", len(queries))
	}
	if queries[0].ID != 1 || queries[249].ID != 250 {
		t.Errorf("IDs run %d..%d, want 1..250 in server order", queries[0].ID, queries[249].ID)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if want := []string{"1", "2", "3"}; !reflect.DeepEqual(pagesRequested(f.listRequests), want) {
		t.Errorf("pages requested = %v, want %v", pagesRequested(f.listRequests), want)
	}
}

// TestListQueriesStopsAtCount is the regression this walk is most likely to
// break: with the server holding exactly a whole number of pages, a client
// that probes for an empty page instead of stopping at count asks for one
// page too many, and the real server answers that with 400 rather than an
// empty page. The fake refuses it the same way, so this fails rather than
// silently costing a round trip.
func TestListQueriesStopsAtCount(t *testing.T) {
	f := &fakeRedash{t: t, savedQueries: 200}
	srv := f.server()
	defer srv.Close()

	queries, count, err := newTestClient(srv, "k").ListQueries(context.Background(), redash.QueryListOptions{Limit: 250})
	if err != nil {
		t.Fatalf("ListQueries() error = %v", err)
	}
	if count != 200 || len(queries) != 200 {
		t.Errorf("ListQueries() = %d rows, count %d, want all 200", len(queries), count)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if want := []string{"1", "2"}; !reflect.DeepEqual(pagesRequested(f.listRequests), want) {
		t.Errorf("pages requested = %v, want %v and no probe past the end", pagesRequested(f.listRequests), want)
	}
}

// TestListQueriesLimitBoundsThePageSize keeps a small limit from fetching a
// whole page to throw most of it away.
func TestListQueriesLimitBoundsThePageSize(t *testing.T) {
	f := &fakeRedash{t: t, savedQueries: 300}
	srv := f.server()
	defer srv.Close()

	queries, count, err := newTestClient(srv, "k").ListQueries(context.Background(), redash.QueryListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("ListQueries() error = %v", err)
	}
	if len(queries) != 5 || count != 300 {
		t.Errorf("ListQueries() = %d rows, count %d, want 5 rows out of 300", len(queries), count)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.listRequests) != 1 {
		t.Fatalf("requests = %d, want a single page", len(f.listRequests))
	}
	if got := f.listRequests[0].values.Get("page_size"); got != "5" {
		t.Errorf("page_size = %q, want the limit 5", got)
	}
}

func TestListQueriesSearch(t *testing.T) {
	f := &fakeRedash{t: t, savedQueries: 2}
	srv := f.server()
	defer srv.Close()

	if _, _, err := newTestClient(srv, "k").ListQueries(context.Background(), redash.QueryListOptions{
		Search: "signups", Limit: 30,
	}); err != nil {
		t.Fatalf("ListQueries() error = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.listRequests[0].values.Get("q"); got != "signups" {
		t.Errorf("q = %q, want the search term", got)
	}
	if got := f.listRequests[0].path; got != "/api/queries" {
		t.Errorf("path = %q, want /api/queries", got)
	}
}

func TestListQueriesMine(t *testing.T) {
	f := &fakeRedash{t: t, savedQueries: 2}
	srv := f.server()
	defer srv.Close()

	if _, _, err := newTestClient(srv, "k").ListQueries(context.Background(), redash.QueryListOptions{
		Mine: true, Search: "signups", Limit: 30,
	}); err != nil {
		t.Fatalf("ListQueries() error = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.listRequests[0].path; got != "/api/queries/my" {
		t.Errorf("path = %q, want /api/queries/my", got)
	}
	if got := f.listRequests[0].values.Get("q"); got != "signups" {
		t.Errorf("q = %q, want the search term to survive --mine", got)
	}
}

// TestListQueriesNoMatches covers the empty listing, which the real server
// answers with count 0 rather than the out-of-range refusal it gives for a
// page past a non-empty result.
func TestListQueriesNoMatches(t *testing.T) {
	f := &fakeRedash{t: t}
	srv := f.server()
	defer srv.Close()

	queries, count, err := newTestClient(srv, "k").ListQueries(context.Background(), redash.QueryListOptions{
		Search: "nothing matches this", Limit: 30,
	})
	if err != nil {
		t.Fatalf("ListQueries() error = %v", err)
	}
	if len(queries) != 0 || count != 0 {
		t.Errorf("ListQueries() = %d rows, count %d, want nothing", len(queries), count)
	}
}
