package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
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
	server := &http.Server{
		Addr:              ":8081",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	log.Printf("deterministic provider listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
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
	count := 1
	if values, ok := request.Input.([]any); ok {
		count = len(values)
	}
	data := make([]map[string]any, count)
	for index := range data {
		vector := make([]float64, dimensions)
		for dimension := range vector {
			vector[dimension] = float64((index+1)*(dimension%7+1)) / 100
		}
		data[index] = map[string]any{
			"object": "embedding", "index": index, "embedding": vector,
		}
	}
	writeJSON(w, map[string]any{
		"object": "list",
		"model":  "deterministic-embedding",
		"data":   data,
		"usage":  map[string]int{"prompt_tokens": 1, "total_tokens": 1},
	})
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
			if schema, ok := schemaWrapper["schema"].(map[string]any); ok {
				content = valueForSchema(schema)
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
