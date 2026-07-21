//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

type response struct {
	Status int
	Header http.Header
	Body   map[string]any
}

func TestEndToEnd(t *testing.T) {
	if phase := os.Getenv("MANIFOLD_INTEGRATION_PHASE"); phase != "" {
		t.Skip("end-to-end phase is not selected")
	}
	c := newClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	c.expectStatus(t, ctx, http.MethodGet, "/health", nil, "", "", http.StatusOK)
	c.expectStatus(t, ctx, http.MethodGet, "/ready", nil, "", "", http.StatusOK)
	status := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/status", nil, "", "", http.StatusOK)
	if got := stringField(t, status.Body, "state"); got != "ready" {
		t.Fatalf("status state = %q, want ready; body=%s", got, mustJSON(status.Body))
	}

	documentBody := map[string]any{
		"id":          "integration-note",
		"folder_path": "integration/integration-evidence",
		"title":       "Integration note",
		"format":      "markdown",
		"content": "# Manifold integration evidence\n\nSee [[integration-note]] and " +
			"manifold://documents/integration-note.\n",
		"metadata": map[string]any{"canonical": "integration-note"},
		"tags":     []string{"integration-note"},
	}
	created := c.expectStatus(t, ctx, http.MethodPost, "/api/v1/documents", documentBody,
		"integration-document-1", "", http.StatusAccepted)
	if len(arrayField(t, created.Body, "folders_created")) != 2 {
		t.Fatalf("nested create = %s, want two atomically created folders", mustJSON(created.Body))
	}
	jobID := nestedStringField(t, created.Body, "job", "id")
	waitForJob(t, ctx, c, jobID, "ready")

	document := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/documents/integration-note", nil, "", "", http.StatusOK)
	if document.Header.Get("ETag") == "" {
		t.Fatal("document response did not include ETag")
	}
	if got := stringField(t, document.Body, "revision"); got != "r1" {
		t.Fatalf("document revision = %q, want r1", got)
	}

	revisions := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/documents/integration-note/revisions",
		nil, "", "", http.StatusOK)
	if len(arrayField(t, revisions.Body, "items")) != 1 {
		t.Fatalf("revision page = %s, want one revision", mustJSON(revisions.Body))
	}
	c.expectStatus(t, ctx, http.MethodGet, "/api/v1/documents/integration-note/revisions/r1",
		nil, "", "", http.StatusOK)
	tree := c.expectStatus(t, ctx, http.MethodGet,
		"/api/v1/tree?glob=integration%2F**&types=document", nil, "", "", http.StatusOK)
	treeItems := arrayField(t, tree.Body, "items")
	if len(treeItems) != 1 || stringField(t, treeItems[0].(map[string]any), "path") !=
		"integration/integration-evidence/integration-note" {
		t.Fatalf("scoped tree = %s", mustJSON(tree.Body))
	}

	for _, entity := range []map[string]any{
		{"id": "manifold-service", "name": "Manifold", "kind": "service"},
		{"id": "integration-agent", "name": "Integration Agent", "kind": "agent"},
	} {
		key := "integration-entity-" + entity["id"].(string)
		c.expectStatus(t, ctx, http.MethodPost, "/api/v1/entities", entity, key, "", http.StatusCreated)
	}

	for index, object := range []string{"ready", "verified"} {
		c.expectStatus(t, ctx, http.MethodPost, "/api/v1/facts", map[string]any{
			"entity_id":          "manifold-service",
			"predicate":          "integration_state",
			"object":             object,
			"confidence":         1,
			"source_document_id": "integration-note",
			"source_revision":    "r1",
		}, fmt.Sprintf("integration-fact-%d", index+1), "", http.StatusCreated)
	}

	c.expectStatus(t, ctx, http.MethodPost, "/api/v1/relations", map[string]any{
		"from_entity_id": "integration-agent",
		"to_entity_id":   "manifold-service",
		"predicate":      "verifies",
		"confidence":     1,
	}, "integration-relation-1", "", http.StatusCreated)

	facts := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/facts?entity_id=manifold-service",
		nil, "", "", http.StatusOK)
	if len(arrayField(t, facts.Body, "items")) != 2 {
		t.Fatalf("facts page = %s, want two facts", mustJSON(facts.Body))
	}
	relations := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/relations?entity_id=manifold-service",
		nil, "", "", http.StatusOK)
	if len(arrayField(t, relations.Body, "items")) != 1 {
		t.Fatalf("relations page = %s, want one relation", mustJSON(relations.Body))
	}
	sources := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/sources", nil, "", "", http.StatusOK)
	if len(arrayField(t, sources.Body, "items")) != 1 {
		t.Fatalf("sources page = %s, want one grouped provenance source", mustJSON(sources.Body))
	}

	path := c.expectStatus(t, ctx, http.MethodPost, "/api/v1/graph/path", map[string]any{
		"from_entity_id": "integration-agent",
		"to_entity_id":   "manifold-service",
		"max_depth":      3,
	}, "", "", http.StatusOK)
	if depth, ok := path.Body["depth"].(float64); !ok || depth < 1 {
		t.Fatalf("graph path = %s, want non-empty path", mustJSON(path.Body))
	}

	conflicts := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/conflicts", nil, "", "", http.StatusOK)
	conflictItems := arrayField(t, conflicts.Body, "items")
	if len(conflictItems) != 1 {
		t.Fatalf("conflicts page = %s, want one explicit conflict", mustJSON(conflicts.Body))
	}
	conflict, ok := conflictItems[0].(map[string]any)
	if !ok {
		t.Fatalf("conflict item has type %T", conflictItems[0])
	}
	conflictID := stringField(t, conflict, "id")
	c.expectStatus(t, ctx, http.MethodPost, "/api/v1/conflicts/"+conflictID+"/resolve", map[string]any{
		"resolution": "The integration suite records the verified state.",
	}, "integration-conflict-1", "", http.StatusOK)

	search := c.expectStatus(t, ctx, http.MethodPost, "/api/v1/search", map[string]any{
		"query": "Manifold integration evidence", "mode": "hybrid", "limit": 10,
		"scope_glob": "integration/**", "source_types": []string{"document"},
	}, "", "", http.StatusOK)
	searchItems := arrayField(t, search.Body, "items")
	if len(searchItems) == 0 {
		t.Fatalf("search response has no canonical items: %s", mustJSON(search.Body))
	}
	for _, raw := range searchItems {
		hit := raw.(map[string]any)
		if strings.HasPrefix(stringField(t, hit, "id"), "viking://") ||
			stringField(t, hit, "canonical_ref") == "" || stringField(t, hit, "path") == "" {
			t.Fatalf("search exposed a non-canonical hit: %s", mustJSON(hit))
		}
	}
	contextPack := c.expectStatus(t, ctx, http.MethodPost, "/api/v1/context", map[string]any{
		"query": "What did the integration agent verify?", "token_budget": 600,
	}, "", "", http.StatusOK)
	if _, exists := contextPack.Body["context"]; !exists {
		t.Fatalf("context response has no context field: %s", mustJSON(contextPack.Body))
	}

	preview := c.expectStatus(t, ctx, http.MethodPost, "/api/v1/rename-plans", map[string]any{
		"operations": []map[string]string{{
			"resource_type": "document", "from": "integration-note", "to": "integration-note-renamed",
		}},
	}, "integration-rename-preview-1", "", http.StatusCreated)
	planID := stringField(t, preview.Body, "id")
	previewBody := objectField(t, preview.Body, "preview")
	if replacements := arrayField(t, previewBody, "replacements"); len(replacements) == 0 {
		t.Fatalf("rename preview did not find active references: %s", mustJSON(preview.Body))
	}
	applied := c.expectStatus(t, ctx, http.MethodPost, "/api/v1/rename-plans/"+planID+"/apply", nil,
		"integration-rename-apply-1", "", http.StatusAccepted)
	renameJobID := stringField(t, applied.Body, "id")
	waitForJob(t, ctx, c, renameJobID, "ready")

	old := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/documents/integration-note", nil,
		"", "", http.StatusNotFound)
	if got := stringField(t, old.Body, "code"); got != "resource_not_found" {
		t.Fatalf("old slug error code = %q, want resource_not_found; body=%s", got, mustJSON(old.Body))
	}
	renamed := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/documents/integration-note-renamed", nil,
		"", "", http.StatusOK)
	if got := stringField(t, renamed.Body, "id"); got != "integration-note-renamed" {
		t.Fatalf("renamed document id = %q", got)
	}
	content := stringField(t, renamed.Body, "content")
	if strings.Contains(content, "integration-note") && !strings.Contains(content, "integration-note-renamed") {
		t.Fatalf("active document references were not rewritten: %q", content)
	}
	if got := stringField(t, renamed.Body, "revision"); got != "r2" {
		t.Fatalf("renamed document revision = %q, want r2", got)
	}

	mismatch := c.expectStatus(t, ctx, http.MethodPut, "/api/v1/documents/integration-note-renamed",
		map[string]any{"title": "Must not be written", "content": content},
		"integration-etag-mismatch-1", `"stale"`, http.StatusConflict)
	if got := stringField(t, mismatch.Body, "code"); got != "etag_mismatch" {
		t.Fatalf("mismatch code = %q, want etag_mismatch; body=%s", got, mustJSON(mismatch.Body))
	}
	if _, exists := mismatch.Body["remediation"]; !exists {
		t.Fatalf("etag mismatch has no remediation: %s", mustJSON(mismatch.Body))
	}

	replay := c.expectStatus(t, ctx, http.MethodPost, "/api/v1/documents", documentBody,
		"integration-document-1", "", http.StatusAccepted)
	if got := nestedStringField(t, replay.Body, "job", "id"); got != jobID {
		t.Fatalf("idempotent replay job = %q, want %q", got, jobID)
	}
	reused := c.expectStatus(t, ctx, http.MethodPost, "/api/v1/documents", map[string]any{
		"id": "different-document", "title": "Different request", "content": "Different body",
	}, "integration-document-1", "", http.StatusConflict)
	if got := stringField(t, reused.Body, "code"); got != "idempotency_key_reused" {
		t.Fatalf("idempotency reuse code = %q, want idempotency_key_reused; body=%s", got, mustJSON(reused.Body))
	}
}

func TestCreateDependencyFailure(t *testing.T) {
	if os.Getenv("MANIFOLD_INTEGRATION_PHASE") != "create_failure" {
		t.Skip("dependency failure creation phase is not selected")
	}
	statePath := os.Getenv("MANIFOLD_INTEGRATION_STATE")
	if statePath == "" {
		t.Fatal("MANIFOLD_INTEGRATION_STATE is required")
	}
	c := newClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	created := c.expectStatus(t, ctx, http.MethodPost, "/api/v1/documents", map[string]any{
		"id":      "dependency-failure",
		"title":   "Dependency failure",
		"format":  "markdown",
		"content": "This document proves durable semantic job failures.",
	}, "integration-dependency-failure-1", "", http.StatusAccepted)
	jobID := nestedStringField(t, created.Body, "job", "id")
	job := waitForTerminalJob(t, ctx, c, jobID)
	if got := stringField(t, job, "status"); got != "partially_ready" {
		t.Fatalf("dependency job status = %q, want partially_ready; body=%s", got, mustJSON(job))
	}
	assertDependencyProblem(t, job)
	if err := os.WriteFile(statePath, []byte(jobID), 0o600); err != nil {
		t.Fatalf("write integration state: %v", err)
	}
}

func TestVerifyDependencyFailure(t *testing.T) {
	if os.Getenv("MANIFOLD_INTEGRATION_PHASE") != "verify_failure" {
		t.Skip("dependency failure verification phase is not selected")
	}
	statePath := os.Getenv("MANIFOLD_INTEGRATION_STATE")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read integration state: %v", err)
	}
	c := newClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	job := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/jobs/"+string(data), nil,
		"", "", http.StatusOK).Body
	if got := stringField(t, job, "status"); got != "partially_ready" {
		t.Fatalf("persisted dependency job status = %q, want partially_ready; body=%s", got, mustJSON(job))
	}
	assertDependencyProblem(t, job)
}

func newClient(t *testing.T) *client {
	t.Helper()
	baseURL := strings.TrimRight(os.Getenv("MANIFOLD_URL"), "/")
	apiKey := os.Getenv("MANIFOLD_API_KEY")
	if baseURL == "" || apiKey == "" {
		t.Skip("MANIFOLD_URL and MANIFOLD_API_KEY are required for integration tests")
	}
	return &client{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *client) expectStatus(
	t *testing.T,
	ctx context.Context,
	method string,
	path string,
	body any,
	idempotencyKey string,
	ifMatch string,
	want int,
) response {
	t.Helper()
	var encoded io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		encoded = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/health" && path != "/ready" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	res, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		t.Fatalf("%s %s: read response: %v", method, path, err)
	}
	decoded := map[string]any{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("%s %s: decode status %d response %q: %v", method, path, res.StatusCode, data, err)
		}
	}
	if res.StatusCode != want {
		t.Fatalf("%s %s status = %d, want %d; body=%s",
			method, path, res.StatusCode, want, mustJSON(decoded))
	}
	return response{Status: res.StatusCode, Header: res.Header.Clone(), Body: decoded}
}

func waitForTerminalJob(t *testing.T, ctx context.Context, c *client, id string) map[string]any {
	t.Helper()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		job := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/jobs/"+id, nil, "", "", http.StatusOK).Body
		switch stringField(t, job, "status") {
		case "ready", "failed", "partially_ready":
			return job
		}
		select {
		case <-ctx.Done():
			t.Fatalf("job %s did not reach a terminal state: %v", id, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForJob(t *testing.T, ctx context.Context, c *client, id string, want string) map[string]any {
	t.Helper()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		job := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/jobs/"+id, nil, "", "", http.StatusOK).Body
		status := stringField(t, job, "status")
		switch status {
		case want:
			return job
		case "failed", "partially_ready":
			t.Fatalf("job %s reached %s, want %s; body=%s", id, status, want, mustJSON(job))
		}
		select {
		case <-ctx.Done():
			t.Fatalf("job %s did not reach %s: %v", id, want, ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertDependencyProblem(t *testing.T, job map[string]any) {
	t.Helper()
	problem := objectField(t, job, "error")
	if got := stringField(t, problem, "code"); got != "dependency_unavailable" {
		t.Fatalf("job error code = %q, want dependency_unavailable; body=%s", got, mustJSON(job))
	}
	if retryable, ok := problem["retryable"].(bool); !ok || !retryable {
		t.Fatalf("job error retryable = %#v, want true; body=%s", problem["retryable"], mustJSON(job))
	}
	remediation := objectField(t, problem, "remediation")
	if len(arrayField(t, remediation, "steps")) == 0 {
		t.Fatalf("job error remediation has no steps: %s", mustJSON(job))
	}
	if requestID := stringField(t, problem, "request_id"); len(requestID) != 20 {
		t.Fatalf("job error request_id = %q, want XID", requestID)
	}
}

func stringField(t *testing.T, object map[string]any, name string) string {
	t.Helper()
	value, ok := object[name].(string)
	if !ok {
		t.Fatalf("field %q in %s has type %T, want string", name, mustJSON(object), object[name])
	}
	return value
}

func nestedStringField(t *testing.T, object map[string]any, parent string, name string) string {
	t.Helper()
	return stringField(t, objectField(t, object, parent), name)
}

func objectField(t *testing.T, object map[string]any, name string) map[string]any {
	t.Helper()
	value, ok := object[name].(map[string]any)
	if !ok {
		t.Fatalf("field %q in %s has type %T, want object", name, mustJSON(object), object[name])
	}
	return value
}

func arrayField(t *testing.T, object map[string]any, name string) []any {
	t.Helper()
	value, ok := object[name].([]any)
	if !ok {
		t.Fatalf("field %q in %s has type %T, want array", name, mustJSON(object), object[name])
	}
	return value
}

func mustJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}
