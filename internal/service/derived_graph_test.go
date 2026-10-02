package service

import (
	"errors"
	"io"
	"log/slog"
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
	if err := svc.materializeGraphDocument(t.Context(), doc, result); err != nil {
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
	if err := svc.materializeGraphDocument(t.Context(), doc, stale); !errors.Is(err, store.ErrDocumentSyncSuperseded) {
		t.Fatalf("stale result error = %v, want ErrDocumentSyncSuperseded", err)
	}
	wrongOrigin := result
	wrongOrigin.OriginURI = "manifold://documents/other-document@r1"
	if err := svc.materializeGraphDocument(t.Context(), doc, wrongOrigin); !errors.Is(err, ErrDerivedGraphSourceMismatch) {
		t.Fatalf("wrong origin error = %v, want ErrDerivedGraphSourceMismatch", err)
	}
	incomplete := result
	incomplete.Facts = nil
	if err := svc.materializeGraphDocument(t.Context(), doc, incomplete); !errors.Is(err, ErrDerivedGraphIncomplete) {
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
			if err := svc.materializeGraphDocument(t.Context(), doc, result); !errors.Is(err, ErrDerivedGraphIncomplete) {
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
	if err := svc.materializeGraphDocument(t.Context(), noIDDoc, noIDs); err != nil {
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
	}); err != nil {
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
		if err := svc.materializeGraphDocument(t.Context(), doc, resultFor(doc)); err != nil {
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
		if err := svc.materializeGraphDocument(t.Context(), doc, resultFor(doc)); err != nil {
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
		if err := svc.materializeGraphDocument(t.Context(), item.doc, resultFor(item.doc, item.object)); err != nil {
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
	if err := svc.materializeGraphDocument(t.Context(), doc, first); err != nil {
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
	if err := svc.materializeGraphDocument(t.Context(), updated, second); err != nil {
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
