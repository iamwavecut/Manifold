//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

const refusedDocument = "provider-refusal"

// The script switches the provider to refuse extraction with 402 before this
// phase. Manifold must name the cause, and keep naming it when its own
// recovery retries the document while the provider still refuses.
func TestProviderRefusalIsNamed(t *testing.T) {
	if os.Getenv("MANIFOLD_INTEGRATION_PHASE") != "provider_refusal" {
		t.Skip("provider refusal phase is not selected")
	}
	c := newClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	created := c.expectStatus(t, ctx, http.MethodPost, "/api/v1/documents", map[string]any{
		"id":      refusedDocument,
		"title":   "Provider refusal",
		"format":  "markdown",
		"content": "Mira Chen works at Cobalt Labs.",
	}, "integration-provider-refusal-1", "", http.StatusAccepted)
	jobID := nestedStringField(t, created.Body, "job", "id")

	// The second attempt is an automatic recovery probe through Brain's retry.
	job := awaitJob(t, ctx, c, jobID, func(job map[string]any) bool {
		attempts, _ := job["attempts"].(float64)
		return job["status"] == "partially_ready" && attempts >= 2
	})
	problem := objectField(t, job, "error")
	if got := stringField(t, problem, "code"); got != "provider_credits_exhausted" {
		t.Fatalf("job error code = %q, want provider_credits_exhausted; body=%s", got, mustJSON(job))
	}
	status := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/status", nil, "", "", http.StatusOK)
	if got := stringField(t, status.Body, "state"); got != "ready" {
		t.Fatalf("component state = %q, want ready; body=%s", got, mustJSON(status.Body))
	}
	if err := os.WriteFile(os.Getenv("MANIFOLD_INTEGRATION_STATE"), []byte(jobID), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The provider accepts requests again; nobody asks Manifold to retry.
func TestProviderRecoveryRetriesByItself(t *testing.T) {
	if os.Getenv("MANIFOLD_INTEGRATION_PHASE") != "provider_recovery" {
		t.Skip("provider recovery phase is not selected")
	}
	jobID, err := os.ReadFile(os.Getenv("MANIFOLD_INTEGRATION_STATE"))
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	awaitJob(t, ctx, c, string(jobID), func(job map[string]any) bool { return job["status"] == "ready" })
	status := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/status", nil, "", "", http.StatusOK)
	pipeline := objectField(t, status.Body, "pipeline")
	if blocked, _ := pipeline["blocked_documents"].(float64); blocked != 0 {
		t.Fatalf("blocked documents after recovery: %s", mustJSON(pipeline))
	}
	entities := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/entities?query=Cobalt", nil, "", "", http.StatusOK)
	if len(arrayField(t, entities.Body, "items")) == 0 {
		t.Fatalf("recovered extraction produced no entity: %s", mustJSON(entities.Body))
	}
	document := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/documents/"+refusedDocument, nil, "", "", http.StatusOK)
	deleted := c.expectStatus(t, ctx, http.MethodDelete, "/api/v1/documents/"+refusedDocument, nil,
		"integration-provider-refusal-delete", document.Header.Get("ETag"), http.StatusAccepted)
	waitForJob(t, ctx, c, nestedStringField(t, deleted.Body, "job", "id"), "ready")
}

// awaitJob polls through failed attempts, which recovery repeats.
func awaitJob(t *testing.T, ctx context.Context, c *client, id string, done func(map[string]any) bool) map[string]any {
	t.Helper()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		job := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/jobs/"+id, nil, "", "", http.StatusOK).Body
		if done(job) {
			return job
		}
		select {
		case <-ctx.Done():
			t.Fatalf("job %s did not reach the expected state: %v; body=%s", id, ctx.Err(), mustJSON(job))
		case <-ticker.C:
		}
	}
}
