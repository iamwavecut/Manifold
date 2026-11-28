package llm

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc, configure ...func(*Config)) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := Config{
		BaseURL: server.URL + "/v1", APIKey: "test-key", ChatModel: "chat-model",
		EmbeddingModel: "embed-model", Dimensions: 4,
		ExtraBody: map[string]any{"provider": map[string]any{"require_parameters": true}, "temperature": 0.7},
	}
	for _, apply := range configure {
		apply(&cfg)
	}
	client := New(cfg)
	client.retryBase = time.Millisecond
	return client
}

func TestJSONSendsSchemaRequestAndDecodesContent(t *testing.T) {
	var body map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected request %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"choices":[{"message":{"content":"` + "```json\\n{\\\"answer\\\":42}\\n```" + `"}}]}`))
	})
	var out struct {
		Answer int `json:"answer"`
	}
	schema := map[string]any{"type": "object"}
	if err := client.JSON(context.Background(), "extraction", schema, "system text", "user text", &out); err != nil {
		t.Fatal(err)
	}
	if out.Answer != 42 {
		t.Fatalf("answer = %d", out.Answer)
	}
	if body["model"] != "chat-model" || body["temperature"] != float64(0) {
		t.Fatalf("model or temperature not set by the call: %v", body)
	}
	if _, ok := body["provider"].(map[string]any); !ok {
		t.Fatalf("extra body not merged: %v", body)
	}
	format, _ := body["response_format"].(map[string]any)
	wrapper, _ := format["json_schema"].(map[string]any)
	if format["type"] != "json_schema" || wrapper["name"] != "extraction" {
		t.Fatalf("response_format = %v", body["response_format"])
	}
	messages, _ := body["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %v", messages)
	}
	if client.Health() != "ready" {
		t.Fatalf("health after success = %q", client.Health())
	}
}

func TestEmbedTruncatesAndNormalizesInInputOrder(t *testing.T) {
	var body map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		// Out of order and longer than the requested size, as some proxies answer.
		w.Write([]byte(`{"data":[
			{"index":1,"embedding":[0,0,3,4,9,9]},
			{"index":0,"embedding":[2,0,0,0,7,7]}]}`))
	})
	vectors, err := client.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatal(err)
	}
	if body["model"] != "embed-model" || body["dimensions"] != float64(4) {
		t.Fatalf("request = %v", body)
	}
	want := [][]float32{{1, 0, 0, 0}, {0, 0, 0.6, 0.8}}
	for i := range want {
		for j := range want[i] {
			if math.Abs(float64(vectors[i][j]-want[i][j])) > 1e-6 {
				t.Fatalf("vectors = %v, want %v", vectors, want)
			}
		}
	}
}

func TestEmbedRejectsShortVectors(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2]}]}`))
	})
	if _, err := client.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("accepted a vector shorter than the configured size")
	}
}

func TestProviderRefusalClasses(t *testing.T) {
	cases := []struct {
		status   int
		body     string
		class    string
		attempts int32
	}{
		{402, `{"error":{"message":"no credit"}}`, ClassBilling, 1},
		{429, `{"error":{"code":"insufficient_quota"}}`, ClassBilling, 1},
		{429, `{"error":{"type":"credit_balance_exhausted"}}`, ClassBilling, 1},
		{401, `{}`, ClassAuth, 1},
		{403, `{}`, ClassAuth, 1},
		{429, `{}`, ClassRateLimit, 3},
		{503, `{}`, ClassOutage, 3},
		{400, `{}`, "", 1},
	}
	for _, tc := range cases {
		var calls atomic.Int32
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body))
		})
		err := client.JSON(context.Background(), "extraction", map[string]any{}, "s", "u", &struct{}{})
		var providerErr *Error
		if !errors.As(err, &providerErr) {
			t.Fatalf("HTTP %d: error %v is not *Error", tc.status, err)
		}
		if providerErr.Class != tc.class || providerErr.Status != tc.status {
			t.Fatalf("HTTP %d %s: class %q status %d, want %q", tc.status, tc.body, providerErr.Class, providerErr.Status, tc.class)
		}
		if got := calls.Load(); got != tc.attempts {
			t.Fatalf("HTTP %d %s: %d attempts, want %d", tc.status, tc.body, got, tc.attempts)
		}
		if strings.Contains(err.Error(), "test-key") {
			t.Fatal("error text contains the API key")
		}
		if client.Health() != "degraded" {
			t.Fatalf("health after failure = %q", client.Health())
		}
	}
}

func TestRetryRecoversAndHonoursRetryAfter(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0,0]}]}`))
	})
	if _, err := client.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestConfiguredReportsMissingSettings(t *testing.T) {
	if New(Config{BaseURL: "http://x", ChatModel: "m"}).Configured() {
		t.Fatal("client without an API key reports configured")
	}
	if !New(Config{BaseURL: "http://x", APIKey: "k", ChatModel: "m", EmbeddingModel: "e", Dimensions: 8}).Configured() {
		t.Fatal("complete client reports unconfigured")
	}
}
