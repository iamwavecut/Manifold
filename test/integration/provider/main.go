package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

func main() {
	// The runtime image has no shell or HTTP client for a healthcheck or for
	// the integration script's switches.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := probe("http://127.0.0.1:8081/health"); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "control" {
		if err := control("http://127.0.0.1:8081", os.Args[2], os.Args[3]); err != nil {
			log.Fatal(err)
		}
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "deterministic-chat", "object": "model"},
				{"id": "deterministic-embedding", "object": "model"},
			},
		})
	})
	mux.HandleFunc("/v1/embeddings", embeddings)
	mux.HandleFunc("/v1/chat/completions", chatCompletion)
	mux.HandleFunc("/control/refuse", controlRefusal)
	mux.HandleFunc("/control/hold", controlHold)
	server := &http.Server{
		Addr:              ":8081",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// A held extraction answers only after its release.
		WriteTimeout: 10 * time.Minute,
	}
	log.Printf("deterministic provider listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func probe(url string) error {
	client := http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("provider health: HTTP %d", response.StatusCode)
	}
	return nil
}

// control switches a running provider: "refuse <status>" (0 accepts again)
// or "hold true|false".
func control(base, action, value string) error {
	var body string
	switch action {
	case "refuse":
		status, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("refuse needs an HTTP status: %w", err)
		}
		body = fmt.Sprintf(`{"status":%d}`, status)
	case "hold":
		if value != "true" && value != "false" {
			return fmt.Errorf("hold needs true or false, got %q", value)
		}
		body = `{"hold":` + value + `}`
	default:
		return fmt.Errorf("unknown control %q", action)
	}
	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Post(base+"/control/"+action, "application/json", strings.NewReader(body))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("control %s: HTTP %d", action, response.StatusCode)
	}
	return nil
}

// refusal is the HTTP status extraction requests are refused with; zero
// accepts them. The integration run switches it to act out a provider account
// without credit. Other requests keep working so only extraction is affected.
var refusal atomic.Int32

func controlRefusal(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Status int32 `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, `{"error":{"message":"invalid JSON"}}`, http.StatusBadRequest)
		return
	}
	refusal.Store(request.Status)
	writeJSON(w, request)
}

// hold, when set, keeps extraction requests unanswered until the release
// closes it. The integration run uses it to keep one extraction in flight.
var (
	holdMu sync.Mutex
	hold   chan struct{}
)

func controlHold(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Hold bool `json:"hold"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, `{"error":{"message":"invalid JSON"}}`, http.StatusBadRequest)
		return
	}
	holdMu.Lock()
	defer holdMu.Unlock()
	switch {
	case request.Hold && hold == nil:
		hold = make(chan struct{})
	case !request.Hold && hold != nil:
		close(hold)
		hold = nil
	}
	writeJSON(w, request)
}

func waitWhileHeld(ctx context.Context) {
	holdMu.Lock()
	held := hold
	holdMu.Unlock()
	if held == nil {
		return
	}
	select {
	case <-held:
	case <-ctx.Done():
	}
}

func embeddings(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Input      any `json:"input"`
		Dimensions int `json:"dimensions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, `{"error":{"message":"invalid JSON"}}`, http.StatusBadRequest)
		return
	}
	dimensions := request.Dimensions
	if dimensions <= 0 {
		dimensions, _ = strconv.Atoi(os.Getenv("EMBEDDING_DIMENSIONS"))
	}
	if dimensions <= 0 {
		dimensions = 32
	}
	var inputs []string
	switch input := request.Input.(type) {
	case string:
		inputs = []string{input}
	case []any:
		for _, value := range input {
			inputs = append(inputs, fmt.Sprint(value))
		}
	}
	data := make([]map[string]any, len(inputs))
	for index, input := range inputs {
		data[index] = map[string]any{
			"object": "embedding", "index": index, "embedding": embed(input, dimensions),
		}
	}
	writeJSON(w, map[string]any{
		"object": "list",
		"model":  "deterministic-embedding",
		"data":   data,
		"usage":  map[string]int{"prompt_tokens": 1, "total_tokens": 1},
	})
}

// embed hashes the words of text into a unit vector, so equal texts get equal
// vectors and texts that share words lie close together.
func embed(text string, dimensions int) []float64 {
	vector := make([]float64, dimensions)
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		hash := fnv.New32a()
		hash.Write([]byte(word))
		vector[hash.Sum32()%uint32(dimensions)]++
	}
	var norm float64
	for _, value := range vector {
		norm += value * value
	}
	if norm == 0 {
		vector[0], norm = 1, 1
	}
	for index := range vector {
		vector[index] /= math.Sqrt(norm)
	}
	return vector
}

func chatCompletion(w http.ResponseWriter, r *http.Request) {
	var request map[string]any
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, `{"error":{"message":"invalid JSON"}}`, http.StatusBadRequest)
		return
	}
	content := any(map[string]any{})
	if responseFormat, ok := request["response_format"].(map[string]any); ok {
		if schemaWrapper, ok := responseFormat["json_schema"].(map[string]any); ok {
			if schemaWrapper["name"] == "extraction" {
				waitWhileHeld(r.Context())
			}
			if status := int(refusal.Load()); status != 0 && schemaWrapper["name"] == "extraction" {
				http.Error(w, `{"error":{"message":"insufficient credits","code":402}}`, status)
				return
			}
			if schema, ok := schemaWrapper["schema"].(map[string]any); ok {
				content = valueForSchema(schema)
			}
			if fixture, ok := extractionFixture(request, schemaWrapper); ok {
				content = fixture
			}
		}
	}
	encoded, _ := json.Marshal(content)
	writeJSON(w, map[string]any{
		"id":      "chatcmpl-deterministic",
		"object":  "chat.completion",
		"created": 1,
		"model":   "deterministic-chat",
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role": "assistant", "content": string(encoded),
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2,
		},
	})
}

// employments are the sentences the provider extracts as grounded facts. The
// convergence phases use their own people, so their facts never conflict.
var employments = [][2]string{
	{"Mira Chen", "Meridian Labs"},
	{"Mira Chen", "Borealis Labs"},
	{"Mira Chen", "Cobalt Labs"},
	{"Noor Patel", "Delta Labs"},
	{"Ines Moreno", "Echo Labs"},
}

func extractionFixture(request, schema map[string]any) (any, bool) {
	if schema["name"] != "extraction" {
		return nil, false
	}
	messages, _ := request["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		text, _ := message["content"].(string)
		if message["role"] != "user" {
			continue
		}
		for _, employment := range employments {
			person, company := employment[0], employment[1]
			clause := person + " works at " + company + "."
			if !strings.Contains(text, clause) {
				continue
			}
			return map[string]any{
				"entities": []map[string]any{
					{"name": person, "type": "staff", "aliases": []string{}},
					{"name": company, "type": "organization", "aliases": []string{}},
				},
				"facts": []map[string]any{{"entity": 0, "predicate": "works_at", "value": company,
					"valid_from": "", "confidence": 1, "evidence": clause}},
				"relations": []map[string]any{{"from": 0, "to": 1, "predicate": "works_at", "confidence": 1}},
			}, true
		}
	}
	return nil, false
}

func valueForSchema(schema map[string]any) any {
	if constant, ok := schema["const"]; ok {
		return constant
	}
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 {
		return values[0]
	}
	schemaType, _ := schema["type"].(string)
	if schemaType == "" {
		if alternatives, ok := schema["anyOf"].([]any); ok && len(alternatives) > 0 {
			if selected, ok := alternatives[0].(map[string]any); ok {
				return valueForSchema(selected)
			}
		}
	}
	switch schemaType {
	case "object", "":
		result := map[string]any{}
		required := map[string]struct{}{}
		if values, ok := schema["required"].([]any); ok {
			for _, value := range values {
				required[fmt.Sprint(value)] = struct{}{}
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		for name, raw := range properties {
			if _, needed := required[name]; !needed {
				continue
			}
			property, _ := raw.(map[string]any)
			result[name] = valueForSchema(property)
		}
		return result
	case "array":
		minItems := 0
		if value, ok := schema["minItems"].(float64); ok {
			minItems = int(value)
		}
		items, _ := schema["items"].(map[string]any)
		result := make([]any, minItems)
		for index := range result {
			result[index] = valueForSchema(items)
		}
		return result
	case "string":
		return "deterministic"
	case "integer":
		return 0
	case "number":
		return 0.5
	case "boolean":
		return false
	default:
		return nil
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}
