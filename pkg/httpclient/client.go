// Package httpclient provides a thin wrapper around net/http with timeout and
// retry support for outbound webhook calls.
package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is a configured HTTP client.
type Client struct {
	inner   *http.Client
	retries int
}

// New returns a Client with the given timeout (seconds) and retry count.
func New(timeoutSec, retries int) *Client {
	if timeoutSec <= 0 {
		timeoutSec = 15
	}
	return &Client{
		inner:   &http.Client{Timeout: time.Duration(timeoutSec) * time.Second},
		retries: retries,
	}
}

// PostJSON marshals payload as JSON and POSTs it to url.
// Returns an error if the response status is not 2xx.
func (c *Client) PostJSON(ctx context.Context, url string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*2) * time.Second):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.inner.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close() //nolint:errcheck
		respBody, _ := io.ReadAll(resp.Body)

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	return fmt.Errorf("post failed after %d attempt(s): %w", c.retries+1, lastErr)
}
