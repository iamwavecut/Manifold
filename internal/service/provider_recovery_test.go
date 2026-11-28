package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/store"
	"github.com/iamwavecut/Manifold/internal/upstream"
)

// refusingGraph fails extraction the way the engine reports a provider
// refusal, or with failure when that is set.
type refusingGraph struct {
	*workflowGraph
	mu      sync.Mutex
	refusal string
	failure error
}

func (g *refusingGraph) IngestDocument(ctx context.Context, doc upstream.GraphDocument) (upstream.GraphIngestResult, error) {
	g.mu.Lock()
	refusal, failure := g.refusal, g.failure
	g.mu.Unlock()
	if failure != nil {
		return upstream.GraphIngestResult{}, failure
	}
	if refusal != "" {
		return upstream.GraphIngestResult{}, &upstream.DependencyError{
			Dependency: "provider", Operation: "extract document", Status: 402, Refusal: refusal,
		}
	}
	return g.workflowGraph.IngestDocument(ctx, doc)
}

func (g *refusingGraph) set(refusal string, failure error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refusal, g.failure = refusal, failure
}

func newRefusingGraph() *refusingGraph { return &refusingGraph{workflowGraph: &workflowGraph{}} }

func syncRefusedDocument(t *testing.T, db *store.Store, svc *Service, graph *refusingGraph, id, class string) model.Job {
	t.Helper()
	graph.set(class, nil)
	_, job, err := svc.CreateDocument(t.Context(), model.Document{ID: id, Title: id, Format: "markdown", Content: "body of " + id})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err == nil {
		t.Fatalf("sync of %s succeeded although the provider refused", id)
	}
	failed, err := db.GetJob(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return failed
}

func TestProviderCreditFailureIsNamedAndShownInStatus(t *testing.T) {
	graph := newRefusingGraph()
	db, svc := newWorkflowService(t, ":memory:", &workflowDocuments{}, graph)

	job := syncRefusedDocument(t, db, svc, graph, "unpaid", "billing")

	if job.Status != model.JobPartiallyReady || job.Error == nil {
		t.Fatalf("job = %#v, want a partially ready job with an error", job)
	}
	if job.Error.Code != "provider_credits_exhausted" || job.Error.Status != 503 || !job.Error.Retryable {
		t.Fatalf("error = %#v, want a retryable provider_credits_exhausted", job.Error)
	}
	status, err := svc.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "ready" {
		t.Fatalf("component state = %q; the components answer, so deployment checks must still pass", status.State)
	}
	if status.Pipeline.State != "degraded" || status.Pipeline.BlockedDocuments != 1 ||
		status.Pipeline.BlockedReason != "provider_credits_exhausted" {
		t.Fatalf("pipeline = %#v", status.Pipeline)
	}
}

func TestOtherProviderRefusalsStayDependencyFailures(t *testing.T) {
	graph := newRefusingGraph()
	db, svc := newWorkflowService(t, ":memory:", &workflowDocuments{}, graph)

	job := syncRefusedDocument(t, db, svc, graph, "badkey", "auth")

	if job.Error == nil || job.Error.Code != "dependency_unavailable" || !job.Error.Retryable {
		t.Fatalf("error = %#v, want a retryable dependency_unavailable", job.Error)
	}
}

func TestRecoveryProbesOneBlockedSyncThenRetriesTheRest(t *testing.T) {
	graph := newRefusingGraph()
	db, svc := newWorkflowService(t, ":memory:", &workflowDocuments{}, graph)
	first := syncRefusedDocument(t, db, svc, graph, "first", "billing")
	second := syncRefusedDocument(t, db, svc, graph, "second", "billing")
	third := syncRefusedDocument(t, db, svc, graph, "third", "billing")
	graph.set("", nil)

	retried, err := svc.RecoverProviderBlocked(t.Context())
	if err != nil || retried != 1 {
		t.Fatalf("probe retried %d jobs, err = %v; want exactly one", retried, err)
	}
	if got := jobStatus(t, db, first.ID); got != model.JobAccepted {
		t.Fatalf("oldest blocked job = %s, want accepted", got)
	}
	if got := jobStatus(t, db, second.ID); got != model.JobPartiallyReady {
		t.Fatalf("second job = %s before the provider proved itself", got)
	}
	if err := svc.processOne(t.Context()); err != nil {
		t.Fatal(err)
	}

	retried, err = svc.RecoverProviderBlocked(t.Context())
	if err != nil || retried != 2 {
		t.Fatalf("after a success retried %d jobs, err = %v; want the remaining two", retried, err)
	}
	for _, id := range []string{second.ID, third.ID} {
		if got := jobStatus(t, db, id); got != model.JobAccepted {
			t.Fatalf("job %s = %s, want accepted", id, got)
		}
	}
}

func TestRecoveryIgnoresOtherFailuresAndSupersededRevisions(t *testing.T) {
	graph := newRefusingGraph()
	db, svc := newWorkflowService(t, ":memory:", &workflowDocuments{}, graph)
	graph.set("", errors.New("incomplete extraction"))
	if _, _, err := svc.CreateDocument(t.Context(), model.Document{ID: "broken", Title: "Broken", Format: "markdown", Content: "body"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err == nil {
		t.Fatal("sync unexpectedly succeeded")
	}
	stale := syncRefusedDocument(t, db, svc, graph, "edited", "billing")
	doc, err := db.GetDocument(t.Context(), "edited", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.UpdateDocument(t.Context(), doc.ID, doc.ETag, doc.Title, "new body", doc.Metadata, doc.Tags); err != nil {
		t.Fatal(err)
	}

	retried, err := svc.RecoverProviderBlocked(t.Context())
	if err != nil || retried != 0 {
		t.Fatalf("retried %d jobs, err = %v; want none", retried, err)
	}
	if got := jobStatus(t, db, stale.ID); got != model.JobPartiallyReady {
		t.Fatalf("superseded job = %s, want it left alone", got)
	}
}

func jobStatus(t *testing.T, db *store.Store, id string) model.JobStatus {
	t.Helper()
	job, err := db.GetJob(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return job.Status
}
