package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/model"
)

func TestDocumentSyncJobsBindTheirExactRevision(t *testing.T) {
	s := openTestStore(t)
	created, firstJob, err := s.CreateDocument(t.Context(), model.Document{
		ID: "workflow-memory", Title: "Workflow memory", Format: "markdown", Content: "revision one",
		OVURI: "viking://resources/manifold/workflow-memory.md",
	}, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "workflow-memory",
		Payload: json.RawMessage(`{"create":true,"folder_paths":["shared"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != "r1" {
		t.Fatalf("created revision = %q, want r1", created.Revision)
	}
	assertPayloadRevision(t, firstJob.Payload, "r1")
	if got, err := s.GetJob(t.Context(), firstJob.ID); err != nil {
		t.Fatal(err)
	} else {
		assertPayloadRevision(t, got.Payload, "r1")
	}

	updated, secondJob, err := s.UpdateDocument(t.Context(), created.ID, created.ETag,
		"Workflow memory", "revision two", nil, nil, model.Job{
			ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: created.ID,
			Payload: json.RawMessage(`{"create":false}`),
		})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != "r2" {
		t.Fatalf("updated revision = %q, want r2", updated.Revision)
	}
	assertPayloadRevision(t, secondJob.Payload, "r2")
	if got, err := s.GetJob(t.Context(), firstJob.ID); err != nil {
		t.Fatal(err)
	} else {
		assertPayloadRevision(t, got.Payload, "r1")
	}
}

func TestLegacyDocumentSyncJobRecoversUniqueRevisionFromCreatedAt(t *testing.T) {
	s := openTestStore(t)
	doc, job, err := s.CreateDocument(t.Context(), model.Document{
		ID: "legacy-revision", Title: "Legacy revision", Format: "markdown", Content: "body",
		OVURI: "viking://resources/manifold/legacy-revision.md",
	}, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "legacy-revision",
		Payload: json.RawMessage(`{"create":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `UPDATE jobs SET payload_json = '{"create":true}' WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}
	legacy, err := s.GetJob(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := s.ResolveDocumentSyncRevision(t.Context(), legacy)
	if err != nil || revision != doc.Revision {
		t.Fatalf("legacy revision = %q, err = %v; want exact original revision %q", revision, err, doc.Revision)
	}
	resolved, err := s.GetJob(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertPayloadRevision(t, resolved.Payload, "r1")
}

func TestLegacyDocumentSyncJobDoesNotGuessAmbiguousRevision(t *testing.T) {
	s := openTestStore(t)
	doc, firstJob, err := s.CreateDocument(t.Context(), model.Document{
		ID: "ambiguous-legacy", Title: "Ambiguous legacy", Format: "markdown", Content: "revision one",
		OVURI: "viking://resources/manifold/ambiguous-legacy.md",
	}, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "ambiguous-legacy",
		Payload: json.RawMessage(`{"create":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpdateDocument(t.Context(), doc.ID, doc.ETag, "Ambiguous legacy", "revision two", nil, nil, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: doc.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `
		UPDATE revisions SET created_at = (SELECT created_at FROM jobs WHERE id = ?) WHERE document_id = ? AND revision = 2`,
		firstJob.ID, doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `UPDATE jobs SET payload_json = '{}' WHERE id = ?`, firstJob.ID); err != nil {
		t.Fatal(err)
	}
	legacy, err := s.GetJob(t.Context(), firstJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveDocumentSyncRevision(t.Context(), legacy); !errors.Is(err, ErrDocumentSyncRevisionUnknown) {
		t.Fatalf("ambiguous legacy revision = %v, want ErrDocumentSyncRevisionUnknown", err)
	}
}

func TestSetJobStatusReportsMissingJob(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetJobStatus(t.Context(), "missing-job", model.JobReady, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetJobStatus on a missing job = %v, want ErrNotFound", err)
	}
}

func TestDocumentSyncCompletionCannotResurrectDeletedDocument(t *testing.T) {
	s := openTestStore(t)
	doc, _, err := s.CreateDocument(t.Context(), model.Document{
		ID: "deleted-during-sync", Title: "Deleted during sync", Format: "markdown", Content: "body",
		OVURI: "viking://resources/manifold/deleted-during-sync.md",
	}, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "deleted-during-sync",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDocumentDeleted(t.Context(), doc.ID, doc.ETag, model.Job{
		ID: identity.NewXID(), Kind: "document.delete", ResourceType: "document", ResourceID: doc.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDocumentSyncForRevision(t.Context(), doc.ID, "r1", model.JobReady, "late-brain-id", "late-snapshot"); !errors.Is(err, ErrDocumentSyncSuperseded) {
		t.Fatalf("completion after deletion = %v, want ErrDocumentSyncSuperseded", err)
	}
	var deleted int
	var status, brainID string
	if err := s.db.QueryRowContext(t.Context(), `SELECT deleted, status, brain_id FROM documents WHERE id = ?`, doc.ID).
		Scan(&deleted, &status, &brainID); err != nil {
		t.Fatal(err)
	}
	if deleted != 1 || status != string(model.JobAccepted) || brainID != "" {
		t.Fatalf("document after stale completion: deleted=%d status=%q brain_id=%q", deleted, status, brainID)
	}
	revision, err := s.GetRevision(t.Context(), doc.ID, 1)
	if err != nil || revision.SnapshotOID != "" {
		t.Fatalf("revision after stale completion = %#v, err = %v", revision, err)
	}
}

func TestCreateTransactionsRejectReservedRenameSlugs(t *testing.T) {
	t.Run("document ID after rename apply", func(t *testing.T) {
		s := openTestStore(t)
		_, _, err := s.CreateDocument(t.Context(), model.Document{
			ID: "reserved-document-source", Title: "Original", Format: "markdown", Content: "body",
		}, model.Job{ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "reserved-document-source"})
		if err != nil {
			t.Fatal(err)
		}
		applyStoreRenameForTest(t, s, "document", "reserved-document-source", "renamed-document-target")
		_, _, err = s.CreateDocument(t.Context(), model.Document{
			ID: "reserved-document-source", Title: "Late create", Format: "markdown", Content: "late body",
		}, model.Job{ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "reserved-document-source"})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("document create into a reserved source slug = %v, want ErrConflict", err)
		}
	})

	t.Run("folder ID after rename apply", func(t *testing.T) {
		s := openTestStore(t)
		if _, err := s.CreateFolder(t.Context(), model.Folder{ID: "reserved-folder-source", Name: "Original"}); err != nil {
			t.Fatal(err)
		}
		applyStoreRenameForTest(t, s, "folder", "reserved-folder-source", "renamed-folder-target")
		if _, err := s.CreateFolder(t.Context(), model.Folder{ID: "reserved-folder-source", Name: "Late create"}); !errors.Is(err, ErrConflict) {
			t.Fatalf("folder create into a reserved source slug = %v, want ErrConflict", err)
		}
	})

	t.Run("document path through reserved target folder", func(t *testing.T) {
		s := openTestStore(t)
		if _, err := s.CreateFolder(t.Context(), model.Folder{ID: "path-source-folder", Name: "Original"}); err != nil {
			t.Fatal(err)
		}
		applyStoreRenameForTest(t, s, "folder", "path-source-folder", "path-target-folder")
		_, _, _, err := s.CreateDocumentAtPath(t.Context(), []string{"path-target-folder"}, model.Document{
			ID: "late-path-document", Title: "Late path document", Format: "markdown", Content: "body",
		}, model.Job{ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "late-path-document"})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("document create through a reserved target folder = %v, want ErrConflict", err)
		}
	})
}

func applyStoreRenameForTest(t *testing.T, s *Store, resourceType, from, to string) {
	t.Helper()
	plan, err := s.CreateRenamePlan(t.Context(), identity.NewXID(), []model.RenameOperation{{
		ResourceType: resourceType, From: from, To: to,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueRename(t.Context(), plan, model.Job{
		ID: identity.NewXID(), Kind: "rename.apply", ResourceType: "rename_plan", ResourceID: plan.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyRename(t.Context(), plan.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRetryDocumentSyncPersistsAndPreservesRetryRequest(t *testing.T) {
	s := openTestStore(t)
	_, job, err := s.CreateDocument(t.Context(), model.Document{
		ID: "retry-request", Title: "Retry request", Format: "markdown", Content: "body",
		OVURI: "viking://resources/manifold/retry-request.md",
	}, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "retry-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetJobStatus(t.Context(), job.ID, model.JobPartiallyReady, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryJob(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	retried, err := s.GetJob(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertPayloadBool(t, retried.Payload, "manual_retry_requested", true)
	if err := s.MergeJobPayload(t.Context(), job.ID, map[string]any{"brain_retry_key": "manifold-job-attempt-2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetJobStatus(t.Context(), job.ID, model.JobPartiallyReady, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryJob(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	retried, err = s.GetJob(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertPayloadBool(t, retried.Payload, "manual_retry_requested", true)
	assertPayloadString(t, retried.Payload, "brain_retry_key", "manifold-job-attempt-2")
}

func TestRenewJobLeaseExtendsOnlyTheCurrentAttempt(t *testing.T) {
	s := openTestStore(t)
	_, job, err := s.CreateDocument(t.Context(), model.Document{
		ID: "leased-document", Title: "Leased document", Format: "markdown", Content: "body",
		OVURI: "viking://resources/manifold/leased-document.md",
	}, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "leased-document",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.NextJob(t.Context())
	if err != nil || claimed.ID != job.ID {
		t.Fatalf("claimed job = %#v, err = %v", claimed, err)
	}
	var initialLease string
	if err := s.db.QueryRowContext(t.Context(), `SELECT lease_until FROM jobs WHERE id = ?`, job.ID).Scan(&initialLease); err != nil {
		t.Fatal(err)
	}
	initial, err := time.Parse(time.RFC3339Nano, initialLease)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RenewJobLease(t.Context(), job.ID, claimed.Attempts, time.Hour); err != nil {
		t.Fatal(err)
	}
	var renewedLease string
	if err := s.db.QueryRowContext(t.Context(), `SELECT lease_until FROM jobs WHERE id = ?`, job.ID).Scan(&renewedLease); err != nil {
		t.Fatal(err)
	}
	renewed, err := time.Parse(time.RFC3339Nano, renewedLease)
	if err != nil {
		t.Fatal(err)
	}
	if !renewed.After(initial) {
		t.Fatalf("renewed lease %s did not extend initial lease %s", renewed, initial)
	}
	if _, err := s.db.ExecContext(t.Context(), `UPDATE jobs SET attempts = attempts + 1 WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RenewJobLease(t.Context(), job.ID, claimed.Attempts, time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale attempt lease renewal = %v, want ErrConflict", err)
	}
}

func assertPayloadRevision(t *testing.T, payload json.RawMessage, want string) {
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

func assertPayloadBool(t *testing.T, payload json.RawMessage, key string, want bool) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("decode job payload %s: %v", payload, err)
	}
	var got bool
	if err := json.Unmarshal(fields[key], &got); err != nil {
		t.Fatalf("decode payload field %q in %s: %v", key, payload, err)
	}
	if got != want {
		t.Fatalf("payload field %q = %t in %s, want %t", key, got, payload, want)
	}
}

func assertPayloadString(t *testing.T, payload json.RawMessage, key, want string) {
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
