//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

// A document whose extraction is interrupted must converge on the same
// revision: one job that reaches ready, one fact, two entities. The script
// holds the provider so the extraction stays in flight, then restarts
// Manifold (restart_*), or lets the extraction time out and retries the job
// once the provider answers again (finished_retry_*).
type convergenceState struct {
	JobID    string  `json:"job_id"`
	Facts    float64 `json:"facts"`
	Entities float64 `json:"entities"`
}

type convergenceCase struct {
	id       string
	content  string
	terminal bool
}

func TestRestartConvergenceSubmit(t *testing.T) {
	submitHeld(t, "restart_submit", convergenceCase{
		id: "restart-convergence", content: "Noor Patel works at Delta Labs.",
	})
}

func TestRestartConvergenceVerify(t *testing.T) {
	verifyConverged(t, "restart_verify", "restart-convergence", false)
}

func TestFinishedRevisionRetrySubmit(t *testing.T) {
	submitHeld(t, "finished_retry_submit", convergenceCase{
		id: "finished-retry", content: "Ines Moreno works at Echo Labs.", terminal: true,
	})
}

func TestFinishedRevisionRetryVerify(t *testing.T) {
	verifyConverged(t, "finished_retry_verify", "finished-retry", true)
}

// submitHeld writes a document whose extraction cannot finish yet, and leaves
// Manifold either waiting for it or, when terminal, after it gave up waiting.
func submitHeld(t *testing.T, phase string, tc convergenceCase) {
	t.Helper()
	if os.Getenv("MANIFOLD_INTEGRATION_PHASE") != phase {
		t.Skip(phase + " phase is not selected")
	}
	c := newClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	before := graphCounts(t, ctx, c)
	created := c.expectStatus(t, ctx, http.MethodPost, "/api/v1/documents", map[string]any{
		"id": tc.id, "title": tc.id, "format": "markdown", "content": tc.content,
	}, "integration-"+tc.id, "", http.StatusAccepted)
	before.JobID = nestedStringField(t, created.Body, "job", "id")
	if tc.terminal {
		job := waitForTerminalJob(t, ctx, c, before.JobID)
		if got := stringField(t, job, "status"); got != "partially_ready" {
			t.Fatalf("job status while the provider is held = %q, want partially_ready; body=%s", got, mustJSON(job))
		}
	} else {
		awaitExtracting(t, ctx, c, tc.id)
	}
	data, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("MANIFOLD_INTEGRATION_STATE"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func verifyConverged(t *testing.T, phase, id string, retry bool) {
	t.Helper()
	if os.Getenv("MANIFOLD_INTEGRATION_PHASE") != phase {
		t.Skip(phase + " phase is not selected")
	}
	data, err := os.ReadFile(os.Getenv("MANIFOLD_INTEGRATION_STATE"))
	if err != nil {
		t.Fatal(err)
	}
	var before convergenceState
	if err := json.Unmarshal(data, &before); err != nil {
		t.Fatal(err)
	}
	c := newClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	if retry {
		retryUnlessRecovered(t, ctx, c, before.JobID, "integration-"+id+"-retry")
	}
	waitForJob(t, ctx, c, before.JobID, "ready")
	after := graphCounts(t, ctx, c)
	if after.Facts != before.Facts+1 || after.Entities != before.Entities+2 {
		t.Fatalf("converged graph = %+v, want one new fact and two new entities over %+v", after, before)
	}
	document := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/documents/"+id, nil, "", "", http.StatusOK)
	if document.Body["status"] != "ready" || document.Body["revision"] != "r1" {
		t.Fatalf("converged document = %s", mustJSON(document.Body))
	}
	deleted := c.expectStatus(t, ctx, http.MethodDelete, "/api/v1/documents/"+id, nil,
		"integration-"+id+"-delete", document.Header.Get("ETag"), http.StatusAccepted)
	waitForJob(t, ctx, c, nestedStringField(t, deleted.Body, "job", "id"), "ready")
}

// awaitExtracting waits until Manifold has sent the document to extraction;
// the job itself stays `indexing` while it runs.
func awaitExtracting(t *testing.T, ctx context.Context, c *client, id string) {
	t.Helper()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		document := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/documents/"+id, nil, "", "", http.StatusOK)
		if document.Body["status"] == "extracting" {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("document %s did not reach extracting: %v; body=%s", id, ctx.Err(), mustJSON(document.Body))
		case <-ticker.C:
		}
	}
}

// graphCounts reads how many facts and entities Manifold holds.
func graphCounts(t *testing.T, ctx context.Context, c *client) convergenceState {
	t.Helper()
	status := c.expectStatus(t, ctx, http.MethodGet, "/api/v1/status", nil, "", "", http.StatusOK)
	counts := objectField(t, status.Body, "counts")
	facts, ok := counts["facts"].(float64)
	entities, ok2 := counts["entities"].(float64)
	if !ok || !ok2 {
		t.Fatalf("status counts lack facts or entities: %s", mustJSON(status.Body))
	}
	return convergenceState{Facts: facts, Entities: entities}
}
