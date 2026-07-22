package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenVikingAdapterMatchesPinnedV0410Contract(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "ov-secret" {
			t.Errorf("X-API-Key = %q", r.Header.Get("X-API-Key"))
		}
		if r.Header.Get("X-OpenViking-Account") != "manifold" ||
			r.Header.Get("X-OpenViking-User") != "manifold" {
			t.Errorf("trusted identity headers = %q/%q",
				r.Header.Get("X-OpenViking-Account"), r.Header.Get("X-OpenViking-User"))
		}
		switch r.URL.Path {
		case "/api/v1/content/write":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			if body["uri"] != "viking://resources/manifold/agent-memory.md" ||
				body["mode"] != "create" || body["wait"] != true {
				t.Errorf("write body = %#v", body)
			}
			_, _ = w.Write([]byte(`{"status":"ok","result":{"uri":"viking://resources/manifold/agent-memory.md"}}`))
		case "/api/v1/snapshot/commit":
			_, _ = w.Write([]byte(`{"status":"ok","result":{"commit_oid":"snapshot-1"}}`))
		case "/api/v1/search/find":
			_, _ = w.Write([]byte(`{"status":"ok","result":{"resources":[{"uri":"viking://resources/manifold/agent-memory.md","score":0.92,"overview":"Canonical evidence"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewOpenViking(server.URL, "ov-secret", server.Client())
	if err := client.Write(t.Context(), "viking://resources/manifold/agent-memory.md", "content", true); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Snapshot(t.Context(), "initial", []string{"viking://resources/manifold/agent-memory.md"})
	if err != nil || snapshot != "snapshot-1" {
		t.Fatalf("snapshot = %q, err = %v", snapshot, err)
	}
	hits, err := client.Search(t.Context(), "evidence", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Source != "openviking" || hits[0].Score != 0.92 {
		t.Fatalf("search hits = %#v", hits)
	}
}

func TestBrainAdapterMatchesPinnedV081DocumentContract(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ingest/document" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer brain-secret" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		contextRef, _ := body["contextRef"].(map[string]any)
		meta, _ := body["meta"].(map[string]any)
		if body["mode"] != "sync" || body["storeContent"] != false ||
			contextRef["vertical"] != "manifold" || meta["manifold_id"] != "agent-memory" {
			t.Errorf("ingest body = %#v", body)
		}
		_, _ = w.Write([]byte(`{
			"documentId":"brain-document-1",
			"committed":{"entityIds":["entity-1"],"factIds":["fact-1"],"edgeIds":["edge-1"]}
		}`))
	}))
	defer server.Close()

	client := NewBrain(server.URL, "brain-secret", server.Client(), server.Client())
	result, err := client.IngestDocument(t.Context(), GraphDocument{
		ID: "agent-memory", Title: "Agent memory", Format: "markdown", Content: "content",
		OriginURI: "viking://resources/manifold/agent-memory.md", Revision: "r1",
		OccurredAt: "2026-07-17T10:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.DocumentID != "brain-document-1" ||
		len(result.EntityIDs) != 1 || len(result.FactIDs) != 1 || len(result.EdgeIDs) != 1 {
		t.Fatalf("ingest result = %#v", result)
	}
}

func TestBrainSearchClampsCandidateLimitToPinnedContract(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/search" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["limit"] != float64(brainSearchMaxLimit) {
			t.Errorf("limit = %#v, want %d", body["limit"], brainSearchMaxLimit)
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	client := NewBrain(server.URL, "brain-secret", server.Client(), server.Client())
	if _, err := client.Search(t.Context(), "release evidence", 200, false); err != nil {
		t.Fatal(err)
	}
}

func TestBrainDocumentIngestUsesDedicatedLongRunningClient(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ingest/document" {
			http.NotFound(w, r)
			return
		}
		time.Sleep(30 * time.Millisecond)
		_, _ = w.Write([]byte(`{"documentId":"brain-document-1","committed":{}}`))
	}))
	defer server.Close()

	ordinaryClient := &http.Client{Timeout: 5 * time.Millisecond}
	ingestClient := &http.Client{Timeout: 200 * time.Millisecond}
	client := NewBrain(server.URL, "brain-secret", ordinaryClient, ingestClient)
	result, err := client.IngestDocument(t.Context(), GraphDocument{
		ID: "release-evidence", Title: "Release evidence", Format: "markdown", Content: "verified",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.DocumentID != "brain-document-1" {
		t.Fatalf("document ID = %q", result.DocumentID)
	}
}

func TestDependencyErrorsClassifyRetrySafetyAndOmitProviderBodies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		retryable bool
	}{
		{name: "caller error", status: http.StatusBadRequest, retryable: false},
		{name: "rate limited", status: http.StatusTooManyRequests, retryable: true},
		{name: "provider failed", status: http.StatusBadGateway, retryable: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(`{"secret":"must-not-escape","internal":"http://brain:3000"}`))
			}))
			defer server.Close()

			err := NewBrain(server.URL, "key", server.Client(), server.Client()).Health(t.Context())
			var dependency *DependencyError
			if !errors.As(err, &dependency) {
				t.Fatalf("error = %T %v, want DependencyError", err, err)
			}
			if dependency.Retryable != test.retryable {
				t.Fatalf("retryable = %v, want %v", dependency.Retryable, test.retryable)
			}
			if strings.Contains(err.Error(), "must-not-escape") || strings.Contains(err.Error(), "brain:3000") {
				t.Fatalf("error exposed provider body: %v", err)
			}
		})
	}
}

func TestProviderTimeoutIsRetryable(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}))
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Millisecond}
	err := NewBrain(server.URL, "key", client, client).Health(context.Background())
	var dependency *DependencyError
	if !errors.As(err, &dependency) || !dependency.Retryable {
		t.Fatalf("timeout error = %#v, want retryable DependencyError", err)
	}
}
