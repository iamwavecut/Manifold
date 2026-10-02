package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrainAsyncSubmitUsesV22DocumentContract(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/ingest/document" {
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
		if body["mode"] != "async" || body["storeContent"] != true || body["indexers"] != "general" {
			t.Errorf("async ingest body = %#v", body)
		}
		if body["originUri"] != "manifold://documents/mira-notes@r3" ||
			contextRef["vertical"] != "manifold" || contextRef["recorder"] != "manifold" ||
			meta["manifold_id"] != "mira-notes" || meta["revision"] != "r3" {
			t.Errorf("source identity body = %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"documentId":   "source_document:brain-doc-1",
			"deduplicated": false,
			"chunkCount":   1,
			"mode":         "async",
			"runs":         []any{map[string]any{"packId": "_general", "status": "enqueued"}},
		})
	}))
	defer server.Close()

	client := NewBrain(server.URL, "brain-secret", server.Client(), server.Client())
	documentID, err := client.SubmitDocument(t.Context(), GraphDocument{
		ID: "mira-notes", Title: "Mira notes", Format: "markdown", Content: "Mira Chen works at Meridian Labs.",
		OriginURI: "viking://resources/manifold/mira-notes.md", Revision: "r3",
		OccurredAt: "2026-09-16T10:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if documentID != "source_document:brain-doc-1" {
		t.Fatalf("Brain document ID = %q", documentID)
	}
}

func TestBrainDocumentResultMaterializesCommittedGraphAndSourceRevision(t *testing.T) {
	t.Parallel()

	const brainDocumentID = "source_document:brain-doc-1"
	const validFrom = "2026-09-16T10:00:00.000Z"
	const validUntil = "2027-09-16T10:00:00.000Z"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/documents/" + brainDocumentID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": brainDocumentID, "status": "committed",
				"meta": map[string]any{"manifold_id": "mira-notes", "revision": "r3"},
				"runs": []any{map[string]any{
					"runId": "indexer_run:run-1", "packId": "_general", "packVersion": "0", "status": "succeeded",
				}},
			})
		case "/v1/documents/" + brainDocumentID + "/candidates":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"documentId": brainDocumentID,
				"candidates": []any{
					candidate("fact", "candidate:fact", "knowledge_fact:fact-1", map[string]any{
						"entityIndex": 0, "predicate": "works_at", "object": "Meridian Labs",
					}),
					candidate("fact", "candidate:redacted-fact", "knowledge_fact:redacted", map[string]any{
						"entityIndex": 0, "predicate": "sensitive_fact", "redacted": true,
					}),
					candidate("relation", "candidate:relation", "knowledge_edge:edge-1", map[string]any{
						"fromEntityIndex": 0, "toEntityIndex": 1, "kind": "works_at",
					}),
					candidate("relation", "candidate:relation-duplicate", "knowledge_edge:edge-1", map[string]any{
						"fromEntityIndex": 0, "toEntityIndex": 1, "kind": "works_at",
					}),
					candidate("entity", "candidate:person", "knowledge_entity:person-1", map[string]any{
						"entityIndex": 0, "name": "Mira Chen", "type": "staff", "canonical": "Mira Chen",
					}),
					candidate("entity", "candidate:project", "knowledge_entity:project-1", map[string]any{
						"entityIndex": 1, "name": "Meridian Labs", "type": "project", "canonical": "Meridian Labs",
					}),
				},
			})
		case "/v1/entities/knowledge_entity:person-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"entityId": "knowledge_entity:person-1", "type": "staff", "canonicalName": "Mira Chen",
				"facts": []any{map[string]any{"factId": "knowledge_fact:fact-1", "validUntil": validUntil}},
			})
		case "/v1/entities/knowledge_entity:project-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"entityId": "knowledge_entity:project-1", "type": "project", "canonicalName": "Meridian Labs",
				"facts": []any{},
			})
		case "/v1/facts/knowledge_fact:fact-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"factId": "knowledge_fact:fact-1", "aspect": "works_at", "statement": "Meridian Labs",
				"confidence": 0.97, "validFrom": validFrom, "retracted": false,
			})
		case "/v1/entities/knowledge_entity:person-1/connections",
			"/v1/entities/knowledge_entity:project-1/connections":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"edges": []any{map[string]any{
					"edgeId": "knowledge_edge:edge-1",
					"from":   "knowledge_entity:person-1", "to": "knowledge_entity:project-1",
					"kind": "works_at", "weight": 0.93,
					// A committed candidate can confirm an existing edge whose
					// first source is an earlier document revision.
					"source": map[string]any{"documentId": "source_document:earlier-revision"},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	state, err := NewBrain(server.URL, "brain-secret", server.Client(), server.Client()).DocumentResult(t.Context(), brainDocumentID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != GraphDocumentReady || state.BrainStatus != "committed" {
		t.Fatalf("document state = %#v", state)
	}
	if state.Result.DocumentID != brainDocumentID || state.Result.OriginURI != "manifold://documents/mira-notes@r3" ||
		state.Result.Revision != "r3" {
		t.Fatalf("source identity = %#v", state.Result)
	}
	if len(state.Result.Entities) != 2 || len(state.Result.Facts) != 1 || len(state.Result.Relations) != 1 {
		t.Fatalf("materialized graph = %#v", state.Result)
	}
	if state.Result.Entities[0].BrainID != "knowledge_entity:person-1" || state.Result.Entities[0].Name != "Mira Chen" ||
		state.Result.Entities[0].Type != "staff" {
		t.Fatalf("person entity = %#v", state.Result.Entities[0])
	}
	fact := state.Result.Facts[0]
	if fact.BrainID != "knowledge_fact:fact-1" || fact.EntityBrainID != "knowledge_entity:person-1" ||
		fact.Predicate != "works_at" || fact.Object != "Meridian Labs" || fact.Status != "active" ||
		fact.Confidence != 0.97 || fact.SourceDocumentID != brainDocumentID ||
		fact.ValidFrom.UTC().Format("2006-01-02T15:04:05.000Z") != validFrom {
		t.Fatalf("fact = %#v", fact)
	}
	if fact.ValidUntil == nil || fact.ValidUntil.UTC().Format("2006-01-02T15:04:05.000Z") != validUntil {
		t.Fatalf("fact validUntil = %#v, want %s", fact.ValidUntil, validUntil)
	}
	relation := state.Result.Relations[0]
	if relation.BrainID != "knowledge_edge:edge-1" || relation.FromEntityBrainID != "knowledge_entity:person-1" ||
		relation.ToEntityBrainID != "knowledge_entity:project-1" || relation.Predicate != "works_at" ||
		relation.Status != "active" || relation.SourceDocumentID != brainDocumentID {
		t.Fatalf("relation = %#v", relation)
	}
	if len(state.Result.EntityIDs) != 2 || len(state.Result.FactIDs) != 1 || len(state.Result.EdgeIDs) != 1 {
		t.Fatalf("upstream ID sets = %#v", state.Result)
	}
}

func TestBrainMaterializeCandidatesPreservesProfileFactStatusAndRetraction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		profileStatus string
		retracted     bool
		wantStatus    string
	}{
		{name: "competing", profileStatus: "competing", wantStatus: "competing"},
		{name: "superseded", profileStatus: "superseded", wantStatus: "superseded"},
		{name: "retraction overrides profile status", profileStatus: "competing", retracted: true, wantStatus: "retracted"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/entities/knowledge_entity:person-1":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"entityId": "knowledge_entity:person-1", "type": "staff", "canonicalName": "Mira Chen",
						"facts": []any{map[string]any{
							"factId": "knowledge_fact:fact-1", "status": test.profileStatus,
						}},
					})
				case "/v1/facts/knowledge_fact:fact-1":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"factId": "knowledge_fact:fact-1", "aspect": "works_at", "statement": "Meridian Labs",
						"confidence": 0.97, "validFrom": "2026-09-16T10:00:00.000Z", "retracted": test.retracted,
					})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			candidates := []brainCandidate{
				{
					Kind: "entity", Status: "committed", CommitRef: "knowledge_entity:person-1",
					Payload: map[string]any{"entityIndex": 0},
				},
				{
					Kind: "fact", Status: "committed", CommitRef: "knowledge_fact:fact-1",
					Payload: map[string]any{"entityIndex": 0},
				},
			}
			result, err := NewBrain(server.URL, "brain-secret", server.Client(), server.Client()).materializeCandidates(
				t.Context(), "source_document:brain-doc-1", "manifold://documents/mira-notes@r1", "r1", candidates,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Facts) != 1 || result.Facts[0].Status != test.wantStatus {
				t.Fatalf("facts = %#v, want status %q", result.Facts, test.wantStatus)
			}
		})
	}
}

func TestBrainDocumentResultDistinguishesPendingEmptyAndFailedRuns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		docStatus  string
		runStatus  string
		candidates []any
		want       GraphDocumentStatus
	}{
		{name: "in progress", docStatus: "indexing", runStatus: "running", candidates: []any{pendingCandidate()}, want: GraphDocumentPending},
		{name: "successful empty extraction", docStatus: "indexing", runStatus: "succeeded", want: GraphDocumentReady},
		{name: "intentional skip", docStatus: "indexing", runStatus: "skipped", want: GraphDocumentReady},
		{name: "all work failed without graph", docStatus: "indexing", runStatus: "failed", want: GraphDocumentFailed},
		{name: "failed document without ledger", docStatus: "failed", want: GraphDocumentFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := documentStateServer(t, test.docStatus, test.runStatus, test.candidates)
			defer server.Close()
			state, err := NewBrain(server.URL, "brain-secret", server.Client(), server.Client()).DocumentResult(
				t.Context(), "source_document:brain-doc-1",
			)
			if err != nil {
				t.Fatal(err)
			}
			if state.Status != test.want {
				t.Fatalf("status = %q, want %q (state %#v)", state.Status, test.want, state)
			}
		})
	}
}

func TestBrainDocumentResultMarksFailedRunWithGraphPartial(t *testing.T) {
	t.Parallel()

	candidates := []any{candidate("entity", "candidate:person", "knowledge_entity:person-1", map[string]any{
		"entityIndex": 0, "name": "Mira Chen", "type": "staff",
	})}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/documents/source_document:brain-doc-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "source_document:brain-doc-1", "status": "indexing",
				"meta": map[string]any{"manifold_id": "mira-notes", "revision": "r1"},
				"runs": []any{map[string]any{"packId": "_general", "status": "failed"}},
			})
		case "/v1/documents/source_document:brain-doc-1/candidates":
			_ = json.NewEncoder(w).Encode(map[string]any{"candidates": candidates})
		case "/v1/entities/knowledge_entity:person-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"entityId": "knowledge_entity:person-1", "type": "staff", "canonicalName": "Mira Chen",
				"facts": []any{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	state, err := NewBrain(server.URL, "brain-secret", server.Client(), server.Client()).DocumentResult(
		t.Context(), "source_document:brain-doc-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != GraphDocumentPartial || len(state.Result.Entities) != 1 {
		t.Fatalf("partial state = %#v", state)
	}
}

func TestBrainSearchDoesNotUseSourceKeyAsDocumentReference(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"entityId":"entity:1","entityType":"staff","canonicalName":"Mira Chen","score":0.9,"facts":[{"factId":"fact:1","predicate":"works_at","object":"Meridian Labs","score":0.8,"sourceKey":"manifold:_general"}]}]}`))
	}))
	defer server.Close()

	hits, err := NewBrain(server.URL, "brain-secret", server.Client(), server.Client()).Search(t.Context(), "Mira", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].UpstreamSourceRef != "" {
		t.Fatalf("search hits = %#v; sourceKey must not be used as a document ref", hits)
	}
}

func TestBrainRetractIncludesRequiredSystemActor(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/facts/knowledge_fact:fact-1/retract" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		retractedBy, _ := body["retractedBy"].(map[string]any)
		if body["reason"] != "superseded" || retractedBy["source"] != "system" {
			t.Errorf("retraction body = %#v", body)
		}
		_, _ = w.Write([]byte(`{"factId":"knowledge_fact:fact-1"}`))
	}))
	defer server.Close()

	if err := NewBrain(server.URL, "brain-secret", server.Client(), server.Client()).RetractFact(
		t.Context(), "knowledge_fact:fact-1", "superseded",
	); err != nil {
		t.Fatal(err)
	}
}

func TestBrainRetryDocumentUsesWriteScopedRetryRoute(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/documents/source_document:brain-doc-1/retry" {
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
		if body["retryKey"] != "job-0123456789abcdefghij-attempt-2" {
			t.Errorf("retry body = %#v", body)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := NewBrain(server.URL, "brain-secret", server.Client(), server.Client())
	if err := client.RetryDocument(t.Context(), "source_document:brain-doc-1", "job-0123456789abcdefghij-attempt-2"); err != nil {
		t.Fatal(err)
	}
}

func TestBrainRateLimitRetryIsScopedToExtractionAndRedactsInternalIDs(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/documents/source_document:private-id" {
			http.NotFound(w, r)
			return
		}
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := NewBrain(server.URL, "brain-secret", server.Client(), server.Client())
	var response map[string]bool
	if err := client.http.doWithRateLimitRetry(
		t.Context(), http.MethodGet, "/v1/documents/source_document:private-id", nil, &response,
	); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 || !response["ok"] {
		t.Fatalf("attempts=%d response=%#v", attempts.Load(), response)
	}

	var ordinaryAttempts atomic.Int32
	ordinary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ordinaryAttempts.Add(1)
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ordinary.Close()
	ordinaryClient := NewBrain(ordinary.URL, "brain-secret", ordinary.Client(), ordinary.Client())
	err := ordinaryClient.http.do(
		t.Context(), http.MethodGet, "/v1/documents/source_document:private-id", nil, nil,
	)
	var dependency *DependencyError
	if !errors.As(err, &dependency) {
		t.Fatalf("error = %T %v, want DependencyError", err, err)
	}
	if ordinaryAttempts.Load() != 1 || dependency.RetryAfter != 5*time.Second ||
		strings.Contains(dependency.Operation, "private-id") {
		t.Fatalf("attempts=%d dependency=%#v", ordinaryAttempts.Load(), dependency)
	}
}

func TestBrainRateLimitRetryStopsWhenContextExpires(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var attempts atomic.Int32
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts.Add(1)
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"60"}},
			Body:       cancelOnCloseBody{Reader: strings.NewReader(""), cancel: cancel},
			Request:    request,
		}, nil
	})
	httpClient := &http.Client{Transport: transport}
	client := NewBrain("http://brain.invalid", "brain-secret", httpClient, httpClient)
	err := client.http.doWithRateLimitRetry(ctx, http.MethodGet, "/v1/documents/source_document:private-id", nil, nil)
	var dependency *DependencyError
	if !errors.As(err, &dependency) || !dependency.Retryable || dependency.Status != http.StatusTooManyRequests ||
		dependency.RetryAfter != 60*time.Second || attempts.Load() != 1 {
		t.Fatalf("error = %#v, want retryable dependency error with Retry-After", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type cancelOnCloseBody struct {
	io.Reader
	cancel context.CancelFunc
}

func (body cancelOnCloseBody) Close() error {
	body.cancel()
	return nil
}

func candidate(kind, id, commitRef string, payload map[string]any) map[string]any {
	return map[string]any{
		"id": id, "runId": "indexer_run:run-1", "chunkSeq": 0,
		"kind": kind, "confidence": 0.9, "status": "committed", "commitRef": commitRef,
		"payload": payload,
	}
}

func pendingCandidate() map[string]any {
	return map[string]any{
		"id": "candidate:pending", "runId": "indexer_run:run-1", "chunkSeq": 0,
		"kind": "entity", "confidence": 0.5, "status": "pending",
		"payload": map[string]any{"entityIndex": 0, "name": "Mira Chen", "type": "staff"},
	}
}

func documentStateServer(t *testing.T, docStatus, runStatus string, candidates []any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/documents/source_document:brain-doc-1":
			document := map[string]any{
				"id": "source_document:brain-doc-1", "status": docStatus,
				"meta": map[string]any{"manifold_id": "mira-notes", "revision": "r1"},
			}
			if runStatus != "" {
				document["runs"] = []any{map[string]any{"packId": "_general", "status": runStatus}}
			}
			_ = json.NewEncoder(w).Encode(document)
		case "/v1/documents/source_document:brain-doc-1/candidates":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"documentId": "source_document:brain-doc-1", "candidates": candidates,
			})
		case "/v1/entities/knowledge_entity:person-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"entityId": "knowledge_entity:person-1", "type": "staff", "canonicalName": "Mira Chen",
				"facts": []any{},
			})
		default:
			if strings.Contains(r.URL.Path, "/connections") || strings.Contains(r.URL.Path, "/facts/") {
				t.Errorf("unexpected graph hydration request %s", r.URL.Path)
			}
			http.NotFound(w, r)
		}
	}))
}
