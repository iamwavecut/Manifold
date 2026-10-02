package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/iamwavecut/Manifold/internal/model"
)

func TestRevisionAPIKeepsStorageCheckpointPrivate(t *testing.T) {
	server := newTestServer(t, nil)
	doc, _, err := server.service.CreateDocument(t.Context(), model.Document{
		ID: "private-checkpoint", Title: "Private checkpoint", Format: "markdown", Content: "Canonical public evidence.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.store.SetDocumentSync(t.Context(), doc.ID, "ready", "private-brain-id", "private-snapshot-oid"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/v1/documents/private-checkpoint/revisions/r1",
		"/api/v1/documents/private-checkpoint/revisions",
	} {
		response := request(t, server.handler, http.MethodGet, path, adminSecret, "", "", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("revision status = %d, body = %s", response.Code, response.Body.String())
		}
		for _, forbidden := range []string{"snapshot_oid", "private-snapshot-oid", "private-brain-id", "viking://"} {
			if strings.Contains(response.Body.String(), forbidden) {
				t.Fatalf("revision response leaked storage mapping %q", forbidden)
			}
		}
		if !strings.Contains(response.Body.String(), "Canonical public evidence.") {
			t.Fatal("canonical revision content was lost")
		}
		unauthorized := request(t, server.handler, http.MethodGet, path, "", "", "", nil)
		if unauthorized.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated revision status = %d", unauthorized.Code)
		}
	}
}
