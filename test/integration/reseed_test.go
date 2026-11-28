//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// A new index, seeded from the canonical database at start, serves the memory
// written before it existed: graph facts at once, documents once the
// background embedder has given their chunks vectors.
func TestReseededIndexServesExistingMemory(t *testing.T) {
	if os.Getenv("MANIFOLD_INTEGRATION_PHASE") != "reseed_verify" {
		t.Skip("reseed phase is not selected")
	}
	c := newClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	status := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/status", nil, "", "", http.StatusOK)
	if got := stringField(t, status.Body, "state"); got != "ready" {
		t.Fatalf("status state = %q, want ready; body=%s", got, mustJSON(status.Body))
	}
	search := func(mode, sourceType, query string) []any {
		t.Helper()
		found := c.expectStatus(t, ctx, http.MethodPost, "/api/v1/search", map[string]any{
			"query": query, "mode": mode, "limit": 10, "source_types": []string{sourceType},
		}, "", "", http.StatusOK)
		if degraded, ok := found.Body["degraded_dependencies"].([]any); ok && len(degraded) > 0 {
			t.Fatalf("%s retrieval degraded: %s", mode, mustJSON(found.Body))
		}
		return arrayField(t, found.Body, "items")
	}
	if facts := search("graph", "fact", "Manifold integration state"); len(facts) == 0 {
		t.Fatal("the seeded index lost the explicit facts")
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, raw := range search("semantic", "document", "Manifold integration evidence") {
			if raw.(map[string]any)["id"] == "integration-note-renamed" {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the seeded index never served the existing document: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}
