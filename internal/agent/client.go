package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/tnmt/kiseki/internal/store"
)

// Batch is the body of one ingest request.
type Batch struct {
	Source  string         `json:"source"`
	Records []store.Record `json:"records"`
}

// RejectedError means the server refused the request and retrying the same
// body will not help.
type RejectedError struct {
	Status  int
	Message string
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("server rejected the request (%d): %s", e.Status, e.Message)
}

type Client struct {
	base    string
	token   string
	http    *http.Client
	retries int
	backoff time.Duration
}

func NewClient(base, token string) *Client {
	return &Client{base: base, token: token, http: &http.Client{Timeout: 60 * time.Second}, retries: 3, backoff: time.Second}
}

// Post sends a batch, retrying network errors and 5xx responses with
// exponential backoff. A 4xx response returns *RejectedError at once.
func (c *Client) Post(ctx context.Context, b Batch) (store.UpsertResult, error) {
	body, err := json.Marshal(b)
	if err != nil {
		return store.UpsertResult{}, err
	}
	var res store.UpsertResult
	err = c.do(ctx, http.MethodPost, "/api/v1/ingest", body, &res)
	return res, err
}

// Cursor returns the newest occurred_at the server holds for this agent's host
// and source.
func (c *Client) Cursor(ctx context.Context, source string) (int64, bool, error) {
	var out struct {
		Latest *int64 `json:"latest_occurred_at"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/cursors?source="+url.QueryEscape(source), nil, &out); err != nil {
		return 0, false, err
	}
	if out.Latest == nil {
		return 0, false, nil
	}
	return *out.Latest, true, nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	var lastErr error
	delay := c.backoff
	for attempt := 0; attempt < c.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			return json.Unmarshal(data, out)
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("server error %d", resp.StatusCode)
		default:
			var e struct {
				Error string `json:"error"`
			}
			json.Unmarshal(data, &e)
			return &RejectedError{Status: resp.StatusCode, Message: e.Error}
		}
	}
	return lastErr
}
