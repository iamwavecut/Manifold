package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/store"
	"github.com/iamwavecut/Manifold/internal/upstream"
)

func TestMaterializeGraphDocumentProjectsTypedBrainResultWithExactSource(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := New(db, fakeDocumentStore{}, fakeGraphStore{}, "", slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	doc, _, err := db.CreateDocument(t.Context(), model.Document{
		ID: "derived-service", Title: "Derived service", Format: "markdown", Content: "Mira works at Meridian.",
		OVURI: "viking://resources/manifold/derived-service.md",
	}, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "derived-service",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := upstream.GraphIngestResult{
		DocumentID: "brain-document",
		OriginURI:  "manifold://documents/derived-service@r1",
		Revision:   "r1",
		EntityIDs:  []string{"brain-mira", "brain-meridian"},
		FactIDs:    []string{"brain-fact"},
		EdgeIDs:    []string{"brain-edge"},
		Entities: []upstream.GraphEntity{
			{BrainID: "brain-mira", Name: "Mira Chen", Type: "staff"},
			{BrainID: "brain-meridian", Name: "Meridian Labs", Type: "project"},
		},
		Facts: []upstream.GraphFact{{
			BrainID: "brain-fact", EntityBrainID: "brain-mira", Predicate: "works_at", Object: "Meridian Labs",
			Status: "active", Confidence: 0.91,
		}},
		Relations: []upstream.GraphRelation{{
			BrainID: "brain-edge", FromEntityBrainID: "brain-mira", ToEntityBrainID: "brain-meridian",
			Predicate: "works_at", Status: "active", Confidence: 0.91,
		}},
	}
	if err := svc.materializeGraphDocument(t.Context(), doc, result, nil); err != nil {
		t.Fatal(err)
	}
	mira, err := db.GetEntity(t.Context(), "mira-chen")
	if err != nil {
		t.Fatal(err)
	}
	facts, err := db.ListFacts(t.Context(), mira.ID, 100)
	if err != nil || len(facts) != 1 {
		t.Fatalf("projected facts = %#v, err = %v", facts, err)
	}
	if facts[0].SourceDocumentID != doc.ID || facts[0].SourceRevision != doc.Revision || facts[0].Status != "active" {
		t.Fatalf("fact source was not bound to the local revision: %#v", facts[0])
	}
	if relations, err := db.ListRelations(t.Context(), mira.ID, 100); err != nil || len(relations) != 1 {
		t.Fatalf("projected relations = %#v, err = %v", relations, err)
	}

	stale := result
	stale.Revision = "r2"
	if err := svc.materializeGraphDocument(t.Context(), doc, stale, nil); !errors.Is(err, store.ErrDocumentSyncSuperseded) {
		t.Fatalf("stale result error = %v, want ErrDocumentSyncSuperseded", err)
	}
	wrongOrigin := result
	wrongOrigin.OriginURI = "manifold://documents/other-document@r1"
	if err := svc.materializeGraphDocument(t.Context(), doc, wrongOrigin, nil); !errors.Is(err, ErrDerivedGraphSourceMismatch) {
		t.Fatalf("wrong origin error = %v, want ErrDerivedGraphSourceMismatch", err)
	}
	incomplete := result
	incomplete.Facts = nil
	if err := svc.materializeGraphDocument(t.Context(), doc, incomplete, nil); !errors.Is(err, ErrDerivedGraphIncomplete) {
		t.Fatalf("incomplete result error = %v, want ErrDerivedGraphIncomplete", err)
	}
	facts, err = db.ListFacts(t.Context(), mira.ID, 100)
	if err != nil || len(facts) != 1 || facts[0].Object != "Meridian Labs" {
		t.Fatalf("rejected result changed current graph: %#v, err = %v", facts, err)
	}
}

func TestMaterializeGraphDocumentMatchesEveryDeclaredCommitID(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := New(db, fakeDocumentStore{}, fakeGraphStore{}, "", slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	doc, _, err := db.CreateDocument(t.Context(), model.Document{
		ID: "derived-id-validation", Title: "ID validation", Format: "markdown", Content: "Mira works at Meridian.",
		OVURI: "viking://resources/manifold/derived-id-validation.md",
	}, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "derived-id-validation",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := upstream.GraphIngestResult{
		OriginURI: "manifold://documents/derived-id-validation@r1",
		Revision:  "r1",
		EntityIDs: []string{"brain-mira", "brain-meridian"},
		FactIDs:   []string{"brain-fact"},
		EdgeIDs:   []string{"brain-edge"},
		Entities: []upstream.GraphEntity{
			{BrainID: "brain-mira", Name: "Mira Chen", Type: "staff"},
			{BrainID: "brain-meridian", Name: "Meridian Labs", Type: "project"},
		},
		Facts: []upstream.GraphFact{{
			BrainID: "brain-fact", EntityBrainID: "brain-mira", Predicate: "works_at", Object: "Meridian Labs",
		}},
		Relations: []upstream.GraphRelation{{
			BrainID: "brain-edge", FromEntityBrainID: "brain-mira", ToEntityBrainID: "brain-meridian", Predicate: "works_at",
		}},
	}
	invalid := []struct {
		name   string
		change func(*upstream.GraphIngestResult)
	}{
		{name: "entity", change: func(result *upstream.GraphIngestResult) {
			result.EntityIDs = []string{"missing-entity", "brain-meridian"}
		}},
		{name: "fact", change: func(result *upstream.GraphIngestResult) { result.FactIDs = []string{"missing-fact"} }},
		{name: "relation", change: func(result *upstream.GraphIngestResult) { result.EdgeIDs = []string{"missing-edge"} }},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			result := result
			test.change(&result)
			if err := svc.materializeGraphDocument(t.Context(), doc, result, nil); !errors.Is(err, ErrDerivedGraphIncomplete) {
				t.Fatalf("mismatched committed ID error = %v, want ErrDerivedGraphIncomplete", err)
			}
		})
	}

	noIDDoc, _, err := db.CreateDocument(t.Context(), model.Document{
		ID: "derived-no-ids", Title: "No committed ID list", Format: "markdown", Content: "Mira works at Meridian.",
		OVURI: "viking://resources/manifold/derived-no-ids.md",
	}, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "derived-no-ids",
	})
	if err != nil {
		t.Fatal(err)
	}
	noIDs := result
	noIDs.OriginURI = "manifold://documents/derived-no-ids@r1"
	noIDs.Revision = "r1"
	noIDs.EntityIDs, noIDs.FactIDs, noIDs.EdgeIDs = nil, nil, nil
	if err := svc.materializeGraphDocument(t.Context(), noIDDoc, noIDs, nil); err != nil {
		t.Fatalf("details without legacy committed ID arrays were rejected: %v", err)
	}
}

func TestCanonicalizeProjectedFactHitRequiresCurrentLocalMapping(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := New(db, fakeDocumentStore{}, fakeGraphStore{}, "", slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	doc, _, err := db.CreateDocument(t.Context(), model.Document{
		ID: "derived-search", Title: "Derived search", Format: "markdown", Content: "Mira works at Meridian.",
		OVURI: "viking://resources/manifold/derived-search.md",
	}, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "derived-search",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.materializeGraphDocument(t.Context(), doc, upstream.GraphIngestResult{
		OriginURI: "manifold://documents/derived-search@r1",
		Revision:  "r1",
		EntityIDs: []string{"brain-mira"},
		FactIDs:   []string{"brain-fact"},
		Entities:  []upstream.GraphEntity{{BrainID: "brain-mira", Name: "Mira Chen", Type: "staff"}},
		Facts: []upstream.GraphFact{{
			BrainID: "brain-fact", EntityBrainID: "brain-mira", Predicate: "works_at", Object: "Meridian Labs", Status: "active",
		}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	canonical, ok, err := svc.canonicalizeProjectedFactHit(t.Context(), model.SearchHit{
		Kind: "fact", ID: "brain-fact", UpstreamSourceRef: doc.OVURI,
	})
	if err != nil || !ok {
		t.Fatalf("mapped graph hit = %#v, ok=%t, err=%v", canonical, ok, err)
	}
	facts, err := db.ListFacts(t.Context(), "mira-chen", 100)
	if err != nil || len(facts) != 1 {
		t.Fatalf("projected facts = %#v, err=%v", facts, err)
	}
	if canonical.ID != facts[0].ID || canonical.CanonicalRef != "manifold://facts/"+facts[0].ID ||
		canonical.Path != doc.ID || canonical.Revision != "r1" || canonical.UpstreamSourceRef != "" {
		t.Fatalf("graph hit did not use local canonical identity/provenance: %#v", canonical)
	}

	for _, hit := range []model.SearchHit{
		{Kind: "fact", ID: "unmapped-fact", UpstreamSourceRef: doc.OVURI},
		{Kind: "fact", ID: "brain-fact", UpstreamSourceRef: doc.OVURI},
	} {
		if hit.ID == "brain-fact" {
			if _, _, err := db.UpdateDocument(t.Context(), doc.ID, doc.ETag, "", "Mira works at Borealis.", nil, nil, model.Job{
				ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: doc.ID,
			}); err != nil {
				t.Fatal(err)
			}
		}
		if hit, ok, err := svc.canonicalizeProjectedFactHit(t.Context(), hit); err != nil || ok {
			t.Fatalf("unmapped or superseded graph hit was retained: %#v, ok=%t, err=%v", hit, ok, err)
		}
	}
}

func TestCanonicalizeSharedFactUsesRemainingLiveDocumentClaim(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := New(db, fakeDocumentStore{}, fakeGraphStore{}, "", slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	create := func(id string) model.Document {
		t.Helper()
		doc, _, err := db.CreateDocument(t.Context(), model.Document{
			ID: id, Title: id, Format: "markdown", Content: "Mira works at Meridian.",
			OVURI: "viking://resources/manifold/" + id + ".md",
		}, model.Job{
			ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: id,
		})
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	first := create("shared-source-a")
	second := create("shared-source-b")
	resultFor := func(doc model.Document) upstream.GraphIngestResult {
		return upstream.GraphIngestResult{
			OriginURI: "manifold://documents/" + doc.ID + "@" + doc.Revision,
			Revision:  doc.Revision,
			EntityIDs: []string{"brain-mira", "brain-meridian"},
			FactIDs:   []string{"brain-shared-fact"},
			Entities: []upstream.GraphEntity{
				{BrainID: "brain-mira", Name: "Mira Chen", Type: "staff"},
				{BrainID: "brain-meridian", Name: "Meridian Labs", Type: "project"},
			},
			Facts: []upstream.GraphFact{{
				BrainID: "brain-shared-fact", EntityBrainID: "brain-mira", Predicate: "works_at",
				Object: "Meridian Labs", Status: "active",
			}},
		}
	}
	for _, doc := range []model.Document{first, second} {
		if err := svc.materializeGraphDocument(t.Context(), doc, resultFor(doc), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.MarkDocumentDeleted(t.Context(), second.ID, second.ETag, model.Job{
		ID: identity.NewXID(), Kind: "document.delete", ResourceType: "document", ResourceID: second.ID,
	}); err != nil {
		t.Fatal(err)
	}

	hit, ok, err := svc.canonicalizeProjectedFactHit(t.Context(), model.SearchHit{Kind: "fact", ID: "brain-shared-fact"})
	if err != nil || !ok {
		t.Fatalf("shared current fact hit = %#v, ok=%t, err=%v", hit, ok, err)
	}
	facts, err := db.ListFacts(t.Context(), "mira-chen", 100)
	if err != nil || len(facts) != 1 {
		t.Fatalf("remaining shared fact = %#v, err=%v", facts, err)
	}
	if facts[0].SourceDocumentID != first.ID || facts[0].SourceRevision != "r1" ||
		hit.Path != first.ID || hit.Revision != "r1" || hit.CanonicalRef != "manifold://facts/"+facts[0].ID {
		t.Fatalf("deleted claim displaced surviving provenance: fact=%#v hit=%#v", facts[0], hit)
	}
}

func TestCanonicalizeProjectedFactChoosesScopeMatchingLiveClaim(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := New(db, fakeDocumentStore{}, fakeGraphStore{}, "", slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	create := func(id, folder string) model.Document {
		t.Helper()
		doc, _, _, err := svc.CreateDocumentAtPath(t.Context(), folder, model.Document{
			ID: id, Title: id, Format: "markdown", Content: "Mira works at Meridian.",
		})
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	outside := create("a-source-outside", "tasks/outside")
	inside := create("z-source-inside", "shared/inside")
	resultFor := func(doc model.Document) upstream.GraphIngestResult {
		return upstream.GraphIngestResult{
			OriginURI: "manifold://documents/" + doc.ID + "@" + doc.Revision,
			Revision:  doc.Revision,
			EntityIDs: []string{"brain-scoped-mira"},
			FactIDs:   []string{"brain-scoped-fact"},
			Entities:  []upstream.GraphEntity{{BrainID: "brain-scoped-mira", Name: "Mira Chen", Type: "staff"}},
			Facts: []upstream.GraphFact{{
				BrainID: "brain-scoped-fact", EntityBrainID: "brain-scoped-mira", Predicate: "works_at", Object: "Meridian Labs",
			}},
		}
	}
	for _, doc := range []model.Document{outside, inside} {
		if err := svc.materializeGraphDocument(t.Context(), doc, resultFor(doc), nil); err != nil {
			t.Fatal(err)
		}
	}
	hit, ok, err := svc.canonicalizeProjectedFactHitWithOptions(
		t.Context(), model.SearchHit{Kind: "fact", ID: "brain-scoped-fact"}, false, "shared/**")
	if err != nil || !ok {
		t.Fatalf("scope-matched graph hit = %#v, ok=%t, err=%v", hit, ok, err)
	}
	if hit.Path != "shared/inside/z-source-inside" || hit.Revision != "r1" {
		t.Fatalf("canonicalizer selected a claim outside ScopeGlob: %#v", hit)
	}
}

func TestCanonicalizeCurrentSharedFactMatchesHitSnippetWithoutScope(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := New(db, fakeDocumentStore{}, fakeGraphStore{}, "", slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	create := func(id string) model.Document {
		t.Helper()
		doc, _, _, err := svc.CreateDocumentAtPath(t.Context(), "shared", model.Document{
			ID: id, Title: id, Format: "markdown", Content: "Mira has a current employer.",
		})
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	first, second := create("a-current-source"), create("z-current-source")
	resultFor := func(doc model.Document, object string) upstream.GraphIngestResult {
		return upstream.GraphIngestResult{
			OriginURI: "manifold://documents/" + doc.ID + "@" + doc.Revision,
			Revision:  doc.Revision,
			EntityIDs: []string{"brain-current-mira"},
			FactIDs:   []string{"brain-current-shared-fact"},
			Entities:  []upstream.GraphEntity{{BrainID: "brain-current-mira", Name: "Mira Chen", Type: "staff"}},
			Facts: []upstream.GraphFact{{
				BrainID: "brain-current-shared-fact", EntityBrainID: "brain-current-mira",
				Predicate: "works_at", Object: object, Status: "active",
			}},
		}
	}
	for _, item := range []struct {
		doc    model.Document
		object string
	}{{first, "Meridian Labs"}, {second, "Borealis Labs"}} {
		if err := svc.materializeGraphDocument(t.Context(), item.doc, resultFor(item.doc, item.object), nil); err != nil {
			t.Fatal(err)
		}
	}
	hit, ok, err := svc.canonicalizeProjectedFactHitWithOptions(t.Context(), model.SearchHit{
		Kind: "fact", ID: "brain-current-shared-fact", Snippet: "works_at Borealis Labs",
	}, false, "")
	if err != nil || !ok {
		t.Fatalf("current shared fact hit = %#v, ok=%t, err=%v", hit, ok, err)
	}
	if hit.Path != "shared/z-current-source" || hit.Snippet != "works_at Borealis Labs" {
		t.Fatalf("snippet did not select the matching current source: %#v", hit)
	}
	fallback, ok, err := svc.canonicalizeProjectedFactHitWithOptions(t.Context(), model.SearchHit{
		Kind: "fact", ID: "brain-current-shared-fact", Snippet: "unrecognized hit content",
	}, false, "")
	if err != nil || !ok || fallback.Path != "shared/a-current-source" || fallback.Snippet != "works_at Meridian Labs" {
		t.Fatalf("unmatched snippet fallback was not deterministic: %#v, ok=%t, err=%v", fallback, ok, err)
	}
}

func TestCanonicalizeHistoricalFactUsesExactRevisionAndExcludesDeletedSources(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := New(db, fakeDocumentStore{}, fakeGraphStore{}, "", slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	doc, _, _, err := svc.CreateDocumentAtPath(t.Context(), "history/archive", model.Document{
		ID: "historical-derived", Title: "Historical fact", Format: "markdown", Content: "Mira works at Meridian.",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := upstream.GraphIngestResult{
		OriginURI: "manifold://documents/historical-derived@r1",
		Revision:  "r1",
		EntityIDs: []string{"brain-history-mira"},
		FactIDs:   []string{"brain-historical-fact"},
		Entities:  []upstream.GraphEntity{{BrainID: "brain-history-mira", Name: "Mira Chen", Type: "staff"}},
		Facts: []upstream.GraphFact{{
			BrainID: "brain-historical-fact", EntityBrainID: "brain-history-mira", Predicate: "works_at",
			Object: "Meridian Labs", Status: "active",
		}},
	}
	if err := svc.materializeGraphDocument(t.Context(), doc, first, nil); err != nil {
		t.Fatal(err)
	}
	updated, _, err := db.UpdateDocument(t.Context(), doc.ID, doc.ETag, "", "Mira works at Borealis.", nil, nil, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: doc.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	second := upstream.GraphIngestResult{
		OriginURI: "manifold://documents/historical-derived@r2",
		Revision:  "r2",
		EntityIDs: []string{"brain-history-mira"},
		FactIDs:   []string{"brain-historical-fact"},
		Entities: []upstream.GraphEntity{{
			BrainID: "brain-history-mira", Name: "Mira Chen Rebranded", Type: "staff",
		}},
		Facts: []upstream.GraphFact{{
			BrainID: "brain-historical-fact", EntityBrainID: "brain-history-mira", Predicate: "works_at",
			Object: "Borealis Labs", Status: "active",
		}},
	}
	if err := svc.materializeGraphDocument(t.Context(), updated, second, nil); err != nil {
		t.Fatal(err)
	}
	current, ok, err := svc.canonicalizeProjectedFactHitWithOptions(
		t.Context(), model.SearchHit{
			Kind: "fact", ID: "brain-historical-fact", Title: "Mira Chen Rebranded", Snippet: "works_at Borealis Labs",
		}, false, "history/**")
	if err != nil || !ok || current.Revision != "r2" || current.Snippet != "works_at Borealis Labs" {
		t.Fatalf("default canonicalization did not use the current snapshot: %#v ok=%t err=%v", current, ok, err)
	}
	if _, ok, err := svc.canonicalizeProjectedFactHitWithOptions(
		t.Context(), model.SearchHit{Kind: "fact", ID: "brain-historical-fact", Revision: "r1"}, false, "history/**"); err != nil || ok {
		t.Fatalf("historical revision was exposed without includeHistory: ok=%t err=%v", ok, err)
	}
	historical, ok, err := svc.canonicalizeProjectedFactHitWithOptions(
		t.Context(), model.SearchHit{
			Kind: "fact", ID: "brain-historical-fact", Revision: "r1",
			Title: "Mira Chen Rebranded", Snippet: "works_at Borealis Labs",
		}, true, "history/**")
	if err != nil || !ok {
		t.Fatalf("historical graph hit = %#v, ok=%t, err=%v", historical, ok, err)
	}
	facts, err := db.ListFactsByUpstreamID(t.Context(), "brain-historical-fact", true)
	var historicalFact *model.Fact
	for i := range facts {
		if facts[i].SourceRevision == "r1" {
			historicalFact = &facts[i]
		}
	}
	if err != nil || len(facts) != 2 || historicalFact == nil || historicalFact.Object != "Meridian Labs" {
		t.Fatalf("historical fact snapshot = %#v, err=%v", facts, err)
	}
	if historical.Path != "history/archive/historical-derived" || historical.Revision != "r1" ||
		historical.CanonicalRef != "manifold://facts/"+historicalFact.ID ||
		historical.Title != "Mira Chen" || historical.Snippet != "works_at Meridian Labs" {
		t.Fatalf("historical hit lost exact revision provenance: %#v", historical)
	}
	matchedBySnapshot, ok, err := svc.canonicalizeProjectedFactHitWithOptions(
		t.Context(), model.SearchHit{
			Kind: "fact", ID: "brain-historical-fact", Title: "Mira Chen Rebranded", Snippet: "works_at Meridian Labs",
		}, true, "history/**")
	if err != nil || !ok || matchedBySnapshot.Revision != "r1" || matchedBySnapshot.Snippet != "works_at Meridian Labs" {
		t.Fatalf("includeHistory did not select the matching snapshot under scope: %#v ok=%t err=%v",
			matchedBySnapshot, ok, err)
	}
	if _, err := db.MarkDocumentDeleted(t.Context(), updated.ID, updated.ETag, model.Job{
		ID: identity.NewXID(), Kind: "document.delete", ResourceType: "document", ResourceID: updated.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.canonicalizeProjectedFactHitWithOptions(
		t.Context(), model.SearchHit{Kind: "fact", ID: "brain-historical-fact", Revision: "r1"}, true, "history/**"); err != nil || ok {
		t.Fatalf("deleted source remained available through includeHistory: ok=%t err=%v", ok, err)
	}
}

type retractingGraphStore struct {
	fakeGraphStore
	mu       sync.Mutex
	calls    []string
	revived  map[string][]string
	failures map[string]error
}

func (g *retractingGraphStore) RetractFact(_ context.Context, upstreamID, reason string) ([]string, error) {
	if reason != derivedFactRetractionReason {
		return nil, fmt.Errorf("unexpected retraction reason %q", reason)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, upstreamID)
	if err := g.failures[upstreamID]; err != nil {
		return nil, err
	}
	revived := g.revived[upstreamID]
	delete(g.revived, upstreamID)
	return revived, nil
}

func derivedFactResult(doc model.Document, factIDs ...string) upstream.GraphIngestResult {
	result := upstream.GraphIngestResult{
		DocumentID: "brain-" + doc.ID,
		OriginURI:  "manifold://documents/" + doc.ID + "@" + doc.Revision,
		Revision:   doc.Revision,
		EntityIDs:  []string{"brain-mira"},
		FactIDs:    factIDs,
		Entities:   []upstream.GraphEntity{{BrainID: "brain-mira", Name: "Mira Chen", Type: "staff"}},
	}
	for _, id := range factIDs {
		result.Facts = append(result.Facts, upstream.GraphFact{
			BrainID: id, EntityBrainID: "brain-mira", Predicate: "note", Object: id, Status: "active",
		})
	}
	return result
}

func TestWithdrawnDocumentFactsAreRetractedInBrain(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	graph := &retractingGraphStore{revived: map[string][]string{}, failures: map[string]error{}}
	svc := New(db, fakeDocumentStore{}, graph, "", slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	doc, _, err := db.CreateDocument(t.Context(), model.Document{
		ID: "withdrawn-source", Title: "Withdrawn source", Format: "markdown", Content: "First revision.",
		OVURI: "viking://resources/manifold/withdrawn-source.md",
	}, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "withdrawn-source",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.materializeGraphDocument(t.Context(), doc, derivedFactResult(doc, "fact-old"), nil); err != nil {
		t.Fatal(err)
	}
	if len(graph.calls) != 0 {
		t.Fatalf("current facts were retracted: %v", graph.calls)
	}

	updated, _, err := db.UpdateDocument(t.Context(), doc.ID, doc.ETag, "", "Second revision.", nil, nil, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: doc.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.materializeGraphDocument(t.Context(), updated,
		derivedFactResult(updated, "fact-kept", "fact-admin", "fact-gone"), nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(graph.calls, []string{"fact-old"}) {
		t.Fatalf("revision retractions = %v, want the dropped fact only", graph.calls)
	}

	graph.calls = nil
	// Retracting fact-kept makes Brain revive the superseded fact-old.
	graph.revived["fact-kept"] = []string{"fact-old"}
	graph.failures["fact-admin"] = &upstream.DependencyError{Dependency: "brain", Status: http.StatusForbidden}
	graph.failures["fact-gone"] = &upstream.DependencyError{Dependency: "brain", Status: http.StatusNotFound}
	job, err := db.MarkDocumentDeleted(t.Context(), updated.ID, updated.ETag, model.Job{
		ID: identity.NewXID(), Kind: "document.delete", ResourceType: "document", ResourceID: updated.ID,
		Payload: []byte(`{"ov_uri":"` + updated.OVURI + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.deleteDocument(t.Context(), job); err != nil {
		t.Fatalf("delete with skipped and missing upstream facts failed: %v", err)
	}
	got := slices.Clone(graph.calls)
	slices.Sort(got)
	if want := []string{"fact-admin", "fact-gone", "fact-kept", "fact-old"}; !slices.Equal(got, want) {
		t.Fatalf("deletion retractions = %v, want %v", got, want)
	}
	if graph.calls[len(graph.calls)-1] != "fact-old" {
		t.Fatalf("revived fact was not retracted after its successor: %v", graph.calls)
	}
	left, err := db.OrphanedDerivedFacts(t.Context(), updated.ID)
	if err != nil || len(left) != 1 || left[0].UpstreamID != "fact-admin" {
		t.Fatalf("unretracted orphans = %#v, err = %v; want only the forbidden fact", left, err)
	}

	graph.calls = nil
	graph.failures["fact-admin"] = &upstream.DependencyError{Dependency: "brain", Status: http.StatusServiceUnavailable, Retryable: true}
	if err := svc.deleteDocument(t.Context(), job); err == nil {
		t.Fatal("transient Brain failure did not fail the delete job")
	}
}

func derivedRelationResult(doc model.Document, edgeIDs ...string) upstream.GraphIngestResult {
	result := upstream.GraphIngestResult{
		DocumentID: "brain-" + doc.ID,
		OriginURI:  "manifold://documents/" + doc.ID + "@" + doc.Revision,
		Revision:   doc.Revision,
		EntityIDs:  []string{"brain-mira", "brain-meridian"},
		EdgeIDs:    edgeIDs,
		Entities: []upstream.GraphEntity{
			{BrainID: "brain-mira", Name: "Mira Chen", Type: "staff"},
			{BrainID: "brain-meridian", Name: "Meridian Labs", Type: "project"},
		},
	}
	for _, id := range edgeIDs {
		result.Relations = append(result.Relations, upstream.GraphRelation{
			BrainID: id, FromEntityBrainID: "brain-mira", ToEntityBrainID: "brain-meridian",
			Predicate: "works_at", Status: "active", Confidence: 0.9,
		})
	}
	return result
}

func TestWithdrawnDocumentRelationsAreRetractedInBrain(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	graph := &retractingGraphStore{revived: map[string][]string{}, failures: map[string]error{}}
	svc := New(db, fakeDocumentStore{}, graph, "", slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	create := func(id string) model.Document {
		t.Helper()
		doc, _, err := db.CreateDocument(t.Context(), model.Document{
			ID: id, Title: id, Format: "markdown", Content: "First revision.",
			OVURI: "viking://resources/manifold/" + id + ".md",
		}, model.Job{ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: id})
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	doc := create("withdrawn-relations")
	live := create("live-relations")
	if err := svc.materializeGraphDocument(t.Context(), live, derivedRelationResult(live, "edge-shared"), nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.materializeGraphDocument(t.Context(), doc,
		derivedRelationResult(doc, "edge-old", "edge-shared"), nil); err != nil {
		t.Fatal(err)
	}
	if len(graph.calls) != 0 {
		t.Fatalf("current relations were retracted: %v", graph.calls)
	}

	updated, _, err := db.UpdateDocument(t.Context(), doc.ID, doc.ETag, "", "Second revision.", nil, nil, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: doc.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.materializeGraphDocument(t.Context(), updated,
		derivedRelationResult(updated, "edge-kept", "edge-admin", "edge-gone", "edge-shared"), nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(graph.calls, []string{"edge-old"}) {
		t.Fatalf("revision retractions = %v, want the dropped relation only", graph.calls)
	}

	graph.calls = nil
	graph.failures["edge-admin"] = &upstream.DependencyError{Dependency: "brain", Status: http.StatusForbidden}
	graph.failures["edge-gone"] = &upstream.DependencyError{Dependency: "brain", Status: http.StatusNotFound}
	job, err := db.MarkDocumentDeleted(t.Context(), updated.ID, updated.ETag, model.Job{
		ID: identity.NewXID(), Kind: "document.delete", ResourceType: "document", ResourceID: updated.ID,
		Payload: []byte(`{"ov_uri":"` + updated.OVURI + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.deleteDocument(t.Context(), job); err != nil {
		t.Fatalf("delete with skipped and missing upstream relations failed: %v", err)
	}
	got := slices.Clone(graph.calls)
	slices.Sort(got)
	if want := []string{"edge-admin", "edge-gone", "edge-kept"}; !slices.Equal(got, want) {
		t.Fatalf("deletion retractions = %v, want %v (edge-shared belongs to a live document)", got, want)
	}
	left, err := db.OrphanedDerivedRelations(t.Context(), updated.ID)
	if err != nil || len(left) != 1 || left[0].UpstreamID != "edge-admin" {
		t.Fatalf("unretracted orphans = %#v, err = %v; want only the forbidden relation", left, err)
	}

	graph.calls = nil
	graph.failures["edge-admin"] = &upstream.DependencyError{Dependency: "brain", Status: http.StatusServiceUnavailable, Retryable: true}
	if err := svc.deleteDocument(t.Context(), job); err == nil {
		t.Fatal("transient Brain failure did not fail the delete job")
	}
}

type slowRetractingGraphStore struct {
	fakeGraphStore
	mu           sync.Mutex
	inFlight     int
	maxInFlight  int
	calls        map[string]int
	conflictOnce map[string]bool
}

func (g *slowRetractingGraphStore) RetractFact(_ context.Context, upstreamID, _ string) ([]string, error) {
	g.mu.Lock()
	g.inFlight++
	g.maxInFlight = max(g.maxInFlight, g.inFlight)
	g.calls[upstreamID]++
	conflict := g.conflictOnce[upstreamID]
	delete(g.conflictOnce, upstreamID)
	g.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	g.mu.Lock()
	g.inFlight--
	g.mu.Unlock()
	if conflict {
		// Brain answers a SurrealDB transaction conflict with a 500.
		return nil, &upstream.DependencyError{Dependency: "brain", Status: http.StatusInternalServerError, Retryable: true}
	}
	return nil, nil
}

func TestDerivedFactRetractionsRunConcurrentlyAndRepeatConflicts(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	graph := &slowRetractingGraphStore{calls: map[string]int{}, conflictOnce: map[string]bool{"fact-3": true}}
	svc := New(db, fakeDocumentStore{}, graph, "", slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	doc, _, err := db.CreateDocument(t.Context(), model.Document{
		ID: "replaced-facts", Title: "Replaced facts", Format: "markdown", Content: "First revision.",
		OVURI: "viking://resources/manifold/replaced-facts.md",
	}, model.Job{ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "replaced-facts"})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 8)
	for i := range ids {
		ids[i] = fmt.Sprintf("fact-%d", i)
	}
	if err := svc.materializeGraphDocument(t.Context(), doc, derivedFactResult(doc, ids...), nil); err != nil {
		t.Fatal(err)
	}
	updated, _, err := db.UpdateDocument(t.Context(), doc.ID, doc.ETag, "", "Second revision.", nil, nil, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: doc.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.materializeGraphDocument(t.Context(), updated, derivedFactResult(updated), nil); err != nil {
		t.Fatalf("a conflict that cleared on the next call failed the sync: %v", err)
	}
	if graph.maxInFlight < 2 || graph.maxInFlight > derivedRetractionConcurrency {
		t.Fatalf("retractions in flight = %d, want 2..%d", graph.maxInFlight, derivedRetractionConcurrency)
	}
	for _, id := range ids {
		want := 1
		if id == "fact-3" {
			want = 2
		}
		if graph.calls[id] != want {
			t.Fatalf("calls for %s = %d, want %d (all calls: %v)", id, graph.calls[id], want, graph.calls)
		}
	}
	if left, err := db.OrphanedDerivedFacts(t.Context(), updated.ID); err != nil || len(left) != 0 {
		t.Fatalf("unretracted orphans = %#v, err = %v", left, err)
	}
}

func TestRetractionWaitsForGraphWorkInFlight(t *testing.T) {
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	graph := &retractingGraphStore{revived: map[string][]string{}, failures: map[string]error{}}
	svc := New(db, fakeDocumentStore{}, graph, "", slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	create := func(id string) model.Document {
		t.Helper()
		doc, _, err := db.CreateDocument(t.Context(), model.Document{
			ID: id, Title: id, Format: "markdown", Content: "First revision.",
			OVURI: "viking://resources/manifold/" + id + ".md",
		}, model.Job{ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: id})
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	stating := create("stating-document")
	dropping := create("dropping-document")
	if err := svc.materializeGraphDocument(t.Context(), dropping, derivedFactResult(dropping, "fact-shared"), nil); err != nil {
		t.Fatal(err)
	}
	updated, _, err := db.UpdateDocument(t.Context(), dropping.ID, dropping.ETag, "", "Second revision.", nil, nil,
		model.Job{ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: dropping.ID})
	if err != nil {
		t.Fatal(err)
	}

	// The stating document's sync is past its Brain submission: Brain may
	// already have committed fact-shared for it.
	hold := svc.holdGraphPhase()
	done := make(chan error, 1)
	go func() {
		done <- svc.materializeGraphDocument(t.Context(), updated, derivedFactResult(updated, "fact-new"), nil)
	}()
	select {
	case err := <-done:
		t.Fatalf("retraction ran while another sync's Brain commit was in flight: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := svc.materializeGraphDocument(t.Context(), stating, derivedFactResult(stating, "fact-shared"), hold); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if slices.Contains(graph.calls, "fact-shared") {
		t.Fatalf("retracted a fact that another document claims: %v", graph.calls)
	}
}
