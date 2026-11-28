package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/iamwavecut/Manifold/internal/engine"
	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/store"
	"github.com/iamwavecut/Manifold/internal/upstream"
)

// A release before data version 2 kept its derived graph in Brain: its
// database holds ready documents without claims, and API facts whose Brain
// call failed have no index ID. One catch-up pass must extract those
// documents through ordinary sync jobs and index those facts, and a second
// start must not queue anything again.
func TestCatchUpBringsDataOfAnEarlierReleaseIntoTheIndex(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	created, job, err := db.CreateDocument(ctx, model.Document{
		ID: "team-notes", Title: "Team notes", Format: "markdown", Content: "Ada Lovelace maintains the parser.",
		OVURI: "viking://resources/manifold/team-notes.md",
	}, model.Job{ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "team-notes"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetDocumentSyncForRevision(ctx, created.ID, "r1", model.JobReady, "brain-document", "snapshot-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobStatus(ctx, job.ID, model.JobReady, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateEntity(ctx, model.Entity{ID: "manifold-service", Name: "Manifold", Kind: "service"}); err != nil {
		t.Fatal(err)
	}
	apiFact, err := db.CreateFact(ctx, model.Fact{
		ID: identity.NewXID(), EntityID: "manifold-service", Predicate: "integration_state", Object: "verified",
		Origin: "explicit", Status: "active", Confidence: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	extracting := &testModel{extractions: map[string]engine.Extraction{
		"maintains the parser": {
			Entities: []engine.ExtractedEntity{{Name: "Ada Lovelace", Type: "person"}},
			Facts: []engine.ExtractedFact{{
				Entity: 0, Predicate: "maintains", Value: "parser", Confidence: 0.9,
				Evidence: "Ada Lovelace maintains the parser",
			}},
		},
	}}
	index := newTestEngine(t, extracting)
	if err := index.Seed(ctx, db); err != nil {
		t.Fatal(err)
	}
	svc, _ := newEngineService(t, db, index)
	if err := svc.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	runQueuedJobs(t, svc)

	if extracting.chatCalls.Load() != 1 {
		t.Fatalf("extractions = %d, want 1", extracting.chatCalls.Load())
	}
	doc, err := db.GetDocument(ctx, created.ID, false)
	if err != nil || doc.Status != model.JobReady || doc.Revision != "r1" {
		t.Fatalf("document = %+v, err = %v", doc, err)
	}
	facts, err := db.ListFacts(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var derived int
	for _, fact := range facts {
		if fact.Origin == "derived" && fact.SourceDocumentID == created.ID && fact.Object == "parser" {
			derived++
		}
	}
	if derived != 1 {
		t.Fatalf("derived facts of the document = %d in %+v, want 1", derived, facts)
	}
	indexed, err := db.GetFact(ctx, apiFact.ID)
	if err != nil || indexed.UpstreamID == "" {
		t.Fatalf("API fact = %+v, err = %v", indexed, err)
	}
	hits, err := index.Graph().Search(ctx, "Manifold integration state", 5, false)
	if err != nil || len(hits) == 0 || hits[0].ID != indexed.UpstreamID {
		t.Fatalf("API fact hits = %+v, err = %v", hits, err)
	}

	if err := svc.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextJob(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second catch-up queued work: err = %v", err)
	}
}

// A relation recorded through the API can point at a derived entity that no
// live document states any more. The public graph hides that entity, so the
// relation stays unindexed, and the catch-up must not report it as a failure
// at every start.
func TestCatchUpSkipsRecordsOfHiddenEntities(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, entity := range []model.Entity{
		{ID: "ines-moreno", Name: "Ines Moreno", Kind: "staff"},
		{ID: "echo-labs", Name: "Echo Labs", Kind: "organization", UpstreamID: "knowledge_entity:echo"},
	} {
		if _, err := db.CreateEntity(ctx, entity); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.CreateRelation(ctx, model.Relation{
		ID: identity.NewXID(), FromID: "ines-moreno", ToID: "echo-labs", Predicate: "works_at",
		Origin: "explicit", Status: "active", Confidence: 1,
	}); err != nil {
		t.Fatal(err)
	}
	svc, _ := newEngineService(t, db, newTestEngine(t, &testModel{}))
	if err := svc.CatchUp(ctx); err != nil {
		t.Fatalf("catch-up: %v", err)
	}
}

// refusedRetractions answers every retraction with 403, as Brain did for
// some facts: the sync went on and the facts stayed active without a claim.
type refusedRetractions struct{ upstream.GraphStore }

func (refusedRetractions) RetractFact(context.Context, string, string) ([]string, error) {
	return nil, &upstream.DependencyError{Dependency: "graph", Operation: "retract fact", Status: http.StatusForbidden}
}

// Facts that a document no longer states and whose retraction failed must be
// retracted by the catch-up, but only for a document whose current revision
// is projected: the old claims of a document with a pending update are not
// current yet and must stay.
func TestCatchUpRetractsOrphansOnlyOfSettledDocuments(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	extraction := func(object string) engine.Extraction {
		return engine.Extraction{
			Entities: []engine.ExtractedEntity{{Name: "Atlas", Type: "project"}},
			Facts: []engine.ExtractedFact{{Entity: 0, Predicate: "uses", Value: object, Confidence: 0.9,
				Evidence: "Atlas uses " + object}},
		}
	}
	extractor := &testModel{extractions: map[string]engine.Extraction{
		"Atlas uses Redis": extraction("Redis"), "Atlas uses Postgres": extraction("Postgres"),
		"Atlas uses Kafka": extraction("Kafka"),
	}}
	index := newTestEngine(t, extractor)
	refusing := New(db, index.Documents(), refusedRetractions{index.Graph()}, "https://memory.example.test",
		slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	write := func(id, content string, update bool) {
		t.Helper()
		if update {
			current, err := db.GetDocument(ctx, id, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := refusing.UpdateDocument(ctx, id, current.ETag, id, content, nil, nil); err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, _, err := refusing.CreateDocument(ctx, model.Document{ID: id, Title: id, Format: "markdown", Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	write("settled", "Atlas uses Redis.", false)
	write("pending", "Atlas uses Kafka.", false)
	runQueuedJobs(t, refusing)
	write("settled", "Atlas uses Postgres.", true)
	runQueuedJobs(t, refusing)
	write("pending", "Atlas uses nothing else.", true)

	orphans, err := db.OrphanedDerivedFacts(ctx, "settled")
	if err != nil || len(orphans) != 1 {
		t.Fatalf("orphans of the settled document before = %+v, err = %v", orphans, err)
	}
	svc, _ := newEngineService(t, db, index)
	if err := svc.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if orphans, err := db.OrphanedDerivedFacts(ctx, "settled"); err != nil || len(orphans) != 0 {
		t.Fatalf("orphans of the settled document after = %+v, err = %v", orphans, err)
	}
	if orphans, err := db.OrphanedDerivedFacts(ctx, "pending"); err != nil || len(orphans) != 1 {
		t.Fatalf("claims of the document with a pending update = %+v, err = %v; want them kept", orphans, err)
	}
	hits, err := index.Graph().Search(ctx, "Atlas uses Redis", 5, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if hit.ID == orphans[0].UpstreamID {
			t.Fatalf("the retracted fact is still current in the index: %+v", hit)
		}
	}
}

// runQueuedJobs runs jobs until none is left.
func runQueuedJobs(t *testing.T, svc *Service) {
	t.Helper()
	for range 50 {
		err := svc.processOne(t.Context())
		if errors.Is(err, store.ErrNotFound) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("jobs did not drain")
}
