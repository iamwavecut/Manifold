package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/iamwavecut/Manifold/internal/identity"
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

func TestDocumentRetryResumesAfterOpenVikingCheckpoint(t *testing.T) {
	var writes, snapshots, ingests int
	openViking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/content/write":
			writes++
			if writes > 1 {
				http.Error(w, `{"error":"resource already exists"}`, http.StatusConflict)
				return
			}
			_, _ = w.Write([]byte(`{"status":"ok","result":{}}`))
		case "/api/v1/snapshot/commit":
			snapshots++
			_, _ = w.Write([]byte(`{"status":"ok","result":{"commit_oid":"snapshot-retry"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer openViking.Close()

	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ingest/document" {
			http.NotFound(w, r)
			return
		}
		ingests++
		if ingests == 1 {
			http.Error(w, `{"error":"provider unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"documentId":"brain-retry","committed":{"entityIds":[],"factIds":[],"edgeIds":[]}}`))
	}))
	defer brain.Close()

	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := New(
		db,
		upstream.NewOpenViking(openViking.URL, "", openViking.Client()),
		upstream.NewBrain(brain.URL, "brain-key", brain.Client()),
		"https://memory.example.test",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		time.Millisecond,
	)
	_, job, err := svc.CreateDocument(t.Context(), model.Document{
		ID: "retry-memory", Title: "Retry memory", Format: "markdown", Content: "Resume from the durable checkpoint.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(t.Context()); err == nil {
		t.Fatal("initial sync unexpectedly succeeded while Brain was unavailable")
	}
	partiallyReady, err := db.GetJob(t.Context(), job.ID)
	if err != nil || partiallyReady.Status != model.JobPartiallyReady {
		t.Fatalf("initial job = %#v, err = %v", partiallyReady, err)
	}
	revision, err := db.GetRevision(t.Context(), "retry-memory", 1)
	if err != nil || revision.SnapshotOID != "snapshot-retry" {
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
	if err != nil || document.Status != model.JobReady || document.BrainID != "brain-retry" {
		t.Fatalf("retried document = %#v, err = %v", document, err)
	}
	if writes != 1 || snapshots != 1 || ingests != 2 {
		t.Fatalf("upstream calls: writes=%d snapshots=%d ingests=%d", writes, snapshots, ingests)
	}
}

func TestDocumentRetryReconcilesCreateBeforeCheckpoint(t *testing.T) {
	var writeModes []string
	snapshotAttempts := 0
	openViking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/content/write":
			var body struct {
				Mode string `json:"mode"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			writeModes = append(writeModes, body.Mode)
			if len(writeModes) == 2 {
				http.Error(w, `{"error":"resource already exists"}`, http.StatusConflict)
				return
			}
			_, _ = w.Write([]byte(`{"status":"ok","result":{}}`))
		case "/api/v1/fs/stat":
			_, _ = w.Write([]byte(`{"status":"ok","result":{"uri":"viking://resources/manifold/retry-before-checkpoint.md"}}`))
		case "/api/v1/snapshot/commit":
			snapshotAttempts++
			if snapshotAttempts == 1 {
				http.Error(w, `{"error":"snapshot unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"status":"ok","result":{"commit_oid":"snapshot-reconciled"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer openViking.Close()

	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ingest/document" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"documentId":"brain-reconciled","committed":{"entityIds":[],"factIds":[],"edgeIds":[]}}`))
	}))
	defer brain.Close()

	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := New(
		db,
		upstream.NewOpenViking(openViking.URL, "", openViking.Client()),
		upstream.NewBrain(brain.URL, "brain-key", brain.Client()),
		"https://memory.example.test",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		time.Millisecond,
	)
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
	if !slices.Equal(writeModes, []string{"create", "create", "replace"}) {
		t.Fatalf("write modes = %#v, want create/create/replace reconciliation", writeModes)
	}
	if snapshotAttempts != 2 {
		t.Fatalf("snapshot attempts = %d, want 2", snapshotAttempts)
	}
}

func TestHTTPPipelineAndInterruptedRenameResumeEndToEnd(t *testing.T) {
	var openVikingMoves []string
	openViking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/api/v1/content/write", "/api/v1/fs/mkdir":
			_, _ = w.Write([]byte(`{"status":"ok","result":{}}`))
		case "/api/v1/fs/mv":
			var body struct {
				From string `json:"from_uri"`
				To   string `json:"to_uri"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			openVikingMoves = append(openVikingMoves, body.From+" -> "+body.To)
			_, _ = w.Write([]byte(`{"status":"ok","result":{}}`))
		case "/api/v1/snapshot/commit":
			_, _ = w.Write([]byte(`{"status":"ok","result":{"commit_oid":"snapshot-1"}}`))
		case "/api/v1/search/find":
			_, _ = w.Write([]byte(`{"status":"ok","result":{"resources":[{"uri":"viking://resources/manifold/old-memory.md","score":0.8,"overview":"Canonical retention evidence"},{"uri":"viking://resources/manifold/.overview.md","score":0.99,"overview":"Internal index metadata"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer openViking.Close()

	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/ingest/document":
			_, _ = w.Write([]byte(`{"documentId":"brain-doc-1","committed":{"entityIds":["entity-1"],"factIds":["fact-1"],"edgeIds":[]}}`))
		case "/v1/search":
			_, _ = w.Write([]byte(`{"results":[{"entityId":"entity-1","entityType":"decision","canonicalName":"Retention","score":0.9,"facts":[{"factId":"fact-1","predicate":"retention","object":"canonical","score":0.9,"sourceKey":"brain-doc-1"}]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer brain.Close()

	dbPath := filepath.Join(t.TempDir(), "manifold.db")
	db, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	documents := upstream.NewOpenViking(openViking.URL, "", openViking.Client())
	graph := upstream.NewBrain(brain.URL, "brain-key", brain.Client())
	svc := New(db, documents, graph, "https://memory.example.test", logger, time.Millisecond)

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
	revision, err := db.GetRevision(t.Context(), "old-memory", 1)
	if err != nil || revision.SnapshotOID != "snapshot-1" {
		t.Fatalf("revision = %#v, err = %v", revision, err)
	}

	search, err := svc.Search(t.Context(), SearchRequest{
		Query: "canonical", Mode: model.SearchHybrid, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Items) != 1 || len(search.DegradedDependencies) != 0 {
		t.Fatalf("hybrid search = %#v", search)
	}
	if search.Items[0].ID != "old-memory" || search.Items[0].CanonicalRef != "manifold://documents/old-memory@r1" ||
		search.Items[0].Path != "old-memory" {
		t.Fatalf("hybrid search exposed a non-canonical hit: %#v", search.Items[0])
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
	resumed := New(recovered, documents, graph, "https://memory.example.test", logger, time.Millisecond)
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
	stageDocument := "viking://resources/manifold/.manifold-renames/" + plan.ID + "/documents/0.md"
	for _, expected := range []string{
		"viking://resources/manifold/old-memory.md -> " + stageDocument,
		stageDocument + " -> viking://resources/manifold/new-memory.md",
	} {
		if !slices.Contains(openVikingMoves, expected) {
			t.Fatalf("OpenViking moves = %#v, missing %q", openVikingMoves, expected)
		}
	}
	if len(openVikingMoves) != 2 {
		t.Fatalf("resumed rename repeated completed upstream moves: %#v", openVikingMoves)
	}
	if !identity.IsXID(renameJob.ID) {
		t.Fatalf("rename job ID = %q, want XID", renameJob.ID)
	}
}

func TestRenameDependencyFailureRestoresIDsAndUpstreamURI(t *testing.T) {
	var moves []string
	openViking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/fs/mkdir":
			_, _ = w.Write([]byte(`{"status":"ok","result":{}}`))
		case "/api/v1/fs/mv":
			var body struct {
				From string `json:"from_uri"`
				To   string `json:"to_uri"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			moves = append(moves, body.From+" -> "+body.To)
			_, _ = w.Write([]byte(`{"status":"ok","result":{}}`))
		case "/api/v1/content/write":
			_, _ = w.Write([]byte(`{"status":"ok","result":{}}`))
		case "/api/v1/snapshot/commit":
			_, _ = w.Write([]byte(`{"status":"ok","result":{"commit_oid":"snapshot"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer openViking.Close()
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ingest/document" {
			http.Error(w, `{"private":"provider detail"}`, http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	defer brain.Close()

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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := New(
		db,
		upstream.NewOpenViking(openViking.URL, "", openViking.Client()),
		upstream.NewBrain(brain.URL, "brain-key", brain.Client()),
		"https://memory.example.test",
		logger,
		time.Millisecond,
	)
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
		t.Fatal("rename unexpectedly succeeded while Brain was unavailable")
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
	failedJob, err := db.GetJob(t.Context(), job.ID)
	if err != nil || failedJob.Status != model.JobFailed || failedJob.Error == nil ||
		failedJob.Error.Code != "dependency_unavailable" {
		t.Fatalf("failed job = %#v, err = %v", failedJob, err)
	}
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
}

func TestEmptyFolderSwapUsesTwoPhaseOpenVikingMoves(t *testing.T) {
	var moves []string
	openViking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/fs/mkdir":
			_, _ = w.Write([]byte(`{"status":"ok","result":{}}`))
		case "/api/v1/fs/mv":
			var body struct {
				From string `json:"from_uri"`
				To   string `json:"to_uri"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			moves = append(moves, body.From+" -> "+body.To)
			_, _ = w.Write([]byte(`{"status":"ok","result":{}}`))
		case "/api/v1/fs":
			_, _ = w.Write([]byte(`{"status":"ok","result":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer openViking.Close()

	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, folder := range []model.Folder{
		{ID: "alpha", Name: "Alpha"},
		{ID: "beta", Name: "Beta"},
	} {
		if _, err := db.CreateFolder(t.Context(), folder); err != nil {
			t.Fatal(err)
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := New(
		db,
		upstream.NewOpenViking(openViking.URL, "", openViking.Client()),
		fakeGraphStore{},
		"https://memory.example.test",
		logger,
		time.Millisecond,
	)
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
}

func TestDeleteFolderRecursivelyRetriesOpenVikingDerivedEntryLocks(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.CreateFolder(t.Context(), model.Folder{
		ID: "release-evidence", Name: "Release evidence",
	}); err != nil {
		t.Fatal(err)
	}
	documents := &recordingDocumentStore{conflictsRemaining: 2}
	svc := New(
		db,
		documents,
		fakeGraphStore{},
		"https://memory.example.test",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		time.Millisecond,
	)

	if err := svc.DeleteFolder(t.Context(), "release-evidence"); err != nil {
		t.Fatal(err)
	}
	if documents.deletedURI != "viking://resources/manifold/release-evidence/" || !documents.recursive {
		t.Fatalf("OpenViking delete = (%q, recursive=%t), want the empty folder recursively removed",
			documents.deletedURI, documents.recursive)
	}
	if documents.deleteCalls != 3 {
		t.Fatalf("OpenViking delete calls = %d, want two path-lock conflicts followed by success",
			documents.deleteCalls)
	}
	if _, err := db.GetFolder(t.Context(), "release-evidence"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted folder remained in the control plane: %v", err)
	}
}

type fakeGraphStore struct{}

type fakeDocumentStore struct{}

type recordingDocumentStore struct {
	fakeDocumentStore
	deletedURI         string
	recursive          bool
	deleteCalls        int
	conflictsRemaining int
}

func (r *recordingDocumentStore) Delete(_ context.Context, uri string, recursive bool) error {
	r.deletedURI = uri
	r.recursive = recursive
	r.deleteCalls++
	if r.conflictsRemaining > 0 {
		r.conflictsRemaining--
		return &upstream.DependencyError{
			Dependency: "openviking", Operation: "delete folder", Status: http.StatusConflict,
		}
	}
	return nil
}

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

func (fakeGraphStore) Search(context.Context, string, int, bool) ([]model.SearchHit, error) {
	return nil, nil
}

func (fakeGraphStore) IngestFact(context.Context, model.Fact, model.Entity) (string, string, error) {
	return "", "", nil
}

func (fakeGraphStore) IngestRelation(context.Context, model.Relation, model.Entity, model.Entity) (string, error) {
	return "", nil
}

func (fakeGraphStore) RetractFact(context.Context, string, string) error {
	return nil
}

func (fakeGraphStore) GetEntity(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}
