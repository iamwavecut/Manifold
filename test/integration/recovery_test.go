//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestRecoverDependencyFailure(t *testing.T) {
	if os.Getenv("MANIFOLD_INTEGRATION_PHASE") != "recover_failure" {
		t.Skip("dependency failure recovery phase is not selected")
	}
	data, err := os.ReadFile(os.Getenv("MANIFOLD_INTEGRATION_STATE"))
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	jobID := string(data)
	c.expectStatus(t, ctx, http.MethodPost, "/api/v1/jobs/"+jobID+"/retry", nil,
		"integration-dependency-retry", "", http.StatusAccepted)
	waitForJob(t, ctx, c, jobID, "ready")
	document := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/documents/dependency-failure", nil,
		"", "", http.StatusOK)
	if document.Body["status"] != "ready" || document.Body["revision"] != "r1" {
		t.Fatalf("recovery changed revision or failed to complete: %s", mustJSON(document.Body))
	}
	status := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/status", nil, "", "", http.StatusOK)
	if pipeline := objectField(t, status.Body, "pipeline"); pipeline["state"] != "ready" {
		t.Fatalf("recovered current pipeline is not ready: %s", mustJSON(pipeline))
	}
}
