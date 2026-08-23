// Package redash is a minimal Redash REST API client covering ad-hoc query
// execution: submit, poll, fetch, cancel, plus saved-query creation and
// editing, data-source listing and a session check for login verification.
package redash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
}

// DataSource is one Redash data source.
type DataSource struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// RunQuery submits an ad-hoc query (cache always bypassed via max_age=0) and
// polls until it finishes, returning the fetched result. When ctx expires or
// is cancelled mid-flight, the server-side job is cancelled best-effort on a
// fresh short-lived context (the original one is already dead) and the
// returned error wraps ctx.Err() so callers can map timeout vs interrupt to
// exit codes.
func (c *Client) RunQuery(ctx context.Context, query string, dataSourceID int) (*QueryResult, error) {
	job, err := c.submitQuery(ctx, query, dataSourceID)
	if err != nil {
		return nil, err
	}

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

// UpdateQuery changes the fields u sets on the saved query and returns it
// as the server now holds it.
func (c *Client) UpdateQuery(ctx context.Context, id int, u QueryUpdate) (*Query, error) {
	var updated Query
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/queries/%d", id), u, &updated); err != nil {
		return nil, err
	}
	return &updated, nil
}

// QueryURL is where a saved query is read in a browser. The API returns no
// URL of its own, so it is built from the base URL NewClient normalised.
func (c *Client) QueryURL(id int) string {
	return fmt.Sprintf("%s/queries/%d", c.baseURL, id)
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

func apiError(method, path string, resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var payload struct {
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(data, &payload) == nil && payload.Message != "" {
		msg = ": " + payload.Message
	}
	return fmt.Errorf("%s %s: HTTP %d%s", method, path, resp.StatusCode, msg)
}
