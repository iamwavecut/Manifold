//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDocumentExtractionProducesCanonicalGraph(t *testing.T) {
	if os.Getenv("MANIFOLD_INTEGRATION_PHASE") != "" {
		t.Skip("extraction phase is not selected")
	}
	c := newClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	const id = "integration-derived-memory"
	const first = "Mira Chen works at Meridian Labs."
	const second = "Mira Chen works at Borealis Labs."
	created := c.expectStatus(t, ctx, http.MethodPost, "/api/v1/documents", map[string]any{
		"id": id, "folder_path": "integration-derived/graph-proof", "title": "Derived graph proof", "format": "markdown", "content": first,
	}, "derived-create", "", http.StatusAccepted)
	waitForJob(t, ctx, c, nestedStringField(t, created.Body, "job", "id"), "ready")

	entities := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/entities?query=Mira", nil, "", "", http.StatusOK)
	var entityID string
	for _, raw := range arrayField(t, entities.Body, "items") {
		entity := raw.(map[string]any)
		if entity["name"] == "Mira Chen" {
			entityID = stringField(t, entity, "id")
		}
	}
	if entityID == "" || strings.Contains(entityID, ":") {
		t.Fatalf("extraction did not create a public entity: %s", mustJSON(entities.Body))
	}

	checkFact := func(want, revision string) string {
		t.Helper()
		graph := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/entities/"+entityID, nil, "", "", http.StatusOK)
		var factID string
		for _, raw := range arrayField(t, graph.Body, "facts") {
			fact := raw.(map[string]any)
			if fact["source_document_id"] != id || fact["status"] != "active" {
				continue
			}
			if fact["object"] != want || fact["source_revision"] != revision {
				t.Fatalf("stale or ungrounded current fact: %s", mustJSON(fact))
			}
			factID = stringField(t, fact, "id")
		}
		if len(factID) != 20 {
			t.Fatalf("no derived fact with a public XID: %s", mustJSON(graph.Body))
		}
		if len(arrayField(t, graph.Body, "relations")) == 0 {
			t.Fatalf("extraction produced no public relations: %s", mustJSON(graph.Body))
		}
		return factID
	}
	factID := checkFact("Meridian Labs", "r1")
	search := func() response {
		t.Helper()
		found := c.expectStatus(t, ctx, http.MethodPost, "/api/v1/search", map[string]any{
			"query": "Mira Chen", "mode": "graph", "limit": 50, "scope_glob": "integration-derived/**", "source_types": []string{"fact"},
		}, "", "", http.StatusOK)
		if degraded, ok := found.Body["degraded_dependencies"].([]any); ok && len(degraded) > 0 {
			t.Fatalf("graph retrieval degraded: %s", mustJSON(found.Body))
		}
		return found
	}
	found := search()
	var canonical bool
	for _, raw := range arrayField(t, found.Body, "items") {
		hit := raw.(map[string]any)
		if hit["id"] == factID && hit["canonical_ref"] == "manifold://facts/"+factID {
			canonical = true
		}
	}
	if !canonical {
		t.Fatalf("graph retrieval lost extracted fact/provenance: %s", mustJSON(found.Body))
	}

	document := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/documents/"+id, nil, "", "", http.StatusOK)
	updated := c.expectStatus(t, ctx, http.MethodPut, "/api/v1/documents/"+id, map[string]any{"content": second}, "derived-update", document.Header.Get("ETag"), http.StatusAccepted)
	waitForJob(t, ctx, c, nestedStringField(t, updated.Body, "job", "id"), "ready")
	checkFact("Borealis Labs", "r2")
	// Identical text at a new origin corroborates an existing Brain fact.
	// Its candidate must reference the serving fact, not the hidden audit row.
	document = c.expectStatus(t, ctx, http.MethodGet, "/api/v1/documents/"+id, nil, "", "", http.StatusOK)
	updated = c.expectStatus(t, ctx, http.MethodPut, "/api/v1/documents/"+id, map[string]any{"content": second}, "derived-confirm", document.Header.Get("ETag"), http.StatusAccepted)
	waitForJob(t, ctx, c, nestedStringField(t, updated.Body, "job", "id"), "ready")
	checkFact("Borealis Labs", "r3")
	confirmed := arrayField(t, search().Body, "items")
	if len(confirmed) == 0 {
		t.Fatal("corroborated current revision disappeared from graph retrieval")
	}
	for _, raw := range confirmed {
		if stringField(t, raw.(map[string]any), "revision") != "r3" {
			t.Fatalf("corroborated search retained stale provenance: %s", mustJSON(raw))
		}
	}
	history := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/documents/"+id+"/revisions/r1", nil, "", "", http.StatusOK)
	if history.Body["content"] != first {
		t.Fatalf("immutable revision changed: %s", mustJSON(history.Body))
	}
	for _, raw := range arrayField(t, search().Body, "items") {
		if strings.Contains(stringField(t, raw.(map[string]any), "snippet"), "Meridian Labs") {
			t.Fatalf("superseded content survived current graph retrieval: %s", mustJSON(raw))
		}
	}

	document = c.expectStatus(t, ctx, http.MethodGet, "/api/v1/documents/"+id, nil, "", "", http.StatusOK)
	deleted := c.expectStatus(t, ctx, http.MethodDelete, "/api/v1/documents/"+id, nil, "derived-delete", document.Header.Get("ETag"), http.StatusAccepted)
	waitForJob(t, ctx, c, nestedStringField(t, deleted.Body, "job", "id"), "ready")
	if hits := arrayField(t, search().Body, "items"); len(hits) != 0 {
		t.Fatalf("deleted document remains in graph retrieval: %s", mustJSON(hits))
	}
}
