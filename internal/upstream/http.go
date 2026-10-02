package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxRateLimitRetries = 8

type httpClient struct {
	name   string
	base   string
	key    string
	client *http.Client
	auth   func(*http.Request, string)
}

func (c *httpClient) do(ctx context.Context, method, path string, body any, out any) error {
	return c.doRateLimited(ctx, method, path, body, out, false)
}

// doWithRateLimitRetry is limited to the durable Brain extraction path.
// Interactive reads and legacy writes keep their ordinary one-attempt
// client deadline; callers that opt in must supply an overall deadline.
func (c *httpClient) doWithRateLimitRetry(ctx context.Context, method, path string, body any, out any) error {
	return c.doRateLimited(ctx, method, path, body, out, true)
}

func (c *httpClient) doRateLimited(ctx context.Context, method, path string, body any, out any, retry429 bool) error {
	var data []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		data = encoded
	}
	operation := method + " " + safeOperationPath(path)
	var resp *http.Response
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
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
		resp, err = c.client.Do(req)
		if err != nil {
			return &DependencyError{Dependency: c.name, Operation: operation, Retryable: true, Cause: err}
		}
		if !retry429 || resp.StatusCode != http.StatusTooManyRequests || attempt >= maxRateLimitRetries {
			break
		}
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		_ = resp.Body.Close()
		if err := waitForRetry(ctx, retryAfter); err != nil {
			return &DependencyError{
				Dependency: c.name, Operation: operation, Status: http.StatusTooManyRequests,
				Retryable: true, RetryAfter: retryAfter, Cause: err,
			}
		}
	}
	defer resp.Body.Close()
	responseData, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return &DependencyError{Dependency: c.name, Operation: operation, Status: resp.StatusCode, Retryable: true, Cause: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		return &DependencyError{
			Dependency: c.name,
			Operation:  operation,
			Status:     resp.StatusCode,
			Retryable:  resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError,
			RetryAfter: retryAfter,
			Cause:      fmt.Errorf("upstream response omitted"),
		}
	}
	if out != nil && len(responseData) > 0 {
		if err := json.Unmarshal(responseData, out); err != nil {
			return &DependencyError{Dependency: c.name, Operation: operation, Status: resp.StatusCode, Cause: err}
		}
	}
	return nil
}

func safeOperationPath(path string) string {
	path, _, _ = strings.Cut(path, "?")
	trimmed := strings.Trim(path, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) >= 3 && parts[0] == "v1" {
		switch parts[1] {
		case "documents":
			suffix := ""
			if len(parts) > 3 {
				suffix = "/" + parts[3]
			}
			return "/v1/documents/{id}" + suffix
		case "entities":
			suffix := ""
			if len(parts) > 3 {
				suffix = "/" + parts[3]
			}
			return "/v1/entities/{id}" + suffix
		case "facts":
			suffix := ""
			if len(parts) > 3 {
				suffix = "/" + parts[3]
			}
			return "/v1/facts/{id}" + suffix
		}
	}
	return "/" + trimmed
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if delay := at.Sub(now); delay > 0 {
			return delay
		}
	}
	return time.Second
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
