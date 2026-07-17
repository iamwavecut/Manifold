package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	manifoldapi "github.com/iamwavecut/Manifold/internal/api"
	"github.com/iamwavecut/Manifold/internal/auth"
	"github.com/iamwavecut/Manifold/internal/config"
	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/service"
	"github.com/iamwavecut/Manifold/internal/store"
	"github.com/iamwavecut/Manifold/internal/upstream"
)

const adminSecret = "test-bootstrap-key-that-is-long-enough"

type testServer struct {
	handler http.Handler
	store   *store.Store
	service *service.Service
	spec    *huma.OpenAPI
}

func newTestServer(t *testing.T, documents upstream.DocumentStore) testServer {
	t.Helper()
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := auth.NewManager(db)
	if err := manager.Bootstrap(t.Context(), adminSecret); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{
		PublicURL: "http://manifold.test", MaxBodyBytes: 5 << 20,
		HTTPTimeout: time.Second, WorkerInterval: time.Millisecond,
	}
	if documents == nil {
		documents = fakeDocuments{}
	}
	svc := service.New(db, documents, fakeGraph{}, cfg.PublicURL, logger, cfg.WorkerInterval)
	api := manifoldapi.New(svc, manager, cfg, logger, nil)
	return testServer{handler: api.Handler, store: db, service: svc, spec: api.Spec}
}

func request(t *testing.T, handler http.Handler, method, path, secret, idempotency string, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	if idempotency != "" {
		req.Header.Set("Idempotency-Key", idempotency)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func decodeObject(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &object); err != nil {
		t.Fatalf("decode response %d %q: %v", recorder.Code, recorder.Body.String(), err)
	}
	return object
}

func assertProblem(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, status, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/problem+json") {
		t.Fatalf("Content-Type = %q, want application/problem+json", contentType)
	}
	problem := decodeObject(t, recorder)
	if problem["code"] != code {
		t.Fatalf("code = %#v, want %q; body=%s", problem["code"], code, recorder.Body.String())
	}
	requestID, _ := problem["request_id"].(string)
	if !identity.IsXID(requestID) {
		t.Fatalf("request_id = %q, want XID", requestID)
	}
	remediation, ok := problem["remediation"].(map[string]any)
	if !ok || remediation["summary"] == "" {
		t.Fatalf("semantic error lacks remediation: %s", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "SELECT ") || strings.Contains(recorder.Body.String(), "stack trace") {
		t.Fatalf("semantic error leaked internals: %s", recorder.Body.String())
	}
	return problem
}

func TestAuthenticationAndCapabilityErrorsAreSemantic(t *testing.T) {
	server := newTestServer(t, nil)

	unauthenticated := request(t, server.handler, http.MethodGet, "/api/v1/documents", "", "", "", nil)
	assertProblem(t, unauthenticated, http.StatusUnauthorized, "invalid_api_key")

	hash, err := auth.HashSecret("read-only-key-that-is-long-enough")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.CreateAPIKey(t.Context(), "reader", hash, []string{auth.ReadDocuments}); err != nil {
		t.Fatal(err)
	}
	forbidden := request(t, server.handler, http.MethodPost, "/api/v1/documents",
		"read-only-key-that-is-long-enough", "capability-check-1",
		`{"title":"No write","content":"blocked"}`, nil)
	problem := assertProblem(t, forbidden, http.StatusForbidden, "missing_capability")
	required, ok := problem["required_capabilities"].([]any)
	if !ok || len(required) != 1 || required[0] != auth.WriteDocuments {
		t.Fatalf("required_capabilities = %#v", problem["required_capabilities"])
	}
}

func TestValidationReturnsAllIndependentViolations(t *testing.T) {
	server := newTestServer(t, nil)
	recorder := request(t, server.handler, http.MethodPost, "/api/v1/documents", adminSecret, "short",
		`{"id":"NOT A SLUG","title":"","format":"unknown"}`, nil)
	problem := assertProblem(t, recorder, http.StatusUnprocessableEntity, "validation_failed")
	violations, ok := problem["violations"].([]any)
	if !ok || len(violations) < 3 {
		t.Fatalf("violations = %#v, want at least three independent field errors", problem["violations"])
	}
}

func TestDocumentIdempotencySlugConflictAndETagRemediation(t *testing.T) {
	server := newTestServer(t, nil)
	body := `{"id":"agent-memory","title":"Agent memory","content":"Canonical text"}`

	first := request(t, server.handler, http.MethodPost, "/api/v1/documents", adminSecret, "create-agent-memory-1", body, nil)
	if first.Code != http.StatusAccepted {
		t.Fatalf("create status = %d; body=%s", first.Code, first.Body.String())
	}
	firstObject := decodeObject(t, first)
	firstJob := firstObject["job"].(map[string]any)["id"]

	replay := request(t, server.handler, http.MethodPost, "/api/v1/documents", adminSecret, "create-agent-memory-1", body, nil)
	if replay.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d; body=%s", replay.Code, replay.Body.String())
	}
	replayJob := decodeObject(t, replay)["job"].(map[string]any)["id"]
	if replayJob != firstJob {
		t.Fatalf("idempotent replay created another job: %v != %v", replayJob, firstJob)
	}

	changedReplay := request(t, server.handler, http.MethodPost, "/api/v1/documents", adminSecret,
		"create-agent-memory-1", `{"id":"different-memory","title":"Different","content":"Changed intent"}`, nil)
	assertProblem(t, changedReplay, http.StatusConflict, "idempotency_key_reused")

	duplicate := request(t, server.handler, http.MethodPost, "/api/v1/documents", adminSecret, "create-agent-memory-2", body, nil)
	problem := assertProblem(t, duplicate, http.StatusConflict, "slug_taken")
	if problem["suggested_slug"] != "agent-memory-2" {
		t.Fatalf("suggested_slug = %#v", problem["suggested_slug"])
	}

	update := request(t, server.handler, http.MethodPut, "/api/v1/documents/agent-memory", adminSecret, "update-agent-memory-1",
		`{"content":"Changed"}`, map[string]string{"If-Match": `"v999"`})
	etagProblem := assertProblem(t, update, http.StatusConflict, "etag_mismatch")
	if etagProblem["current_etag"] != `"v1"` {
		t.Fatalf("current_etag = %#v", etagProblem["current_etag"])
	}
}

func TestFailedJobKeepsStructuredDependencyError(t *testing.T) {
	documents := fakeDocuments{writeErr: &upstream.DependencyError{
		Dependency: "openviking", Operation: "write", Retryable: true, Cause: errors.New("test outage"),
	}}
	server := newTestServer(t, documents)
	create := request(t, server.handler, http.MethodPost, "/api/v1/documents", adminSecret, "create-failing-document",
		`{"id":"failing-document","title":"Failing document","content":"content"}`, nil)
	if create.Code != http.StatusAccepted {
		t.Fatalf("create status = %d; body=%s", create.Code, create.Body.String())
	}
	jobID := decodeObject(t, create)["job"].(map[string]any)["id"].(string)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go server.service.RunWorker(ctx)
	var job model.Job
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		job, err = server.store.GetJob(t.Context(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == model.JobFailed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if job.Status != model.JobFailed {
		t.Fatalf("job status = %s, want failed", job.Status)
	}
	if job.Error == nil || job.Error.Code != "dependency_unavailable" || !job.Error.Retryable {
		t.Fatalf("persisted semantic error = %#v", job.Error)
	}
	if !identity.IsXID(job.Error.RequestID) {
		t.Fatalf("persisted semantic error request_id = %q, want XID", job.Error.RequestID)
	}
	if job.Error.Job != "/api/v1/jobs/"+jobID || job.Error.Remediation.Summary == "" {
		t.Fatalf("persisted job remediation is incomplete: %#v", job.Error)
	}
}

func TestOpenAPIUsesTheSemanticErrorContractForEveryFailureResponse(t *testing.T) {
	server := newTestServer(t, nil)
	data, err := server.spec.YAML()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	failureResponses := 0
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) != 6 || trimmed[0] != '"' || (trimmed[1] != '4' && trimmed[1] != '5') ||
			trimmed[4:] != "\":" {
			continue
		}
		failureResponses++
		indent := len(line) - len(strings.TrimLeft(line, " "))
		hasProblemContent := false
		hasSemanticSchema := false
		for next := index + 1; next < len(lines); next++ {
			nextLine := lines[next]
			nextTrimmed := strings.TrimSpace(nextLine)
			nextIndent := len(nextLine) - len(strings.TrimLeft(nextLine, " "))
			if nextTrimmed != "" && nextIndent <= indent {
				break
			}
			if nextTrimmed == "application/problem+json:" {
				hasProblemContent = true
			}
			if nextTrimmed == `$ref: "#/components/schemas/Error"` {
				hasSemanticSchema = true
			}
		}
		if !hasProblemContent || !hasSemanticSchema {
			t.Errorf("failure response at line %d does not use application/problem+json Error", index+1)
		}
	}
	if failureResponses < 20 {
		t.Fatalf("inspected only %d failure responses; generated contract appears incomplete", failureResponses)
	}
	for _, field := range []string{"code", "request_id", "retryable", "remediation", "documentation_url"} {
		if !strings.Contains(string(data), "\n        - "+field+"\n") {
			t.Errorf("semantic Error schema does not require %s", field)
		}
	}
	for _, code := range []string{
		"validation_failed", "invalid_request", "invalid_api_key", "missing_capability",
		"resource_not_found", "slug_taken", "etag_mismatch", "idempotency_key_reused", "stale_rename_plan",
		"unrewritable_reference", "dependency_unavailable", "invalid_state_transition",
		"state_conflict", "csrf_failed", "storage_unavailable", "job_failed",
		"cannot_revoke_current_key", "internal_error", "request_failed", "error_code_not_found",
	} {
		if !strings.Contains(string(data), "code: "+code) {
			t.Errorf("OpenAPI Error schema lacks an example for %s", code)
		}
	}
}

func TestEveryPublicSemanticErrorCodeHasReachableDocumentation(t *testing.T) {
	server := newTestServer(t, nil)
	codes := []string{
		"validation_failed", "invalid_request", "invalid_api_key", "missing_capability",
		"resource_not_found", "slug_taken", "etag_mismatch", "idempotency_key_reused", "stale_rename_plan",
		"unrewritable_reference", "dependency_unavailable", "invalid_state_transition",
		"state_conflict", "csrf_failed", "storage_unavailable", "job_failed",
		"cannot_revoke_current_key", "internal_error", "request_failed", "error_code_not_found",
	}
	for _, code := range codes {
		recorder := request(t, server.handler, http.MethodGet, "/docs/errors/"+code, "", "", "", nil)
		if recorder.Code != http.StatusOK {
			t.Errorf("documentation for %s returned %d: %s", code, recorder.Code, recorder.Body.String())
		}
	}
}

func TestIdempotencyCoversGraphMutationsAndIsScopedToTheAPIKey(t *testing.T) {
	server := newTestServer(t, nil)
	const body = `{"id":"agent","name":"Agent","kind":"software"}`
	first := request(t, server.handler, http.MethodPost, "/api/v1/entities", adminSecret, "create-agent-entity", body, nil)
	if first.Code != http.StatusCreated {
		t.Fatalf("create entity status = %d: %s", first.Code, first.Body.String())
	}
	replay := request(t, server.handler, http.MethodPost, "/api/v1/entities", adminSecret, "create-agent-entity", body, nil)
	if replay.Code != http.StatusCreated || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("graph mutation was not replayed: status=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
	if replay.Body.String() != first.Body.String() {
		t.Fatalf("replayed representation changed:\nfirst: %s\nreplay: %s", first.Body.String(), replay.Body.String())
	}

	const secondSecret = "second-write-key-that-is-long-enough"
	hash, err := auth.HashSecret(secondSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.CreateAPIKey(t.Context(), "second-writer", hash, []string{auth.WriteGraph}); err != nil {
		t.Fatal(err)
	}
	crossKey := request(t, server.handler, http.MethodPost, "/api/v1/entities", secondSecret, "create-agent-entity", body, nil)
	assertProblem(t, crossKey, http.StatusConflict, "slug_taken")
}

func TestListEndpointsExposeCursorPaginationAndTree(t *testing.T) {
	server := newTestServer(t, nil)
	for _, id := range []string{"alpha", "beta", "gamma"} {
		if _, err := server.store.CreateEntity(t.Context(), model.Entity{
			ID: id, Name: strings.ToUpper(id), Kind: "test",
		}); err != nil {
			t.Fatal(err)
		}
	}
	first := request(t, server.handler, http.MethodGet, "/api/v1/entities?limit=2", adminSecret, "", "", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first page status = %d: %s", first.Code, first.Body.String())
	}
	firstPage := decodeObject(t, first)
	if items, _ := firstPage["items"].([]any); len(items) != 2 || firstPage["next_cursor"] != "c2" {
		t.Fatalf("first page = %#v", firstPage)
	}
	second := request(t, server.handler, http.MethodGet, "/api/v1/entities?limit=2&cursor=c2", adminSecret, "", "", nil)
	secondPage := decodeObject(t, second)
	if items, _ := secondPage["items"].([]any); len(items) != 1 {
		t.Fatalf("second page = %#v", secondPage)
	}
	invalid := request(t, server.handler, http.MethodGet, "/api/v1/entities?cursor=not-a-cursor", adminSecret, "", "", nil)
	assertProblem(t, invalid, http.StatusUnprocessableEntity, "validation_failed")

	tree := request(t, server.handler, http.MethodGet, "/api/v1/tree", adminSecret, "", "", nil)
	if tree.Code != http.StatusOK {
		t.Fatalf("tree status = %d: %s", tree.Code, tree.Body.String())
	}
	treeBody := decodeObject(t, tree)
	if _, ok := treeBody["folders"]; !ok {
		t.Fatalf("tree lacks folders: %#v", treeBody)
	}
	if _, ok := treeBody["documents"]; !ok {
		t.Fatalf("tree lacks documents: %#v", treeBody)
	}
}

func TestFactProvenanceDefaultsToCurrentRevisionAndPreservesZeroConfidence(t *testing.T) {
	server := newTestServer(t, nil)
	if _, err := server.store.CreateEntity(t.Context(), model.Entity{
		ID: "service", Name: "Service", Kind: "system",
	}); err != nil {
		t.Fatal(err)
	}
	document := request(t, server.handler, http.MethodPost, "/api/v1/documents", adminSecret,
		"create-source-document", `{"id":"source-document","title":"Source","content":"Evidence"}`, nil)
	if document.Code != http.StatusAccepted {
		t.Fatalf("create document status = %d: %s", document.Code, document.Body.String())
	}
	fact := request(t, server.handler, http.MethodPost, "/api/v1/facts", adminSecret,
		"create-zero-confidence-fact", `{
			"entity_id":"service",
			"predicate":"state",
			"object":"observed",
			"confidence":0,
			"source_document_id":"source-document"
		}`, nil)
	if fact.Code != http.StatusCreated {
		t.Fatalf("create fact status = %d: %s", fact.Code, fact.Body.String())
	}
	factBody := decodeObject(t, fact)
	created, _ := factBody["fact"].(map[string]any)
	if created["confidence"] != float64(0) || created["source_revision"] != "r1" {
		t.Fatalf("fact = %#v", created)
	}
	sources := request(t, server.handler, http.MethodGet, "/api/v1/sources", adminSecret, "", "", nil)
	sourcePage := decodeObject(t, sources)
	items, _ := sourcePage["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("sources = %#v", sourcePage)
	}
	source, _ := items[0].(map[string]any)
	if source["id"] != "source-document-r1" || source["document_id"] != "source-document" {
		t.Fatalf("source = %#v", source)
	}
}

type fakeDocuments struct {
	writeErr error
}

func (f fakeDocuments) Health(context.Context) error                      { return nil }
func (f fakeDocuments) Exists(context.Context, string) (bool, error)      { return true, nil }
func (f fakeDocuments) Mkdir(context.Context, string, string) error       { return nil }
func (f fakeDocuments) Write(context.Context, string, string, bool) error { return f.writeErr }
func (f fakeDocuments) Move(context.Context, string, string) error        { return nil }
func (f fakeDocuments) Delete(context.Context, string, bool) error        { return nil }
func (f fakeDocuments) Snapshot(context.Context, string, []string) (string, error) {
	return "snapshot-1", nil
}
func (f fakeDocuments) Search(context.Context, string, string, int) ([]model.SearchHit, error) {
	return nil, nil
}

type fakeGraph struct{}

func (fakeGraph) Health(context.Context) error { return nil }
func (fakeGraph) IngestDocument(context.Context, upstream.GraphDocument) (upstream.GraphIngestResult, error) {
	return upstream.GraphIngestResult{DocumentID: "brain-document"}, nil
}
func (fakeGraph) Search(context.Context, string, int, bool) ([]model.SearchHit, error) {
	return nil, nil
}
func (fakeGraph) IngestFact(context.Context, model.Fact, model.Entity) (string, string, error) {
	return "brain-fact", "active", nil
}
func (fakeGraph) IngestRelation(context.Context, model.Relation, model.Entity, model.Entity) (string, error) {
	return "brain-relation", nil
}
func (fakeGraph) RetractFact(context.Context, string, string) error { return nil }
func (fakeGraph) GetEntity(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}
