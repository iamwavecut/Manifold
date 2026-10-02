package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestProviderReturnsGroundedExtraction(t *testing.T) {
	const sentence = "Mira Chen works at Meridian Labs."
	request := map[string]any{
		"messages":        []map[string]string{{"role": "user", "content": sentence}},
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "extraction", "schema": map[string]any{"type": "object"}}},
	}
	encoded, _ := json.Marshal(request)
	response := httptest.NewRecorder()
	chatCompletion(response, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(encoded)))
	var completion struct {
		Choices []struct{ Message struct{ Content string } }
	}
	if err := json.Unmarshal(response.Body.Bytes(), &completion); err != nil {
		t.Fatal(err)
	}
	var extraction struct {
		Entities []map[string]any
		Facts    []map[string]any
		Edges    []map[string]any
	}
	if err := json.Unmarshal([]byte(completion.Choices[0].Message.Content), &extraction); err != nil {
		t.Fatal(err)
	}
	if len(extraction.Entities) != 2 || len(extraction.Facts) != 1 || len(extraction.Edges) != 1 {
		t.Fatalf("empty or incomplete fixture: %+v", extraction)
	}
	if extraction.Facts[0]["valueSpan"] != "Meridian Labs" {
		t.Fatalf("ungrounded fact: %+v", extraction.Facts)
	}
}
