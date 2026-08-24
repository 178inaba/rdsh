// Package redash is a minimal Redash REST API client covering query
// execution, ad-hoc and by saved-query ID: submit, poll, fetch, cancel,
// plus saved-query creation, reading and editing, data-source listing and
// a session check for login verification.
package redash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Job statuses as defined by Redash.
const (
	StatusPending   = 1
	StatusStarted   = 2
	StatusSuccess   = 3
	StatusFailure   = 4
	StatusCancelled = 5
)

const cancelTimeout = 10 * time.Second

// ErrQueryVersionConflict reports that a saved query changed on the server
// between the read that supplied QueryUpdate.Version and the update that
// carried it, so the update was refused rather than applied on top of an
// edit the caller never saw.
var ErrQueryVersionConflict = errors.New("saved query changed on the server since it was read")

// Client talks to one Redash instance authenticated by a user API key
// (query API keys cannot run ad-hoc queries).
type Client struct {
	// PollInterval is the delay between job status checks.
	PollInterval time.Duration

	baseURL string
	apiKey  string
	httpc   *http.Client
}

// NewClient returns a client for the Redash instance at baseURL. A trailing
// slash is trimmed so env-pair and hand-edited config URLs do not produce
// double-slash request paths.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		PollInterval: time.Second,
		baseURL:      strings.TrimRight(baseURL, "/"),
		apiKey:       apiKey,
		httpc:        &http.Client{},
	}
}

// Job is the server-side execution of one submitted query.
type Job struct {
	ID            string `json:"id"`
	Status        int    `json:"status"`
	Error         string `json:"error"`
	QueryResultID int    `json:"query_result_id"`
}

// Column describes one result column. FriendlyName and Type can be null in
// real Redash responses.
type Column struct {
	Name         string `json:"name"`
	FriendlyName string `json:"friendly_name"`
	Type         string `json:"type"`
}

// QueryResult holds the fetched result data. Rows are maps keyed by column
// name; output ordering must come from Columns.
type QueryResult struct {
	Columns []Column
	Rows    []map[string]any
}

// Query is a saved Redash query.
type Query struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Query        string `json:"query"`
	DataSourceID int    `json:"data_source_id"`
	IsDraft      bool   `json:"is_draft"`
	// Version is what an update sends back to prove it was composed
	// against the query as it stands; Redash refuses one carrying a stale
	// value.
	Version int `json:"version"`
	// Options carries the settings the query holds beside its SQL. Only
	// the parameter definitions are read; the rest of the object is left
	// where it is, since nothing here writes options back.
	Options QueryOptions `json:"options"`
}

// QueryOptions is the part of a saved query's options rdsh reads. A query
// saved through the API carries no options at all, which reads back as the
// zero value rather than as a failure.
type QueryOptions struct {
	Parameters []QueryParameter `json:"parameters"`
}

// QueryParameter is one parameter defined on a saved query.
type QueryParameter struct {
	Name string `json:"name"`
	// Value is the stored default, decoded as it came: numbers stay
	// json.Number, so sending one back reproduces the text Redash hashed
	// the query with. A parameter defined without a default is nil.
	Value any `json:"value"`
}

// NewQuery holds the fields the API accepts when a saved query is created.
type NewQuery struct {
	Name         string `json:"name"`
	Query        string `json:"query"`
	DataSourceID int    `json:"data_source_id"`
	Description  string `json:"description,omitempty"`
}

// QueryUpdate holds the fields to change on a saved query; a nil field is
// left as it is. Pointers rather than plain fields so clearing a value
// (an empty description) stays distinguishable from not touching it.
type QueryUpdate struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	Query       *string `json:"query,omitempty"`
	IsDraft     *bool   `json:"is_draft,omitempty"`
	// Version opts into the server's conflict check: set it to the version
	// of the query this update was composed from and a competing edit fails
	// the update instead of being silently overwritten. Left nil, the
	// server applies the update as a last write.
	Version *int `json:"version,omitempty"`
}

// QueryListOptions narrows what ListQueries returns.
type QueryListOptions struct {
	// Search is the API's q full-text search. Empty means no filtering.
	Search string
	// Mine lists only the caller's own queries.
	Mine bool
	// Limit caps how many queries are returned. It must be at least 1;
	// the server refuses the page size a smaller one would ask for.
	Limit int
}

// DataSource is one Redash data source.
type DataSource struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// RunQuery submits an ad-hoc query (cache always bypassed via max_age=0)
// and polls until it finishes, returning the fetched result.
func (c *Client) RunQuery(ctx context.Context, query string, dataSourceID int) (*QueryResult, error) {
	job, err := c.submitQuery(ctx, query, dataSourceID)
	if err != nil {
		return nil, err
	}
	return c.awaitResult(ctx, job)
}

// RefreshQuery executes the saved query on the server and returns the
// fetched result. Unlike RunQuery, which runs SQL the server has never
// seen, this is the execution the query page itself makes: Redash records
// it against the saved query, so the cached result everyone sees advances
// — as long as parameters render the query to the text its stored hash was
// taken over.
//
// parameters supplies a value for every placeholder the query has; one left
// uncovered fails the execution on the server rather than here.
func (c *Client) RefreshQuery(ctx context.Context, id int, parameters map[string]any) (*QueryResult, error) {
	job, err := c.submitSavedQuery(ctx, id, parameters)
	if err != nil {
		return nil, err
	}
	return c.awaitResult(ctx, job)
}

// awaitResult polls the submitted job until it finishes and fetches what it
// produced. Both ways of submitting share it, so a query behaves the same
// on the way out whichever one started it: when ctx expires or is cancelled
// mid-flight, the server-side job is cancelled best-effort on a fresh
// short-lived context (the original one is already dead) and the returned
// error wraps ctx.Err() so callers can map timeout vs interrupt to exit
// codes.
func (c *Client) awaitResult(ctx context.Context, job *Job) (*QueryResult, error) {
	// The job ID is kept separately because getJob returns nil on error and
	// the ID must survive a poll that failed due to ctx expiry.
	jobID := job.ID

	for {
		switch job.Status {
		case StatusPending, StatusStarted:
		case StatusSuccess:
			return c.getQueryResult(ctx, job.QueryResultID)
		case StatusFailure:
			return nil, fmt.Errorf("query failed: %s", job.Error)
		case StatusCancelled:
			return nil, errors.New("job was cancelled on the server")
		default:
			return nil, fmt.Errorf("job entered unknown status %d", job.Status)
		}

		select {
		case <-ctx.Done():
			return nil, c.abandonJob(ctx, jobID)
		case <-time.After(c.PollInterval):
		}

		var err error
		if job, err = c.getJob(ctx, jobID); err != nil {
			if ctx.Err() != nil {
				return nil, c.abandonJob(ctx, jobID)
			}
			return nil, err
		}
	}
}

// abandonJob cancels the job best-effort and returns the context error,
// joined with the cancellation failure if one occurred (errors.Is still
// matches ctx.Err() through the join).
func (c *Client) abandonJob(ctx context.Context, jobID string) error {
	ctxErr := ctx.Err()
	if jobID == "" {
		return ctxErr
	}
	cancelCtx, cancel := context.WithTimeout(context.Background(), cancelTimeout)
	defer cancel()
	if err := c.CancelJob(cancelCtx, jobID); err != nil {
		return errors.Join(ctxErr, fmt.Errorf("additionally, cancelling job %s failed: %w", jobID, err))
	}
	return ctxErr
}

func (c *Client) submitQuery(ctx context.Context, query string, dataSourceID int) (*Job, error) {
	body := map[string]any{
		"query":          query,
		"data_source_id": dataSourceID,
		"max_age":        0, // always bypass the Redash result cache
	}
	var resp struct {
		Job *Job `json:"job"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/query_results", body, &resp); err != nil {
		return nil, err
	}
	if resp.Job == nil {
		return nil, errors.New("server did not return a job")
	}
	return resp.Job, nil
}

// submitSavedQuery asks the server to execute the saved query with the
// given parameter values.
func (c *Client) submitSavedQuery(ctx context.Context, id int, parameters map[string]any) (*Job, error) {
	if parameters == nil {
		// The server reads the field with a default an explicit null
		// defeats, and then fails on the null it got instead.
		parameters = map[string]any{}
	}
	// apply_auto_limit is deliberately absent rather than false: left out,
	// the server falls back to the query's own options.apply_auto_limit,
	// which is the setting its stored hash was taken with. Sending false
	// would execute a different text on any query that has it on, and the
	// cached result everyone sees would not advance.
	body := map[string]any{
		"parameters": parameters,
		"max_age":    0, // always execute rather than serve the cached result
	}
	var resp struct {
		Job *Job `json:"job"`
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/queries/%d/results", id), body, &resp); err != nil {
		return nil, err
	}
	if resp.Job == nil {
		return nil, errors.New("server did not return a job")
	}
	return resp.Job, nil
}

func (c *Client) getJob(ctx context.Context, id string) (*Job, error) {
	var resp struct {
		Job *Job `json:"job"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/jobs/"+id, nil, &resp); err != nil {
		return nil, err
	}
	if resp.Job == nil {
		return nil, errors.New("server did not return a job")
	}
	return resp.Job, nil
}

// CancelJob asks the server to stop the job.
func (c *Client) CancelJob(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/jobs/"+id, nil, nil)
}

func (c *Client) getQueryResult(ctx context.Context, resultID int) (*QueryResult, error) {
	var resp struct {
		QueryResult struct {
			Data struct {
				Columns []Column         `json:"columns"`
				Rows    []map[string]any `json:"rows"`
			} `json:"data"`
		} `json:"query_result"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/query_results/%d", resultID), nil, &resp); err != nil {
		return nil, err
	}
	return &QueryResult{Columns: resp.QueryResult.Data.Columns, Rows: resp.QueryResult.Data.Rows}, nil
}

// CreateQuery saves a new query. Redash forces the new query to be a draft
// whatever the request body says, so publishing it is a second call to
// UpdateQuery rather than a field set here.
//
// Redash attaches the latest result of a matching query text and data
// source to the new query as it is created, which is what lets a run
// followed by a create share results without executing them twice.
func (c *Client) CreateQuery(ctx context.Context, q NewQuery) (*Query, error) {
	var created Query
	if err := c.do(ctx, http.MethodPost, "/api/queries", q, &created); err != nil {
		return nil, err
	}
	return &created, nil
}

// GetQuery reads one saved query, including the Version an update needs.
func (c *Client) GetQuery(ctx context.Context, id int) (*Query, error) {
	var q Query
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/queries/%d", id), nil, &q); err != nil {
		return nil, err
	}
	return &q, nil
}

// UpdateQuery changes the fields u sets on the saved query and returns it
// as the server now holds it. When u carries a Version, the 409 a stale one
// earns is wrapped in ErrQueryVersionConflict; without one the server
// applies the update unconditionally, so a 409 means something else and is
// returned as it came.
func (c *Client) UpdateQuery(ctx context.Context, id int, u QueryUpdate) (*Query, error) {
	var updated Query
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/queries/%d", id), u, &updated); err != nil {
		var status *statusError
		if u.Version != nil && errors.As(err, &status) && status.statusCode == http.StatusConflict {
			return nil, fmt.Errorf("%w: %v", ErrQueryVersionConflict, err)
		}
		return nil, err
	}
	return &updated, nil
}

// queryListPageSize is how many queries one listing request asks for. The
// server bounds page_size at 1-250; staying well inside that leaves room
// for an instance with a stricter bound, and one request still covers any
// ordinary limit.
const queryListPageSize = 100

// ListQueries returns up to opts.Limit saved queries and the total number
// the server holds, which is what lets a caller tell that the limit
// truncated the listing rather than that it saw everything.
//
// Without a search term the server orders them newest first; with one the
// order is its own search ranking, so no order is promised here.
//
// The API paginates, so this walks the pages itself. It stops from the
// count the server reports rather than by probing for an empty page:
// Redash refuses a page past the end with 400 "Page is out of range".
func (c *Client) ListQueries(ctx context.Context, opts QueryListOptions) ([]Query, int, error) {
	path := "/api/queries"
	if opts.Mine {
		path = "/api/queries/my"
	}
	values := url.Values{}
	values.Set("page_size", strconv.Itoa(min(opts.Limit, queryListPageSize)))
	if opts.Search != "" {
		values.Set("q", opts.Search)
	}

	queries, count := []Query{}, 0
	for page := 1; ; page++ {
		values.Set("page", strconv.Itoa(page))
		var resp struct {
			Count   int     `json:"count"`
			Results []Query `json:"results"`
		}
		if err := c.do(ctx, http.MethodGet, path+"?"+values.Encode(), nil, &resp); err != nil {
			return nil, 0, err
		}
		count = resp.Count
		queries = append(queries, resp.Results...)
		// The empty-page check is a backstop rather than the stop
		// condition: a count that disagrees with what the pages actually
		// hold would otherwise loop until the server refused a page.
		if len(queries) >= opts.Limit || len(queries) >= count || len(resp.Results) == 0 {
			break
		}
	}
	// The last page can overshoot when the limit is not a multiple of the
	// page size.
	if len(queries) > opts.Limit {
		queries = queries[:opts.Limit]
	}
	return queries, count, nil
}

// QueryURL is where a saved query is read in a browser. The API returns no
// URL of its own, so it is built from the base URL NewClient normalised.
func (c *Client) QueryURL(id int) string {
	return fmt.Sprintf("%s%d", c.QueryURLPrefix(), id)
}

// QueryURLPrefix is what every query URL on this instance begins with. A
// caller reading a query URL matches against this rather than rebuilding it
// from the base URL, so the two directions cannot drift apart.
func (c *Client) QueryURLPrefix() string {
	return c.baseURL + "/queries/"
}

// ListDataSources returns the data sources visible to the API key.
func (c *Client) ListDataSources(ctx context.Context) ([]DataSource, error) {
	var dss []DataSource
	if err := c.do(ctx, http.MethodGet, "/api/data_sources", nil, &dss); err != nil {
		return nil, err
	}
	return dss, nil
}

// GetSession verifies the base URL and API key with an authenticated
// request; auth login calls this before saving anything.
func (c *Client) GetSession(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/api/session", nil, nil)
}

// do sends one authenticated request and decodes the JSON response into out
// (skipped when out is nil). Non-2xx responses become errors carrying the
// HTTP status and the server's message field when present.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Key "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(method, path, resp)
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(resp.Body)
	// Keep numbers as json.Number so large integers survive the round trip
	// to CSV/JSON output instead of degrading to float64.
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

// statusError is a non-2xx response. The status is kept rather than only
// formatted into the message so a caller that treats one code as its own
// outcome can recognise it without matching on text.
type statusError struct {
	method     string
	path       string
	statusCode int
	message    string
}

func (e *statusError) Error() string {
	msg := ""
	if e.message != "" {
		msg = ": " + e.message
	}
	return fmt.Sprintf("%s %s: HTTP %d%s", e.method, e.path, e.statusCode, msg)
}

func apiError(method, path string, resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	// Two shapes, because Redash refuses a query run in a different one
	// from everything else: run_query answers a paused data source or a
	// parameter value it was never given with a failed job rather than
	// with the message field flask_restful's abort() produces. Reading
	// only the latter would leave the caller an HTTP status and nothing
	// to act on.
	var payload struct {
		Message string `json:"message"`
		Job     struct {
			Error string `json:"error"`
		} `json:"job"`
	}
	msg := ""
	if json.Unmarshal(data, &payload) == nil {
		msg = payload.Message
		if msg == "" {
			msg = payload.Job.Error
		}
	}
	return &statusError{method: method, path: path, statusCode: resp.StatusCode, message: msg}
}
