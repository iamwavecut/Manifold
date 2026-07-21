package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProblemRenderingUsesCodeAndRemediation(t *testing.T) {
	var p problem
	p.Code = "slug_taken"
	p.Title = "Slug already in use"
	p.RequestID = "d5vqh9idc6b5u5sctq00"
	p.SuggestedSlug = "agent-memory-2"
	p.Remediation.Summary = "Choose an unused slug."
	p.Remediation.Steps = []string{"Review suggested_slug."}
	var output bytes.Buffer
	renderProblem(&output, p)
	for _, expected := range []string{
		"ERROR [slug_taken]", "How to fix: Choose an unused slug.",
		"Suggested slug: agent-memory-2", "Request ID: d5vqh9idc6b5u5sctq00",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("output lacks %q: %s", expected, output.String())
		}
	}
	if exitCode(p.Code) != 14 {
		t.Fatalf("slug_taken exit code = %d", exitCode(p.Code))
	}
	if exitCode("idempotency_key_reused") != 15 {
		t.Fatalf("idempotency_key_reused exit code = %d", exitCode("idempotency_key_reused"))
	}
	if exitCode("api_key_secret_not_replayable") != 20 {
		t.Fatalf("api_key_secret_not_replayable exit code = %d", exitCode("api_key_secret_not_replayable"))
	}
}

func TestIdempotencyKeyIsNotAnXID(t *testing.T) {
	key := newIdempotencyKey()
	if !strings.HasPrefix(key, "skill-") || len(key) < 25 {
		t.Fatalf("unexpected idempotency key %q", key)
	}
}

func TestRememberRequiresReadingCandidatesBeforeChoosingCreateOrUpdate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/search":
			_, _ = w.Write([]byte(`{"items":[{"kind":"document","id":"memory-policy","path":"shared/agent-practice/memory-policy","title":"Memory policy","revision":"r3","canonical_ref":"manifold://documents/memory-policy@r3","snippet":"Search before creating."}]}`))
		case r.URL.Path == "/api/v1/documents/new-policy":
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"resource_not_found","title":"Not found","detail":"missing","request_id":"request"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &client{baseURL: server.URL, apiKey: "test", http: server.Client()}

	_, err := executeRemember(c, []string{
		"--id", "new-policy", "--title", "New policy", "--content", "Search before creating.",
		"--folder-path", "shared/agent-practice",
	})
	var semantic *apiError
	if !errors.As(err, &semantic) || semantic.Body.Code != "memory_candidates_found" {
		t.Fatalf("remember error = %#v", err)
	}
	if len(semantic.Body.Candidates) != 1 || semantic.Body.Candidates[0].ID != "memory-policy" {
		t.Fatalf("candidates = %#v", semantic.Body.Candidates)
	}
}

func TestRememberCreatesNestedPathOnlyAfterExplicitDistinctDecisionAndWaitsForReady(t *testing.T) {
	oldInterval := jobPollInterval
	jobPollInterval = time.Millisecond
	t.Cleanup(func() { jobPollInterval = oldInterval })
	var createBody map[string]any
	jobReads := 0
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/search":
			_, _ = w.Write([]byte(`{"items":[{"kind":"document","id":"existing","path":"shared/existing","title":"Existing","revision":"r1","canonical_ref":"manifold://documents/existing@r1"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/documents/incident-note":
			if created {
				_, _ = w.Write([]byte(`{"id":"incident-note","title":"Incident note","revision":"r1","status":"ready","etag":"\"v1\""}`))
			} else {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":"resource_not_found","title":"Not found","detail":"missing","request_id":"request"}`))
			}
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/documents":
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Error(err)
			}
			created = true
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"document":{"id":"incident-note","revision":"r1"},"job":{"id":"d5vqh9idc6b5u5sctq00","status":"accepted"}}`))
		case r.URL.Path == "/api/v1/jobs/d5vqh9idc6b5u5sctq00":
			jobReads++
			status := "indexing"
			if jobReads > 1 {
				status = "ready"
			}
			_, _ = w.Write([]byte(`{"id":"d5vqh9idc6b5u5sctq00","status":"` + status + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &client{baseURL: server.URL, apiKey: "test", http: server.Client()}

	payload, err := executeRemember(c, []string{
		"--id", "incident-note", "--title", "Incident note", "--content", "Isolated evidence.",
		"--folder-path", "tasks/incident-42", "--new", "--reason", "separate incident lifecycle",
		"--wait-timeout", "1s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if createBody["folder_path"] != "tasks/incident-42" || createBody["folder_id"] != nil {
		t.Fatalf("create body = %#v", createBody)
	}
	metadata, _ := createBody["metadata"].(map[string]any)
	if metadata["creation_reason"] != "separate incident lifecycle" {
		t.Fatalf("creation metadata = %#v", metadata)
	}
	var response struct {
		Job      jobState `json:"job"`
		Document struct {
			Status string `json:"status"`
		} `json:"document"`
	}
	if err := json.Unmarshal(payload, &response); err != nil || response.Job.Status != "ready" ||
		response.Document.Status != "ready" || jobReads < 2 {
		t.Fatalf("terminal response = %s, job reads=%d, err=%v", payload, jobReads, err)
	}
}

func TestRememberRefusesToMutateWhenSemanticDiscoveryIsDegraded(t *testing.T) {
	postCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/search" {
			_, _ = w.Write([]byte(`{"items":[],"degraded_dependencies":["openviking"]}`))
			return
		}
		if r.Method == http.MethodPost {
			postCalls++
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	c := &client{baseURL: server.URL, apiKey: "test", http: server.Client()}

	_, err := executeRemember(c, []string{
		"--id", "new-policy", "--title", "New policy", "--content", "Canonical content.",
		"--folder-path", "shared/agent-practice",
	})
	var semantic *apiError
	if !errors.As(err, &semantic) || semantic.Body.Code != "discovery_degraded" {
		t.Fatalf("remember error = %#v", err)
	}
	if postCalls != 0 {
		t.Fatalf("degraded discovery allowed %d mutation requests", postCalls)
	}
}

func TestRememberDoesNotBlindlyRetryAnETagMismatch(t *testing.T) {
	putCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/search":
			_, _ = w.Write([]byte(`{"items":[]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/documents/memory-policy":
			w.Header().Set("ETag", `"v3"`)
			_, _ = w.Write([]byte(`{"id":"memory-policy","title":"Memory policy","etag":"\"v3\""}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/documents/memory-policy":
			putCalls++
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"etag_mismatch","title":"Resource changed","detail":"changed","request_id":"request","remediation":{"summary":"Read and merge."}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &client{baseURL: server.URL, apiKey: "test", http: server.Client()}

	_, err := executeRemember(c, []string{
		"--update", "memory-policy", "--content", "Reconciled content.",
	})
	var semantic *apiError
	if !errors.As(err, &semantic) || semantic.Body.Code != "etag_mismatch" {
		t.Fatalf("update error = %#v", err)
	}
	if putCalls != 1 {
		t.Fatalf("etag mismatch triggered %d PUT requests, want exactly one", putCalls)
	}
}
