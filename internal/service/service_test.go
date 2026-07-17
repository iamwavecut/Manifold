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
			_, _ = w.Write([]byte(`{"status":"ok","result":{"resources":[{"uri":"viking://resources/manifold/new-memory.md","score":0.8,"overview":"Canonical retention evidence"}]}}`))
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
			_, _ = w.Write([]byte(`{"results":[{"entityId":"entity-1","entityType":"decision","canonicalName":"Retention","score":0.9,"facts":[{"factId":"fact-1","predicate":"retention","object":"canonical","score":0.9,"sourceKey":"document"}]}]}`))
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
	if len(search.Items) < 2 || len(search.DegradedDependencies) != 0 {
		t.Fatalf("hybrid search = %#v", search)
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

type fakeGraphStore struct{}

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
