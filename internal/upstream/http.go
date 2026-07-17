package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type httpClient struct {
	name   string
	base   string
	key    string
	client *http.Client
	auth   func(*http.Request, string)
}

func (c *httpClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		c.auth(req, c.key)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return &DependencyError{Dependency: c.name, Operation: method + " " + path, Retryable: true, Cause: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return &DependencyError{Dependency: c.name, Operation: method + " " + path, Status: resp.StatusCode, Retryable: true, Cause: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &DependencyError{
			Dependency: c.name,
			Operation:  method + " " + path,
			Status:     resp.StatusCode,
			Retryable:  resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError,
			Cause:      fmt.Errorf("upstream response omitted"),
		}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return &DependencyError{Dependency: c.name, Operation: method + " " + path, Status: resp.StatusCode, Cause: err}
		}
	}
	return nil
}
