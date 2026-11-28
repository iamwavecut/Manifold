// Package llm is a small client for OpenAI-compatible chat completion and
// embedding APIs, such as OpenAI itself or OpenRouter.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Refusal classes. A billing or auth refusal needs an operator; the others
// pass on their own.
const (
	ClassBilling   = "billing"
	ClassAuth      = "auth"
	ClassRateLimit = "rate_limit"
	ClassOutage    = "outage"
)

type Config struct {
	BaseURL        string
	APIKey         string
	ChatModel      string
	EmbeddingModel string
	// Dimensions is the embedding size the caller stores. Longer vectors are
	// truncated and normalized again, which keeps Matryoshka embeddings such
	// as text-embedding-3 usable when a proxy ignores the dimensions field.
	Dimensions int
	// ExtraBody is merged into every chat request; fields the call sets win.
	ExtraBody  map[string]any
	HTTPClient *http.Client
}

type Client struct {
	cfg       Config
	http      *http.Client
	attempts  int
	retryBase time.Duration
	// health is 0 before the first call, 1 after a success and 2 after a
	// failure.
	health atomic.Int32
}

func New(cfg Config) *Client {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute}
	}
	return &Client{cfg: cfg, http: client, attempts: 3, retryBase: time.Second}
}

// Configured reports whether the client has what a call needs.
func (c *Client) Configured() bool {
	return c.cfg.BaseURL != "" && c.cfg.APIKey != "" && c.cfg.ChatModel != "" &&
		c.cfg.EmbeddingModel != "" && c.cfg.Dimensions > 0
}

// Health is "unknown" before the first call, then "ready" or "degraded"
// after the last one. It sends no request.
func (c *Client) Health() string {
	switch c.health.Load() {
	case 1:
		return "ready"
	case 2:
		return "degraded"
	default:
		return "unknown"
	}
}

// Error is a failed provider call. It never carries the response body or the
// key.
type Error struct {
	Operation  string
	Status     int
	Class      string
	RetryAfter time.Duration
	cause      error
}

func (e *Error) Error() string {
	text := "model provider " + e.Operation
	if e.Status > 0 {
		text += " failed with HTTP " + strconv.Itoa(e.Status)
	} else {
		text += " failed with a request error"
	}
	if e.Class != "" {
		text += " (" + e.Class + ")"
	}
	return text
}

func (e *Error) Unwrap() error { return e.cause }

// Retryable reports whether a later call can succeed without an operator.
func (e *Error) Retryable() bool {
	return e.Class == ClassRateLimit || e.Class == ClassOutage
}

// JSON asks the chat model for one JSON object that matches schema and
// decodes it into out.
func (c *Client) JSON(ctx context.Context, name string, schema map[string]any, system, user string, out any) error {
	body := make(map[string]any, len(c.cfg.ExtraBody)+4)
	for key, value := range c.cfg.ExtraBody {
		body[key] = value
	}
	body["model"] = c.cfg.ChatModel
	body["temperature"] = 0
	body["messages"] = []map[string]string{
		{"role": "system", "content": system},
		{"role": "user", "content": user},
	}
	body["response_format"] = map[string]any{
		"type":        "json_schema",
		"json_schema": map[string]any{"name": name, "schema": schema},
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := c.post(ctx, "chat completion", "/chat/completions", body, &response); err != nil {
		return err
	}
	if len(response.Choices) == 0 {
		return c.fail(&Error{Operation: "chat completion", Class: ClassOutage, cause: errors.New("no choices")})
	}
	content := stripFence(response.Choices[0].Message.Content)
	if err := json.Unmarshal([]byte(content), out); err != nil {
		// A cut-off or malformed answer is not the provider's refusal.
		return fmt.Errorf("decode model answer (finish reason %q): %w", response.Choices[0].FinishReason, err)
	}
	return nil
}

// Embed returns one unit vector of Dimensions values per input, in input
// order.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	body := map[string]any{
		"model": c.cfg.EmbeddingModel, "input": inputs, "dimensions": c.cfg.Dimensions,
		"encoding_format": "float",
	}
	var response struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := c.post(ctx, "embedding", "/embeddings", body, &response); err != nil {
		return nil, err
	}
	if len(response.Data) != len(inputs) {
		return nil, fmt.Errorf("model provider returned %d embeddings for %d inputs", len(response.Data), len(inputs))
	}
	vectors := make([][]float32, len(inputs))
	for _, item := range response.Data {
		if item.Index < 0 || item.Index >= len(inputs) || vectors[item.Index] != nil {
			return nil, fmt.Errorf("model provider returned embedding index %d out of order", item.Index)
		}
		if len(item.Embedding) < c.cfg.Dimensions {
			return nil, fmt.Errorf("model provider returned %d embedding values, want %d", len(item.Embedding), c.cfg.Dimensions)
		}
		vector := make([]float32, c.cfg.Dimensions)
		var norm float64
		for i := range vector {
			norm += item.Embedding[i] * item.Embedding[i]
		}
		norm = math.Sqrt(norm)
		if norm == 0 {
			norm = 1
		}
		for i := range vector {
			vector[i] = float32(item.Embedding[i] / norm)
		}
		vectors[item.Index] = vector
	}
	return vectors, nil
}

func (c *Client) post(ctx context.Context, operation, path string, body, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var last *Error
	for attempt := 1; ; attempt++ {
		last = c.send(ctx, operation, path, encoded, out)
		if last == nil {
			c.health.Store(1)
			return nil
		}
		if !last.Retryable() || attempt >= c.attempts || ctx.Err() != nil {
			return c.fail(last)
		}
		delay := c.retryBase << (attempt - 1)
		if last.RetryAfter > 0 {
			delay = min(last.RetryAfter, 30*time.Second)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return c.fail(last)
		case <-timer.C:
		}
	}
}

func (c *Client) fail(err *Error) error {
	c.health.Store(2)
	return err
}

func (c *Client) send(ctx context.Context, operation, path string, body []byte, out any) *Error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return &Error{Operation: operation, cause: err}
	}
	request.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		class := ClassOutage
		if ctx.Err() != nil {
			class = ""
		}
		return &Error{Operation: operation, Class: class, cause: err}
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return &Error{Operation: operation, Class: ClassOutage, cause: err}
	}
	if response.StatusCode >= 300 {
		return &Error{
			Operation: operation, Status: response.StatusCode,
			Class:      refusalClass(response.StatusCode, payload),
			RetryAfter: retryAfter(response.Header.Get("Retry-After")),
		}
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return &Error{Operation: operation, Status: response.StatusCode, Class: ClassOutage, cause: err}
	}
	return nil
}

func refusalClass(status int, payload []byte) string {
	switch {
	case status == http.StatusPaymentRequired:
		return ClassBilling
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ClassAuth
	case status == http.StatusTooManyRequests:
		var body struct {
			Error struct {
				Code any    `json:"code"`
				Type string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(payload, &body)
		if fmt.Sprint(body.Error.Code) == "insufficient_quota" || body.Error.Type == "credit_balance_exhausted" {
			return ClassBilling
		}
		return ClassRateLimit
	case status >= 500:
		return ClassOutage
	default:
		return ""
	}
}

func retryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// stripFence removes a Markdown code fence some models put around JSON.
func stripFence(content string) string {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "```") {
		return content
	}
	content = strings.TrimPrefix(content, "```")
	if newline := strings.IndexByte(content, '\n'); newline >= 0 {
		content = content[newline+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(content), "```"))
}
