package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestProviderRefusesOnlyExtractionWhileSwitched(t *testing.T) {
	t.Cleanup(func() { refusal.Store(0) })
	call := func(schema string) int {
		request := map[string]any{
			"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": schema, "schema": map[string]any{"type": "object"}}},
		}
		encoded, _ := json.Marshal(request)
		response := httptest.NewRecorder()
		chatCompletion(response, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(encoded)))
		return response.Code
	}
	switchTo := func(body string) {
		controlRefusal(httptest.NewRecorder(), httptest.NewRequest("POST", "/control/refuse", strings.NewReader(body)))
	}

	switchTo(`{"status":402}`)
	if got := call("extraction"); got != 402 {
		t.Fatalf("extraction status = %d, want 402", got)
	}
	if got := call("summary"); got != 200 {
		t.Fatalf("other request status = %d, want 200", got)
	}
	switchTo(`{"status":0}`)
	if got := call("extraction"); got != 200 {
		t.Fatalf("extraction status after recovery = %d, want 200", got)
	}
}

func completionFor(t *testing.T, sentence, schemaName string) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"messages":        []map[string]string{{"role": "user", "content": sentence}},
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": schemaName, "schema": map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	chatCompletion(response, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(encoded)))
	return response
}

func TestProviderGroundsEveryFixturePerson(t *testing.T) {
	for sentence, person := range map[string]string{
		"Noor Patel works at Delta Labs.": "Noor Patel",
		"Ines Moreno works at Echo Labs.": "Ines Moreno",
	} {
		body := completionFor(t, sentence, "extraction").Body.String()
		if !strings.Contains(body, `\"name\":\"`+person+`\"`) {
			t.Fatalf("no entity %q for %q: %s", person, sentence, body)
		}
	}
}

func TestProviderHoldsExtractionUntilReleased(t *testing.T) {
	hold := func(value string) {
		controlHold(httptest.NewRecorder(), httptest.NewRequest("POST", "/control/hold", strings.NewReader(`{"hold":`+value+`}`)))
	}
	hold("true")
	defer hold("false")
	if response := completionFor(t, "x", "entity_judge_verdict"); response.Code != http.StatusOK {
		t.Fatalf("non-extraction request blocked: %d", response.Code)
	}
	done := make(chan int, 1)
	go func() { done <- completionFor(t, "x", "extraction").Code }()
	select {
	case code := <-done:
		t.Fatalf("held extraction answered %d", code)
	case <-time.After(200 * time.Millisecond):
	}
	hold("false")
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("released extraction status = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("released extraction did not answer")
	}
}
