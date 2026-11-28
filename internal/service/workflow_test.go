package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/store"
	"github.com/iamwavecut/Manifold/internal/upstream"
	_ "modernc.org/sqlite"
)

func TestDocumentSyncFailureBeforeCheckpointMarksCurrentDocumentFailed(t *testing.T) {
	db, svc := newWorkflowService(t, ":memory:", &workflowDocuments{
		writeErr: errors.New("OpenViking write failed"),
	}, &workflowGraph{})

	doc, job, err := svc.CreateDocument(t.Context(), model.Document{
		ID: "failed-write", Title: "Failed write", Format: "markdown", Content: "body",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err == nil {
		t.Fatal("document sync unexpectedly succeeded after OpenViking write failed")
	}

	failedJob, err := db.GetJob(t.Context(), job.ID)
	if err != nil || failedJob.Status != model.JobFailed {
		t.Fatalf("failed job = %#v, err = %v", failedJob, err)
	}
	failedDoc, err := db.GetDocument(t.Context(), doc.ID, false)
	if err != nil || failedDoc.Status != model.JobFailed {
		t.Fatalf("document after pre-checkpoint failure = %#v, err = %v", failedDoc, err)
	}
	status, err := svc.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.Counts["failed_jobs"] != 1 || status.Counts["documents_failed"] != 1 ||
		status.Counts["documents_incomplete"] != 0 {
		t.Fatalf("pipeline counts mix current documents with historical job failures: %#v", status.Counts)
	}
	if status.Pipeline.State != "degraded" || status.Pipeline.CurrentDocuments != 1 ||
		status.Pipeline.FailedDocuments != 1 || status.Pipeline.FailedJobs != 1 {
		t.Fatalf("pipeline status = %#v, want the current failed document and failed job", status.Pipeline)
	}
	if status.State != "ready" {
		t.Fatalf("document pipeline failure changed component liveness state to %q", status.State)
	}
}

func TestPipelineStatusSeparatesHistoricalFailedJobFromCurrentReadyDocument(t *testing.T) {
	docs := &workflowDocuments{writeErr: errors.New("first OpenViking write failed")}
	db, svc := newWorkflowService(t, ":memory:", docs, &workflowGraph{})
	first, firstJob, err := svc.CreateDocument(t.Context(), model.Document{
		ID: "pipeline-history", Title: "Pipeline history", Format: "markdown", Content: "first revision",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err == nil {
		t.Fatal("initial sync unexpectedly succeeded after OpenViking failed")
	}
	docs.writeErr = nil
	if _, _, err := svc.UpdateDocument(t.Context(), first.ID, first.ETag,
		"Pipeline history", "second revision", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	failedJob, err := db.GetJob(t.Context(), firstJob.ID)
	if err != nil || failedJob.Status != model.JobFailed {
		t.Fatalf("historical revision job = %#v, err = %v", failedJob, err)
	}
	status, err := svc.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.Counts["failed_jobs"] != 1 || status.Counts["documents_failed"] != 0 ||
		status.Counts["documents_incomplete"] != 0 {
		t.Fatalf("historical failure changed current counts: %#v", status.Counts)
	}
	if status.Pipeline.State != "ready" || status.Pipeline.CurrentDocuments != 1 ||
		status.Pipeline.FailedDocuments != 0 || status.Pipeline.FailedJobs != 1 {
		t.Fatalf("pipeline status = %#v, want current ready and one historical failed job", status.Pipeline)
	}
}

func TestSupersededQueuedDocumentSyncDoesNotIndexNewerRevision(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	db, svc := newWorkflowService(t, dbPath, &workflowDocuments{}, &workflowGraph{})
	first, firstJob, err := svc.CreateDocument(t.Context(), model.Document{
		ID: "queued-update", Title: "Queued update", Format: "markdown", Content: "revision one",
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, secondJob, err := svc.UpdateDocument(t.Context(), first.ID, first.ETag,
		"Queued update", "revision two", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != "r2" {
		t.Fatalf("current revision = %q, want r2", updated.Revision)
	}
	if err := setWorkflowJobCreatedAt(dbPath, secondJob.ID, "9999-12-31T23:59:59.000000000Z"); err != nil {
		t.Fatal(err)
	}

	if err := svc.processOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, err := svc.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.Pipeline.State != "processing" || status.Pipeline.IncompleteDocuments != 1 || status.Pipeline.PendingJobs != 1 {
		t.Fatalf("queued current revision pipeline = %#v, want processing", status.Pipeline)
	}
	completedOldJob, err := db.GetJob(t.Context(), firstJob.ID)
	if err != nil || completedOldJob.Status != model.JobReady {
		t.Fatalf("superseded job = %#v, err = %v", completedOldJob, err)
	}
	current, err := db.GetDocument(t.Context(), first.ID, false)
	if err != nil || current.Revision != "r2" || current.Status != model.JobAccepted || current.BrainID != "" {
		t.Fatalf("current document after stale job = %#v, err = %v", current, err)
	}
	firstRevision, err := db.GetRevision(t.Context(), first.ID, 1)
	if err != nil || firstRevision.SnapshotOID != "" {
		t.Fatalf("stale job checkpointed revision one: %#v, err = %v", firstRevision, err)
	}
	secondRevision, err := db.GetRevision(t.Context(), first.ID, 2)
	if err != nil || secondRevision.SnapshotOID != "" {
		t.Fatalf("stale job wrote revision two checkpoint: %#v, err = %v", secondRevision, err)
	}
	if got := len(svc.documents.(*workflowDocuments).writeCalls()); got != 0 {
		t.Fatalf("stale queued job performed %d OpenViking writes, want zero", got)
	}
	if got := len(svc.graph.(*workflowGraph).ingestedDocuments()); got != 0 {
		t.Fatalf("stale queued job wrote %d Brain provenance records, want zero", got)
	}
}

func TestDocumentUpdateDuringSyncRejectsOldCheckpointAndProvenance(t *testing.T) {
	docs := &workflowDocuments{snapshotStarted: make(chan struct{}, 1), releaseSnapshot: make(chan struct{})}
	db, svc := newWorkflowService(t, ":memory:", docs, &workflowGraph{})
	first, firstJob, err := svc.CreateDocument(t.Context(), model.Document{
		ID: "concurrent-update", Title: "Concurrent update", Format: "markdown", Content: "revision one",
	})
	if err != nil {
		t.Fatal(err)
	}
	processed := make(chan error, 1)
	go func() {
		processed <- svc.processOne(t.Context())
	}()
	select {
	case <-docs.snapshotStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not reach the blocked OpenViking snapshot")
	}

	updated, _, err := svc.UpdateDocument(t.Context(), first.ID, first.ETag,
		"Concurrent update", "revision two", nil, nil)
	if err != nil {
		close(docs.releaseSnapshot)
		t.Fatal(err)
	}
	if updated.Revision != "r2" {
		close(docs.releaseSnapshot)
		t.Fatalf("current revision = %q, want r2", updated.Revision)
	}
	close(docs.releaseSnapshot)
	select {
	case err := <-processed:
		if err != nil {
			t.Fatalf("superseded sync returned an error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish after snapshot was released")
	}

	completedOldJob, err := db.GetJob(t.Context(), firstJob.ID)
	if err != nil || completedOldJob.Status != model.JobReady {
		t.Fatalf("superseded in-flight job = %#v, err = %v", completedOldJob, err)
	}
	current, err := db.GetDocument(t.Context(), first.ID, false)
	if err != nil || current.Revision != "r2" || current.Status != model.JobAccepted || current.BrainID != "" {
		t.Fatalf("current document after in-flight stale sync = %#v, err = %v", current, err)
	}
	for _, revisionNumber := range []int{1, 2} {
		revision, err := db.GetRevision(t.Context(), first.ID, revisionNumber)
		if err != nil || revision.SnapshotOID != "" {
			t.Fatalf("revision r%d checkpoint = %#v, err = %v; stale completion must not persist it",
				revisionNumber, revision, err)
		}
	}
	if got := len(svc.graph.(*workflowGraph).ingestedDocuments()); got != 0 {
		t.Fatalf("stale in-flight sync wrote %d Brain provenance records, want zero", got)
	}
}

func TestDocumentRetryKeepsCheckpointAfterRepeatedBrainFailure(t *testing.T) {
	docs := &workflowDocuments{}
	graph := &workflowGraph{failIngests: 2}
	db, svc := newWorkflowService(t, ":memory:", docs, graph)
	doc, job, err := svc.CreateDocument(t.Context(), model.Document{
		ID: "retry-checkpoint", Title: "Retry checkpoint", Format: "markdown", Content: "body",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.processOne(t.Context()); err == nil {
		t.Fatal("initial sync unexpectedly succeeded while Brain was unavailable")
	}
	firstRevision, err := db.GetRevision(t.Context(), doc.ID, 1)
	if err != nil || firstRevision.SnapshotOID == "" {
		t.Fatalf("first attempt checkpoint = %#v, err = %v", firstRevision, err)
	}
	if err := db.RetryJob(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err == nil {
		t.Fatal("first retry unexpectedly succeeded while Brain was unavailable")
	}
	partialJob, err := db.GetJob(t.Context(), job.ID)
	if err != nil || partialJob.Status != model.JobPartiallyReady {
		t.Fatalf("failed retry job = %#v, err = %v; a usable OpenViking checkpoint is partially ready", partialJob, err)
	}
	partialDoc, err := db.GetDocument(t.Context(), doc.ID, false)
	if err != nil || partialDoc.Status != model.JobPartiallyReady {
		t.Fatalf("document after failed retry = %#v, err = %v", partialDoc, err)
	}
	secondRevision, err := db.GetRevision(t.Context(), doc.ID, 1)
	if err != nil || secondRevision.SnapshotOID != firstRevision.SnapshotOID {
		t.Fatalf("retry changed the usable checkpoint from %q to %#v, err = %v",
			firstRevision.SnapshotOID, secondRevision, err)
	}
	if len(docs.writeCalls()) != 1 || docs.snapshotCount() != 1 {
		t.Fatalf("retry repeated OpenViking work: writes=%d snapshots=%d", len(docs.writeCalls()), docs.snapshotCount())
	}

	if err := db.RetryJob(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	completed, err := db.GetJob(t.Context(), job.ID)
	if err != nil || completed.Status != model.JobReady {
		t.Fatalf("completed retry = %#v, err = %v", completed, err)
	}
	if len(docs.writeCalls()) != 1 || docs.snapshotCount() != 1 {
		t.Fatalf("successful retry discarded the checkpoint: writes=%d snapshots=%d", len(docs.writeCalls()), docs.snapshotCount())
	}
}

func TestDocumentSyncRestartResumesStoredCheckpoint(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "restart.db")
	db, svc := newWorkflowService(t, dbPath, &workflowDocuments{}, &workflowGraph{})
	doc, job, err := svc.CreateDocument(t.Context(), model.Document{
		ID: "restart-checkpoint", Title: "Restart checkpoint", Format: "markdown", Content: "body",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := db.NextJob(t.Context())
	if err != nil || claimed.ID != job.ID {
		t.Fatalf("claimed job = %#v, err = %v", claimed, err)
	}
	if err := db.SetDocumentSync(t.Context(), doc.ID, string(model.JobExtracting), "", "snapshot-before-restart"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := recovered.Close(); err != nil {
			t.Errorf("close recovered store: %v", err)
		}
	})
	recoveredJob, err := recovered.GetJob(t.Context(), job.ID)
	if err != nil || recoveredJob.Status != model.JobAccepted {
		t.Fatalf("recovered job = %#v, err = %v", recoveredJob, err)
	}
	assertWorkflowPayloadRevision(t, recoveredJob.Payload, "r1")
	recoveredDoc, err := recovered.GetDocument(t.Context(), doc.ID, false)
	if err != nil || recoveredDoc.Status != model.JobExtracting {
		t.Fatalf("recovered document = %#v, err = %v", recoveredDoc, err)
	}

	resumeDocuments := &workflowDocuments{}
	resumeGraph := &workflowGraph{}
	resumed := New(recovered, resumeDocuments, resumeGraph, "https://memory.example.test",
		slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	if err := resumed.processOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(resumeDocuments.writeCalls()) != 0 || resumeDocuments.snapshotCount() != 0 {
		t.Fatalf("restart repeated checkpointed OpenViking work: writes=%d snapshots=%d",
			len(resumeDocuments.writeCalls()), resumeDocuments.snapshotCount())
	}
	completed, err := recovered.GetJob(t.Context(), job.ID)
	if err != nil || completed.Status != model.JobReady {
		t.Fatalf("restarted job = %#v, err = %v", completed, err)
	}
}

func TestRenameSyncProjectsGraphAgainstRenamedRevisionBeforeReady(t *testing.T) {
	graph := &workflowGraph{resultFor: func(doc upstream.GraphDocument) upstream.GraphIngestResult {
		return upstream.GraphIngestResult{
			DocumentID: "brain-" + doc.ID,
			OriginURI:  doc.OriginURI,
			Revision:   doc.Revision,
			EntityIDs:  []string{"brain-renamed-entity"},
			FactIDs:    []string{"brain-renamed-fact"},
			Entities: []upstream.GraphEntity{{
				BrainID: "brain-renamed-entity", Name: "Renamed record", Type: "decision",
			}},
			Facts: []upstream.GraphFact{{
				BrainID: "brain-renamed-fact", EntityBrainID: "brain-renamed-entity",
				Predicate: "retains", Object: "the renamed revision", Status: "active",
			}},
		}
	}}
	db, svc := newWorkflowService(t, ":memory:", &workflowDocuments{}, graph)
	doc, createJob, err := svc.CreateDocument(t.Context(), model.Document{
		ID: "rename-source", Title: "Rename source", Format: "markdown", Content: "rename-source keeps this rule",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetDocumentSyncForRevision(t.Context(), doc.ID, "r1", model.JobReady, "brain-original", "snapshot-original"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobStatus(t.Context(), createJob.ID, model.JobReady, nil); err != nil {
		t.Fatal(err)
	}
	plan, err := svc.CreateRenamePlan(t.Context(), []model.RenameOperation{{
		ResourceType: "document", From: "rename-source", To: "renamed-source",
	}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.ApplyRenamePlan(t.Context(), plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	renamed, err := db.GetDocument(t.Context(), "renamed-source", true)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Revision != "r2" || renamed.Status != model.JobReady {
		t.Fatalf("renamed document = %#v, want ready r2", renamed)
	}
	facts, err := db.ListFacts(t.Context(), "renamed-record", 10)
	if err != nil || len(facts) != 1 {
		t.Fatalf("renamed graph facts = %#v, err = %v", facts, err)
	}
	if facts[0].SourceDocumentID != renamed.ID || facts[0].SourceRevision != renamed.Revision {
		t.Fatalf("renamed graph provenance = %#v, want %s@%s", facts[0], renamed.ID, renamed.Revision)
	}
	if got := graph.ingestedDocuments(); len(got) != 1 ||
		got[0].OriginURI != "manifold://documents/renamed-source@r2" || got[0].Revision != "r2" {
		t.Fatalf("rename graph ingest source = %#v, want renamed-source@r2", got)
	}
	completed, err := db.GetJob(t.Context(), job.ID)
	if err != nil || completed.Status != model.JobReady {
		t.Fatalf("rename job = %#v, err = %v", completed, err)
	}
}

func TestRenameRollbackReprojectsTheRestoredDocumentRevision(t *testing.T) {
	graph := &workflowGraph{
		failIngestAt: 2,
		resultFor: func(doc upstream.GraphDocument) upstream.GraphIngestResult {
			entityID := "brain-entity-" + doc.ID
			factID := "brain-fact-" + doc.ID
			return upstream.GraphIngestResult{
				DocumentID: "brain-document-" + doc.ID,
				OriginURI:  doc.OriginURI,
				Revision:   doc.Revision,
				EntityIDs:  []string{entityID},
				FactIDs:    []string{factID},
				Entities:   []upstream.GraphEntity{{BrainID: entityID, Name: "Record " + doc.ID, Type: "decision"}},
				Facts: []upstream.GraphFact{{
					BrainID: factID, EntityBrainID: entityID, Predicate: "keeps", Object: doc.ID, Status: "active",
				}},
			}
		},
	}
	db, svc := newWorkflowService(t, ":memory:", &workflowDocuments{}, graph)
	for _, id := range []string{"rollback-a", "rollback-b"} {
		doc, job, err := svc.CreateDocument(t.Context(), model.Document{
			ID: id, Title: id, Format: "markdown", Content: id + " keeps its rule",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetDocumentSyncForRevision(t.Context(), doc.ID, doc.Revision, model.JobReady, "brain-original-"+id, "snapshot-original"); err != nil {
			t.Fatal(err)
		}
		if err := db.SetJobStatus(t.Context(), job.ID, model.JobReady, nil); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := svc.CreateRenamePlan(t.Context(), []model.RenameOperation{
		{ResourceType: "document", From: "rollback-a", To: "renamed-a"},
		{ResourceType: "document", From: "rollback-b", To: "renamed-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.ApplyRenamePlan(t.Context(), plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err == nil {
		t.Fatal("rename unexpectedly succeeded after the second Brain ingest failed")
	}
	failedPlan, err := db.GetRenamePlan(t.Context(), plan.ID)
	if err != nil || failedPlan.Status != "failed" {
		t.Fatalf("rename plan after compensation = %#v, err = %v", failedPlan, err)
	}
	for _, id := range []string{"rollback-a", "rollback-b"} {
		doc, err := db.GetDocument(t.Context(), id, true)
		if err != nil || doc.Revision != "r1" || doc.Status != model.JobReady {
			t.Fatalf("restored document %q = %#v, err = %v", id, doc, err)
		}
		facts, err := db.ListFacts(t.Context(), "record-"+id, 10)
		if err != nil || len(facts) != 1 || facts[0].SourceDocumentID != id || facts[0].SourceRevision != "r1" {
			t.Fatalf("restored graph source for %q = %#v, err = %v", id, facts, err)
		}
	}
	completed, err := db.GetJob(t.Context(), job.ID)
	if err != nil || completed.Status != model.JobFailed {
		t.Fatalf("failed rename job = %#v, err = %v", completed, err)
	}
}

func TestRenameReservationsRejectConcurrentDocumentMutations(t *testing.T) {
	for _, operation := range []string{"update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			db, svc := newWorkflowService(t, ":memory:", &workflowDocuments{}, &workflowGraph{})
			doc, _, err := svc.CreateDocument(t.Context(), model.Document{
				ID: "reserved-document", Title: "Reserved document", Format: "markdown", Content: "original",
			})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := svc.CreateRenamePlan(t.Context(), []model.RenameOperation{{
				ResourceType: "document", From: doc.ID, To: "renamed-document",
			}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.ApplyRenamePlan(t.Context(), plan.ID); err != nil {
				t.Fatal(err)
			}

			switch operation {
			case "update":
				_, _, err = svc.UpdateDocument(t.Context(), doc.ID, doc.ETag, "Updated", "changed", nil, nil)
			case "delete":
				_, err = svc.DeleteDocument(t.Context(), doc.ID, doc.ETag)
			}
			if !errors.Is(err, store.ErrConflict) {
				t.Fatalf("%s during queued rename error = %v, want ErrConflict", operation, err)
			}
			current, getErr := db.GetDocument(t.Context(), doc.ID, true)
			if getErr != nil || current.Revision != doc.Revision || current.Content != doc.Content {
				t.Fatalf("document after rejected %s = %#v, err = %v", operation, current, getErr)
			}
		})
	}
}

func newWorkflowService(
	t *testing.T,
	dbPath string,
	documents *workflowDocuments,
	graph upstream.GraphStore,
) (*store.Store, *Service) {
	t.Helper()
	db, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close workflow store: %v", err)
		}
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return db, New(db, documents, graph, "https://memory.example.test", logger, time.Millisecond)
}

func setWorkflowJobCreatedAt(dbPath, jobID, timestamp string) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`UPDATE jobs SET created_at = ? WHERE id = ?`, timestamp, jobID)
	return err
}

func assertWorkflowPayloadRevision(t *testing.T, payload []byte, want string) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("decode job payload %s: %v", payload, err)
	}
	var revision string
	if err := json.Unmarshal(fields["revision"], &revision); err != nil {
		t.Fatalf("decode job payload revision %s: %v", payload, err)
	}
	if revision != want {
		t.Fatalf("job payload revision = %q in %s, want %q", revision, payload, want)
	}
}

func assertWorkflowPayloadField(t *testing.T, payload []byte, key, want string) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("decode job payload %s: %v", payload, err)
	}
	var got string
	if err := json.Unmarshal(fields[key], &got); err != nil {
		t.Fatalf("decode payload field %q in %s: %v", key, payload, err)
	}
	if got != want {
		t.Fatalf("payload field %q = %q in %s, want %q", key, got, payload, want)
	}
}

type workflowDocuments struct {
	mu              sync.Mutex
	writes          []workflowWrite
	snapshots       int
	writeErr        error
	snapshotStarted chan struct{}
	releaseSnapshot chan struct{}
}

type workflowWrite struct {
	URI     string
	Content string
	Create  bool
}

func (d *workflowDocuments) Health(context.Context) error { return nil }

func (d *workflowDocuments) Exists(context.Context, string) (bool, error) { return false, nil }

func (d *workflowDocuments) Mkdir(context.Context, string, string) error { return nil }

func (d *workflowDocuments) Write(_ context.Context, uri, content string, create bool) error {
	d.mu.Lock()
	d.writes = append(d.writes, workflowWrite{URI: uri, Content: content, Create: create})
	err := d.writeErr
	d.mu.Unlock()
	return err
}

func (d *workflowDocuments) Move(context.Context, string, string) error { return nil }

func (d *workflowDocuments) Delete(context.Context, string, bool) error { return nil }

func (d *workflowDocuments) Snapshot(ctx context.Context, _ string, _ []string) (string, error) {
	d.mu.Lock()
	d.snapshots++
	count := d.snapshots
	d.mu.Unlock()
	if d.snapshotStarted != nil {
		select {
		case d.snapshotStarted <- struct{}{}:
		default:
		}
	}
	if d.releaseSnapshot != nil {
		select {
		case <-d.releaseSnapshot:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return fmt.Sprintf("snapshot-%d", count), nil
}

func (d *workflowDocuments) Search(context.Context, string, string, int) ([]model.SearchHit, error) {
	return nil, nil
}

func (d *workflowDocuments) writeCalls() []workflowWrite {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]workflowWrite(nil), d.writes...)
}

func (d *workflowDocuments) snapshotCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshots
}

type workflowGraph struct {
	mu           sync.Mutex
	ingests      []upstream.GraphDocument
	failIngests  int
	failIngestAt int
	resultFor    func(upstream.GraphDocument) upstream.GraphIngestResult
	// started, when set, receives a signal as an ingest begins; block keeps
	// the ingest waiting until its context ends.
	started chan struct{}
	block   bool
}

func (g *workflowGraph) Health(context.Context) error { return nil }

func (g *workflowGraph) IngestDocument(ctx context.Context, doc upstream.GraphDocument) (upstream.GraphIngestResult, error) {
	if g.started != nil {
		select {
		case g.started <- struct{}{}:
		default:
		}
	}
	if g.block {
		<-ctx.Done()
		return upstream.GraphIngestResult{}, ctx.Err()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ingests = append(g.ingests, doc)
	if len(g.ingests) <= g.failIngests || len(g.ingests) == g.failIngestAt {
		return upstream.GraphIngestResult{}, errors.New("graph ingest failed")
	}
	if g.resultFor != nil {
		return g.resultFor(doc), nil
	}
	return upstream.GraphIngestResult{
		DocumentID: "brain-" + doc.ID, OriginURI: doc.OriginURI, Revision: doc.Revision,
	}, nil
}

func (g *workflowGraph) Search(context.Context, string, int, bool) ([]model.SearchHit, error) {
	return nil, nil
}

func (g *workflowGraph) IngestFact(context.Context, model.Fact, model.Entity) (string, string, error) {
	return "", "", nil
}

func (g *workflowGraph) IngestRelation(context.Context, model.Relation, model.Entity, model.Entity) (string, error) {
	return "", nil
}

func (g *workflowGraph) RetractFact(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (g *workflowGraph) ingestedDocuments() []upstream.GraphDocument {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]upstream.GraphDocument(nil), g.ingests...)
}

func TestRenameCancellationRestartsFromSnapshotCheckpoint(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rename-checkpoint.sqlite")
	documents := &workflowDocuments{}
	firstGraph := &workflowGraph{started: make(chan struct{}, 1), block: true}
	db, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	firstStoreOpen := true
	t.Cleanup(func() {
		if firstStoreOpen {
			if err := db.Close(); err != nil {
				t.Errorf("close first rename store: %v", err)
			}
		}
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	firstService := New(db, documents, firstGraph, "https://memory.example.test", logger, time.Millisecond)

	doc, createJob, err := firstService.CreateDocument(t.Context(), model.Document{
		ID: "rename-checkpoint", Title: "Rename checkpoint", Format: "markdown",
		Content: "rename-checkpoint remains durable",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetDocumentSyncForRevision(t.Context(), doc.ID, doc.Revision, model.JobReady,
		"graph-original", "snapshot-original"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobStatus(t.Context(), createJob.ID, model.JobReady, nil); err != nil {
		t.Fatal(err)
	}
	plan, err := firstService.CreateRenamePlan(t.Context(), []model.RenameOperation{{
		ResourceType: "document", From: doc.ID, To: "renamed-checkpoint",
	}})
	if err != nil {
		t.Fatal(err)
	}
	renameJob, err := firstService.ApplyRenamePlan(t.Context(), plan.ID)
	if err != nil {
		t.Fatal(err)
	}

	workerCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- firstService.processOne(workerCtx) }()
	select {
	case <-firstGraph.started:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("rename did not start extracting the rewritten document")
	}
	revision, err := db.GetRevision(t.Context(), "renamed-checkpoint", 2)
	if err != nil || revision.SnapshotOID == "" {
		cancel()
		<-done
		t.Fatalf("rename checkpoint was not persisted before extraction: %#v, %v", revision, err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled rename worker error = %v, want context.Canceled", err)
	}
	if got := documents.snapshotCount(); got != 1 {
		t.Fatalf("snapshot count before restart = %d, want one checkpoint", got)
	}
	if got := len(documents.writeCalls()); got != 1 {
		t.Fatalf("write count before restart = %d, want one write", got)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	firstStoreOpen = false

	recovered, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := recovered.Close(); err != nil {
			t.Errorf("close recovered rename store: %v", err)
		}
	})
	recoveredJob, err := recovered.GetJob(t.Context(), renameJob.ID)
	if err != nil || recoveredJob.Status != model.JobAccepted {
		t.Fatalf("rename job after restart = %#v, err = %v", recoveredJob, err)
	}
	secondGraph := &workflowGraph{}
	resumedService := New(recovered, documents, secondGraph, "https://memory.example.test", logger, time.Millisecond)
	if err := resumedService.processOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	completed, err := recovered.GetJob(t.Context(), renameJob.ID)
	if err != nil || completed.Status != model.JobReady {
		t.Fatalf("rename job after checkpoint recovery = %#v, err = %v", completed, err)
	}
	plan, err = recovered.GetRenamePlan(t.Context(), plan.ID)
	if err != nil || plan.Status != "applied" {
		t.Fatalf("rename plan after checkpoint recovery = %#v, err = %v", plan, err)
	}
	renamed, err := recovered.GetDocument(t.Context(), "renamed-checkpoint", true)
	if err != nil || renamed.Revision != "r2" || renamed.Status != model.JobReady {
		t.Fatalf("renamed document after checkpoint recovery = %#v, err = %v", renamed, err)
	}
	if got := len(secondGraph.ingestedDocuments()); got != 1 {
		t.Fatalf("ingests after restart = %d, want one", got)
	}
	if got := documents.snapshotCount(); got != 1 {
		t.Fatalf("snapshot count after restart = %d, want persisted checkpoint reuse", got)
	}
	if got := len(documents.writeCalls()); got != 1 {
		t.Fatalf("write count after restart = %d, want persisted checkpoint reuse", got)
	}
}
