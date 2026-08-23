package redash_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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

	mu            sync.Mutex
	jobStatuses   []int  // consecutive GET /api/jobs responses
	jobError      string // error field served with status 4
	pollCount     int
	cancelled     bool
	cancelStatus  int // HTTP status for DELETE, default 200
	submittedBody map[string]any
	createdQuery  map[string]any // body of POST /api/queries
	updatedQuery  map[string]any // body of POST /api/queries/7
	gotAuth       []string
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
	mux.HandleFunc("POST /api/queries/7", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&f.updatedQuery); err != nil {
			f.t.Errorf("decode updated query: %v", err)
		}
		writeJSON(w, map[string]any{"id": 7, "name": "saved", "is_draft": false})
	})
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

func TestQueryURL(t *testing.T) {
	if got := redash.NewClient("https://redash.example.com/", "k").QueryURL(7); got != "https://redash.example.com/queries/7" {
		t.Errorf("QueryURL(7) = %q, want the trailing slash trimmed", got)
	}
}
