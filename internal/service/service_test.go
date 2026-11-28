package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iamwavecut/Manifold/internal/engine"
	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/llm"
	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/store"
	"github.com/iamwavecut/Manifold/internal/upstream"
)

func TestReciprocalRankFusionRewardsAgreement(t *testing.T) {
	t.Parallel()

	first := []model.SearchHit{
		{Kind: "document", ID: "alpha"},
		{Kind: "document", ID: "beta"},
	}
	second := []model.SearchHit{
		{Kind: "document", ID: "beta"},
		{Kind: "fact", ID: "gamma"},
	}
	result := reciprocalRankFusion([][]model.SearchHit{first, second}, 3)
	if len(result) != 3 {
		t.Fatalf("result length = %d, want 3", len(result))
	}
	if result[0].ID != "beta" {
		t.Fatalf("source agreement should rank beta first: %#v", result)
	}
	if result[1].Score <= 0 || result[2].Score <= 0 {
		t.Fatalf("fused scores must be positive: %#v", result)
	}
}

func TestTokenEstimateUsesRunesAndNeverReturnsZeroForText(t *testing.T) {
	t.Parallel()

	if got := estimateTokens("abc"); got != 1 {
		t.Fatalf("estimateTokens(abc) = %d, want 1", got)
	}
	if got := estimateTokens("память"); got != 2 {
		t.Fatalf("estimateTokens(память) = %d, want 2", got)
	}
	if got := estimateTokens(""); got != 0 {
		t.Fatalf("estimateTokens(empty) = %d, want 0", got)
	}
}

func TestSearchModesAndScopeGlobUseCanonicalPaths(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := New(db, fakeDocumentStore{}, fakeGraphStore{}, "https://memory.example.test", logger, time.Millisecond)
	for _, item := range []struct {
		id      string
		path    string
		content string
	}{
		{id: "shared-policy", path: "shared/agent-practice", content: "canonical memory workflow"},
		{id: "task-note", path: "tasks/incident-42", content: "canonical memory workflow"},
	} {
		if _, _, _, err := svc.CreateDocumentAtPath(t.Context(), item.path, model.Document{
			ID: item.id, Title: item.id, Format: "markdown", Content: item.content,
		}); err != nil {
			t.Fatal(err)
		}
	}

	lexical, err := svc.Search(t.Context(), SearchRequest{
		Query: "canonical memory", Mode: model.SearchLexical, ScopeGlob: "shared/**", Limit: 10,
		SourceTypes: []string{"document"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lexical.Items) != 1 || lexical.Items[0].ID != "shared-policy" ||
		lexical.Items[0].Path != "shared/agent-practice/shared-policy" {
		t.Fatalf("scoped lexical search = %#v", lexical)
	}

	semantic, err := svc.Search(t.Context(), SearchRequest{
		Query: "canonical memory", Mode: model.SearchSemantic, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(semantic.Items) != 0 {
		t.Fatalf("semantic mode leaked lexical results: %#v", semantic)
	}
}

func TestDocumentRetryResumesAfterIndexCheckpoint(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	model0 := &testModel{chatErr: &llm.Error{Operation: "chat completion", Status: http.StatusBadRequest}, failChats: 1}
	svc, documents := newEngineService(t, db, newTestEngine(t, model0))
	_, job, err := svc.CreateDocument(t.Context(), model.Document{
		ID: "retry-memory", Title: "Retry memory", Format: "markdown", Content: "Resume from the durable checkpoint.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err == nil {
		t.Fatal("initial sync unexpectedly succeeded while extraction failed")
	}
	partiallyReady, err := db.GetJob(t.Context(), job.ID)
	if err != nil || partiallyReady.Status != model.JobPartiallyReady || partiallyReady.Error == nil ||
		partiallyReady.Error.Code != "dependency_unavailable" {
		t.Fatalf("initial job = %#v, err = %v", partiallyReady, err)
	}
	revision, err := db.GetRevision(t.Context(), "retry-memory", 1)
	if err != nil || !strings.HasPrefix(revision.SnapshotOID, "local:") {
		t.Fatalf("checkpoint revision = %#v, err = %v", revision, err)
	}

	if err := db.RetryJob(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	completed, err := db.GetJob(t.Context(), job.ID)
	if err != nil || completed.Status != model.JobReady {
		t.Fatalf("retried job = %#v, err = %v", completed, err)
	}
	document, err := db.GetDocument(t.Context(), "retry-memory", true)
	if err != nil || document.Status != model.JobReady || document.BrainID != "retry-memory" {
		t.Fatalf("retried document = %#v, err = %v", document, err)
	}
	_, writes, snapshots := documents.recorded()
	if len(writes) != 1 || snapshots != 1 || model0.chatCalls.Load() != 2 {
		t.Fatalf("calls: writes=%v snapshots=%d extractions=%d", writes, snapshots, model0.chatCalls.Load())
	}
}

func TestDocumentRetryReconcilesCreateBeforeCheckpoint(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	index := newTestEngine(t, &testModel{})
	svc, documents := newEngineService(t, db, index)
	documents.snapshotFailures = 1
	_, job, err := svc.CreateDocument(t.Context(), model.Document{
		ID: "retry-before-checkpoint", Title: "Retry before checkpoint", Format: "markdown",
		Content: "The canonical body must win when create is reconciled.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err == nil {
		t.Fatal("initial sync unexpectedly succeeded while snapshots were unavailable")
	}
	failed, err := db.GetJob(t.Context(), job.ID)
	if err != nil || failed.Status != model.JobFailed {
		t.Fatalf("initial job = %#v, err = %v", failed, err)
	}

	if err := db.RetryJob(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	completed, err := db.GetJob(t.Context(), job.ID)
	if err != nil || completed.Status != model.JobReady {
		t.Fatalf("retried job = %#v, err = %v", completed, err)
	}
	_, writes, snapshots := documents.recorded()
	if !slices.Equal(writes, []string{"create", "create", "replace"}) {
		t.Fatalf("write modes = %#v, want create/create/replace reconciliation", writes)
	}
	if snapshots != 2 {
		t.Fatalf("snapshot attempts = %d, want 2", snapshots)
	}
}

func retentionExtraction() engine.Extraction {
	return engine.Extraction{
		Entities: []engine.ExtractedEntity{{Name: "Retention", Type: "topic"}},
		Facts: []engine.ExtractedFact{{
			Entity: 0, Predicate: "retention", Value: "canonical", Confidence: 1, Evidence: "decision is canonical",
		}},
	}
}

func TestPipelineAndInterruptedRenameResumeEndToEnd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "manifold.db")
	db, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	index := newTestEngine(t, &testModel{extractions: map[string]engine.Extraction{
		"decision is canonical": retentionExtraction(),
	}})
	svc, documents := newEngineService(t, db, index)

	_, initialJob, err := svc.CreateDocument(t.Context(), model.Document{
		ID: "old-memory", Title: "Old memory", Format: "markdown",
		Content: "The old-memory decision is canonical.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	completed, err := db.GetJob(t.Context(), initialJob.ID)
	if err != nil || completed.Status != model.JobReady {
		t.Fatalf("initial job = %#v, err = %v", completed, err)
	}
	if _, err := index.EmbedPending(t.Context()); err != nil {
		t.Fatal(err)
	}

	search, err := svc.Search(t.Context(), SearchRequest{
		Query: "canonical", Mode: model.SearchHybrid, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Items) != 2 || len(search.DegradedDependencies) != 0 {
		t.Fatalf("hybrid search = %#v", search)
	}
	for _, hit := range search.Items {
		if hit.Path != "old-memory" || hit.Revision != "r1" || !strings.HasPrefix(hit.CanonicalRef, "manifold://") {
			t.Fatalf("hybrid search exposed a non-canonical hit: %#v", hit)
		}
		if hit.Kind == "document" && (hit.ID != "old-memory" || hit.CanonicalRef != "manifold://documents/old-memory@r1") {
			t.Fatalf("document mapping = %#v", hit)
		}
		if hit.Kind == "fact" && (!identity.IsXID(hit.ID) || hit.CanonicalRef != "manifold://facts/"+hit.ID) {
			t.Fatalf("fact mapping = %#v", hit)
		}
	}
	semantic, err := svc.Search(t.Context(), SearchRequest{Query: "old-memory decision", Mode: model.SearchSemantic, Limit: 10})
	if err != nil || len(semantic.DegradedDependencies) != 0 {
		t.Fatalf("semantic search = %#v, %v", semantic, err)
	}
	if !slices.ContainsFunc(semantic.Items, func(hit model.SearchHit) bool { return hit.Kind == "document" && hit.ID == "old-memory" }) {
		t.Fatalf("semantic search did not find the document by its vector: %#v", semantic.Items)
	}
	contextPack, degraded, err := svc.Context(t.Context(), SearchRequest{
		Query: "canonical", Mode: model.SearchHybrid, Limit: 10,
	}, 200)
	if err != nil || len(degraded) != 0 || contextPack.EstimatedTokens == 0 {
		t.Fatalf("context = %#v degraded=%v err=%v", contextPack, degraded, err)
	}

	plan, err := svc.CreateRenamePlan(t.Context(), []model.RenameOperation{{
		ResourceType: "document", From: "old-memory", To: "new-memory",
	}})
	if err != nil {
		t.Fatal(err)
	}
	renameJob, err := svc.ApplyRenamePlan(t.Context(), plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := db.NextJob(t.Context())
	if err != nil || claimed.ID != renameJob.ID {
		t.Fatalf("claimed job = %#v, err = %v", claimed, err)
	}
	if _, err := db.ApplyRename(t.Context(), plan.ID); err != nil {
		t.Fatal(err)
	}
	afterLocalRename, err := db.ListAllDocuments(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.syncRenamedResources(t.Context(), plan.ID, buildDocumentMoves(
		plan.ID,
		afterLocalRename,
		map[string]string{"new-memory": "viking://resources/manifold/old-memory.md"},
		map[string]string{"new-memory": "viking://resources/manifold/new-memory.md"},
	)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	resumedJob, err := recovered.GetJob(t.Context(), renameJob.ID)
	if err != nil || resumedJob.Status != model.JobAccepted {
		t.Fatalf("recovered job = %#v, err = %v", resumedJob, err)
	}
	resumed := New(recovered, documents, index.Graph(), "https://memory.example.test",
		slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	if err := resumed.processOne(t.Context()); err != nil {
		t.Fatal(err)
	}

	renamed, err := recovered.GetDocument(t.Context(), "new-memory", true)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Revision != "r2" || renamed.Content != "The new-memory decision is canonical." ||
		renamed.Status != model.JobReady {
		t.Fatalf("renamed document = %#v", renamed)
	}
	appliedPlan, err := recovered.GetRenamePlan(t.Context(), plan.ID)
	if err != nil || appliedPlan.Status != "applied" {
		t.Fatalf("rename plan = %#v, err = %v", appliedPlan, err)
	}
	moves, _, _ := documents.recorded()
	stageDocument := "viking://resources/manifold/.manifold-renames/" + plan.ID + "/documents/0.md"
	for _, expected := range []string{
		"viking://resources/manifold/old-memory.md -> " + stageDocument,
		stageDocument + " -> viking://resources/manifold/new-memory.md",
	} {
		if !slices.Contains(moves, expected) {
			t.Fatalf("index moves = %#v, missing %q", moves, expected)
		}
	}
	if len(moves) != 2 {
		t.Fatalf("resumed rename repeated completed index moves: %#v", moves)
	}
	if exists, _ := index.Documents().Exists(t.Context(), "viking://resources/manifold/new-memory.md"); !exists {
		t.Fatal("renamed document is missing from the index")
	}
	if !identity.IsXID(renameJob.ID) {
		t.Fatalf("rename job ID = %q, want XID", renameJob.ID)
	}
}

func TestRenameDependencyFailureRestoresIDsAndIndexPath(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	initialJob := model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "old-memory",
	}
	if _, _, err := db.CreateDocument(t.Context(), model.Document{
		ID: "old-memory", Title: "Old memory", Format: "markdown",
		Content: "old-memory", OVURI: "viking://resources/manifold/old-memory.md",
	}, initialJob); err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobStatus(t.Context(), initialJob.ID, model.JobReady, nil); err != nil {
		t.Fatal(err)
	}
	index := newTestEngine(t, &testModel{
		chatErr: &llm.Error{Operation: "chat completion", Status: http.StatusBadRequest}, failChats: 1 << 20,
	})
	if err := index.Documents().Write(t.Context(), "viking://resources/manifold/old-memory.md", "old-memory", true); err != nil {
		t.Fatal(err)
	}
	svc, documents := newEngineService(t, db, index)
	plan, err := svc.CreateRenamePlan(t.Context(), []model.RenameOperation{{
		ResourceType: "document", From: "old-memory", To: "new-memory",
	}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.ApplyRenamePlan(t.Context(), plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err == nil {
		t.Fatal("rename unexpectedly succeeded while extraction failed")
	}
	if _, err := db.GetDocument(t.Context(), "old-memory", true); err != nil {
		t.Fatalf("old document was not restored: %v", err)
	}
	if _, err := db.GetDocument(t.Context(), "new-memory", true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("new slug remained active: %v", err)
	}
	failedPlan, err := db.GetRenamePlan(t.Context(), plan.ID)
	if err != nil || failedPlan.Status != "failed" || failedPlan.Error == nil ||
		failedPlan.Error.Code != "dependency_unavailable" {
		t.Fatalf("failed plan = %#v, err = %v", failedPlan, err)
	}
	if failedPlan.Error.Retryable || !slices.Contains(
		failedPlan.Error.Remediation.Steps, "Create a fresh rename preview and review every change.",
	) {
		t.Fatalf("terminal rollback remediation = %#v; want a fresh reviewed plan, not retrying the failed job", failedPlan.Error)
	}
	for _, step := range failedPlan.Error.Remediation.Steps {
		if strings.Contains(step, "/jobs/"+job.ID+"/retry") {
			t.Fatalf("rolled-back rename advertises an unusable retry: %#v", failedPlan.Error.Remediation)
		}
	}
	failedJob, err := db.GetJob(t.Context(), job.ID)
	if err != nil || failedJob.Status != model.JobFailed || failedJob.Error == nil ||
		failedJob.Error.Code != "dependency_unavailable" {
		t.Fatalf("failed job = %#v, err = %v", failedJob, err)
	}
	moves, _, _ := documents.recorded()
	stageDocument := "viking://resources/manifold/.manifold-renames/" + plan.ID + "/documents/0.md"
	for _, expected := range []string{
		"viking://resources/manifold/old-memory.md -> " + stageDocument,
		stageDocument + " -> viking://resources/manifold/new-memory.md",
		"viking://resources/manifold/new-memory.md -> " + stageDocument,
		stageDocument + " -> viking://resources/manifold/old-memory.md",
	} {
		if !slices.Contains(moves, expected) {
			t.Fatalf("moves = %#v, missing %q", moves, expected)
		}
	}
	if exists, _ := index.Documents().Exists(t.Context(), "viking://resources/manifold/old-memory.md"); !exists {
		t.Fatal("rollback did not restore the index path")
	}
}

func TestEmptyFolderSwapUsesTwoPhaseIndexMoves(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	index := newTestEngine(t, &testModel{})
	svc, documents := newEngineService(t, db, index)
	for _, folder := range []model.Folder{
		{ID: "alpha", Name: "Alpha"},
		{ID: "beta", Name: "Beta"},
	} {
		if _, err := svc.CreateFolder(t.Context(), folder); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := svc.CreateRenamePlan(t.Context(), []model.RenameOperation{
		{ResourceType: "folder", From: "alpha", To: "beta"},
		{ResourceType: "folder", From: "beta", To: "alpha"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyRenamePlan(t.Context(), plan.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	applied, err := db.GetRenamePlan(t.Context(), plan.ID)
	if err != nil || applied.Status != "applied" {
		t.Fatalf("plan = %#v, err = %v", applied, err)
	}
	alpha, err := db.GetFolder(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := db.GetFolder(t.Context(), "beta")
	if err != nil {
		t.Fatal(err)
	}
	if alpha.Name != "Beta" || beta.Name != "Alpha" {
		t.Fatalf("folder swap did not preserve row identities: alpha=%#v beta=%#v", alpha, beta)
	}
	moves, _, _ := documents.recorded()
	stagePrefix := "viking://resources/manifold/.manifold-renames/" + plan.ID
	for _, expected := range []string{
		"viking://resources/manifold/alpha/ -> " + stagePrefix + "/0/",
		"viking://resources/manifold/beta/ -> " + stagePrefix + "/1/",
		stagePrefix + "/0/ -> viking://resources/manifold/beta/",
		stagePrefix + "/1/ -> viking://resources/manifold/alpha/",
	} {
		if !slices.Contains(moves, expected) {
			t.Fatalf("moves = %#v, missing %q", moves, expected)
		}
	}
	for _, uri := range []string{"viking://resources/manifold/alpha/", "viking://resources/manifold/beta/"} {
		if exists, _ := index.Documents().Exists(t.Context(), uri); !exists {
			t.Fatalf("folder %s is missing after the swap", uri)
		}
	}
}

func TestDeleteFolderRemovesItsIndexPath(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	index := newTestEngine(t, &testModel{})
	svc, _ := newEngineService(t, db, index)
	if _, err := svc.CreateFolder(t.Context(), model.Folder{ID: "release-evidence", Name: "Release evidence"}); err != nil {
		t.Fatal(err)
	}
	uri := "viking://resources/manifold/release-evidence/"
	if exists, _ := index.Documents().Exists(t.Context(), uri); !exists {
		t.Fatal("created folder is missing from the index")
	}
	if err := svc.DeleteFolder(t.Context(), "release-evidence"); err != nil {
		t.Fatal(err)
	}
	if exists, _ := index.Documents().Exists(t.Context(), uri); exists {
		t.Fatal("deleted folder remained in the index")
	}
	if _, err := db.GetFolder(t.Context(), "release-evidence"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted folder remained in the control plane: %v", err)
	}
}

type fakeGraphStore struct{}

type fakeDocumentStore struct{}

func (fakeDocumentStore) Health(context.Context) error                      { return nil }
func (fakeDocumentStore) Exists(context.Context, string) (bool, error)      { return false, nil }
func (fakeDocumentStore) Mkdir(context.Context, string, string) error       { return nil }
func (fakeDocumentStore) Write(context.Context, string, string, bool) error { return nil }
func (fakeDocumentStore) Move(context.Context, string, string) error        { return nil }
func (fakeDocumentStore) Delete(context.Context, string, bool) error        { return nil }
func (fakeDocumentStore) Snapshot(context.Context, string, []string) (string, error) {
	return "snapshot", nil
}
func (fakeDocumentStore) Search(context.Context, string, string, int) ([]model.SearchHit, error) {
	return nil, nil
}

func (fakeGraphStore) Health(context.Context) error {
	return nil
}

func (fakeGraphStore) IngestDocument(context.Context, upstream.GraphDocument) (upstream.GraphIngestResult, error) {
	return upstream.GraphIngestResult{}, nil
}

func (fakeGraphStore) HasStoredResult(context.Context, string, string) (bool, error) {
	return false, nil
}

func (fakeGraphStore) Search(context.Context, string, int, bool) ([]model.SearchHit, error) {
	return nil, nil
}

func (fakeGraphStore) IngestFact(context.Context, model.Fact, model.Entity) (string, string, error) {
	return "", "", nil
}

func (fakeGraphStore) IngestRelation(context.Context, model.Relation, model.Entity, model.Entity) (string, error) {
	return "", nil
}

func (fakeGraphStore) RetractFact(context.Context, string, string) ([]string, error) {
	return nil, nil
}

// scopeRecordingDocuments records the scope of every document index search.
type scopeRecordingDocuments struct {
	fakeDocumentStore
	mu     sync.Mutex
	scopes []string
}

func (d *scopeRecordingDocuments) Search(_ context.Context, _ string, scope string, _ int) ([]model.SearchHit, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.scopes = append(d.scopes, scope)
	return nil, nil
}

func TestScopeGlobNarrowsTheSemanticDocumentSearch(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	documents := &scopeRecordingDocuments{}
	svc := New(db, documents, fakeGraphStore{}, "https://memory.example.test",
		slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	for _, request := range []SearchRequest{
		{Query: "deploy", Mode: model.SearchSemantic, ScopeGlob: "projects/ngbot/**"},
		{Query: "deploy", Mode: model.SearchSemantic, ScopeGlob: "**/notes"},
		{Query: "deploy", Mode: model.SearchSemantic, ScopeGlob: "projects/**", Scope: "viking://resources/manifold/explicit/"},
	} {
		if _, err := svc.Search(t.Context(), request); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"viking://resources/manifold/projects/ngbot/", "", "viking://resources/manifold/explicit/",
	}
	if !slices.Equal(documents.scopes, want) {
		t.Fatalf("document index scopes = %q, want %q", documents.scopes, want)
	}
}
