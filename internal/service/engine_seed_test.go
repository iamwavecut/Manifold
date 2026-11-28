package service

import (
	"net/http"
	"testing"

	"github.com/iamwavecut/Manifold/internal/engine"
	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/llm"
	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/store"
	"github.com/iamwavecut/Manifold/internal/upstream"
)

// A new index seeded from Manifold's database must answer a repeated ingest
// of a ready revision without a model call and with the exact projection
// the store holds, so a rename or retry after the migration costs nothing.
func TestSeededIndexReprojectsReadyRevisionWithoutExtraction(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	extracting := &testModel{extractions: map[string]engine.Extraction{
		"maintains the parser": {
			Entities: []engine.ExtractedEntity{
				{Name: "Ada Lovelace", Type: "person"},
				{Name: "Parser", Type: "project"},
			},
			Facts: []engine.ExtractedFact{{
				Entity: 0, Predicate: "maintains", Value: "parser", Confidence: 0.9,
				Evidence: "Ada Lovelace maintains the parser",
			}},
			Relations: []engine.ExtractedRelation{{From: 0, To: 1, Predicate: "maintains", Confidence: 0.8}},
		},
	}}
	svc, _ := newEngineService(t, db, newTestEngine(t, extracting))
	if _, err := svc.CreateFolder(ctx, model.Folder{ID: "team", Name: "Team"}); err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.CreateDocument(ctx, model.Document{
		ID: "team-notes", FolderID: "team", Title: "Team notes", Format: "markdown",
		Content: "Ada Lovelace maintains the parser.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.processOne(ctx); err != nil {
		t.Fatal(err)
	}
	doc, err := db.GetDocument(ctx, "team-notes", true)
	if err != nil || doc.Status != model.JobReady {
		t.Fatalf("document = %#v, err = %v", doc, err)
	}

	refusing := &testModel{chatErr: &llm.Error{Operation: "chat completion", Status: http.StatusPaymentRequired, Class: "billing"}, failChats: 1000}
	index := newTestEngine(t, refusing)
	if err := index.Seed(ctx, db); err != nil {
		t.Fatal(err)
	}
	if seeded, err := index.Seeded(ctx); err != nil || !seeded {
		t.Fatalf("seeded = %v, err = %v", seeded, err)
	}
	reseeded, _ := newEngineService(t, db, index)
	revision, err := db.GetRevision(ctx, doc.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	ingest, err := reseeded.graph.IngestDocument(ctx, upstream.GraphDocument{
		ID: doc.ID, Title: doc.Title, Format: doc.Format, Content: revision.Content,
		OriginURI: "manifold://documents/" + doc.ID + "@" + doc.Revision, Revision: doc.Revision,
	})
	if err != nil || refusing.chatCalls.Load() != 0 {
		t.Fatalf("seeded ingest err = %v, extractions = %d", err, refusing.chatCalls.Load())
	}
	if len(ingest.Entities) != 2 || len(ingest.Facts) != 1 || len(ingest.Relations) != 1 {
		t.Fatalf("seeded result = %#v", ingest)
	}
	before, err := db.StateVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := reseeded.materializeGraphDocument(ctx, doc, ingest, nil); err != nil {
		t.Fatalf("reprojecting the seeded result: %v", err)
	}
	if after, err := db.StateVersion(ctx); err != nil || after != before {
		t.Fatalf("state version %d -> %d (err %v); the seeded result changed the projection", before, after, err)
	}

	hits, err := index.Graph().Search(ctx, "parser", 5, false)
	if err != nil || len(hits) == 0 || hits[0].ID != ingest.Facts[0].BrainID {
		t.Fatalf("graph hits = %#v, err = %v", hits, err)
	}
	if _, err := index.EmbedPending(ctx); err != nil {
		t.Fatal(err)
	}
	documents, err := index.Documents().Search(ctx, "who maintains the parser", "viking://resources/manifold/team/", 5)
	if err != nil || len(documents) != 1 || documents[0].ID != doc.OVURI {
		t.Fatalf("document hits = %#v, err = %v", documents, err)
	}
}

// The provider is outside Manifold: its refusals block documents, which the
// pipeline reports, while status.state keeps describing the components.
func TestProviderHealthDoesNotDegradeComponentState(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc, _ := newEngineService(t, db, newTestEngine(t, &testModel{}))
	svc.SetProviderHealth(func() string { return "degraded" })
	status, err := svc.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "ready" || status.Components["provider"] != "degraded" || status.Components["index"] != "ready" {
		t.Fatalf("status = %q, components = %v", status.State, status.Components)
	}
}

// An entity created through the API has no index ID in Manifold. Its facts
// must survive the seed, and a later fact about it must join the same index
// entity instead of a duplicate.
func TestSeedKeepsExplicitFactsOfUnmappedEntities(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc, _ := newEngineService(t, db, newTestEngine(t, &testModel{}))
	if _, err := db.CreateEntity(ctx, model.Entity{ID: "manifold-service", Name: "Manifold", Kind: "service"}); err != nil {
		t.Fatal(err)
	}
	created, err := svc.CreateFact(ctx, model.Fact{
		ID: identity.NewXID(), EntityID: "manifold-service", Predicate: "integration_state", Object: "verified", Confidence: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetFact(ctx, created.ID)
	if err != nil || stored.UpstreamID == "" {
		t.Fatalf("explicit fact = %#v, err = %v", stored, err)
	}

	index := newTestEngine(t, &testModel{})
	if err := index.Seed(ctx, db); err != nil {
		t.Fatal(err)
	}
	hits, err := index.Graph().Search(ctx, "Manifold integration state", 5, false)
	if err != nil || len(hits) == 0 || hits[0].ID != stored.UpstreamID {
		t.Fatalf("hits = %#v, err = %v", hits, err)
	}
	reseeded, _ := newEngineService(t, db, index)
	if _, err := reseeded.CreateFact(ctx, model.Fact{
		ID: identity.NewXID(), EntityID: "manifold-service", Predicate: "integration_owner", Object: "operations", Confidence: 1,
	}); err != nil {
		t.Fatal(err)
	}
	hits, err = index.Graph().Search(ctx, "Manifold integration", 10, false)
	if err != nil || len(hits) != 2 {
		t.Fatalf("hits after a new fact = %#v, err = %v", hits, err)
	}
	for _, hit := range hits {
		if hit.Title != "Manifold" {
			t.Fatalf("hit of another entity: %#v", hit)
		}
	}
}
