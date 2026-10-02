package store

import (
	"errors"
	"testing"
	"time"

	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/model"
)

func TestProjectDerivedGraphUsesStableIDsAndCurrentRevisionClaims(t *testing.T) {
	s := openTestStore(t)
	doc := createTestDocument(t, s, "derived-proof", "markdown", "Mira Chen works at Meridian Labs.")
	first := DerivedGraphProjection{
		DocumentID: doc.ID,
		Revision:   doc.Revision,
		Entities: []DerivedGraphEntity{
			{CommitRef: "entity-mira", Name: "Mira Chen", Kind: "staff"},
			{CommitRef: "entity-meridian", Name: "Meridian Labs", Kind: "project"},
		},
		Facts: []DerivedGraphFact{{
			CommitRef: "fact-works-at", EntityCommitRef: "entity-mira", Predicate: "works_at",
			Object: "Meridian Labs", Status: "active", Confidence: 0.93,
		}},
		Relations: []DerivedGraphRelation{{
			CommitRef: "edge-works-at", FromEntityCommitRef: "entity-mira", ToEntityCommitRef: "entity-meridian",
			Predicate: "works_at", Status: "active", Confidence: 0.93,
		}},
	}
	if err := s.ProjectDerivedGraph(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	entities, err := s.ListEntities(t.Context(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 2 || entities[0].ID == "" || entities[1].ID == "" {
		t.Fatalf("projected entities = %#v", entities)
	}
	var miraID, meridianID string
	for _, entity := range entities {
		switch entity.Name {
		case "Mira Chen":
			miraID = entity.ID
		case "Meridian Labs":
			meridianID = entity.ID
		}
	}
	if miraID != "mira-chen" || meridianID != "meridian-labs" {
		t.Fatalf("semantic entity IDs = %q, %q", miraID, meridianID)
	}
	facts, err := s.ListFacts(t.Context(), miraID, 100)
	if err != nil || len(facts) != 1 {
		t.Fatalf("projected facts = %#v, err = %v", facts, err)
	}
	sources, err := s.ListSources(t.Context(), 100)
	if err != nil || len(sources) != 1 || sources[0].DocumentID != doc.ID || sources[0].Revision != "r1" {
		t.Fatalf("projected source claims = %#v, err = %v", sources, err)
	}
	factID := facts[0].ID
	if !identity.IsXID(factID) || facts[0].SourceDocumentID != doc.ID || facts[0].SourceRevision != "r1" {
		t.Fatalf("projected fact lacks local ID or exact provenance: %#v", facts[0])
	}
	relations, err := s.ListRelations(t.Context(), miraID, 100)
	if err != nil || len(relations) != 1 || !identity.IsXID(relations[0].ID) {
		t.Fatalf("projected relations = %#v, err = %v", relations, err)
	}
	firstRelationID := relations[0].ID

	if err := s.ProjectDerivedGraph(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	facts, err = s.ListFacts(t.Context(), miraID, 100)
	if err != nil || len(facts) != 1 || facts[0].ID != factID {
		t.Fatalf("retry changed fact identity or duplicated it: %#v, err = %v", facts, err)
	}
	relations, err = s.ListRelations(t.Context(), miraID, 100)
	if err != nil || len(relations) != 1 || relations[0].ID != firstRelationID {
		t.Fatalf("retry changed relation identity or duplicated it: %#v, err = %v", relations, err)
	}

	updated, _, err := s.UpdateDocument(t.Context(), doc.ID, doc.ETag, "", "Mira Chen works at Borealis Labs.", nil, nil, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: doc.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	second := DerivedGraphProjection{
		DocumentID: updated.ID,
		Revision:   updated.Revision,
		Entities: []DerivedGraphEntity{
			{CommitRef: "entity-mira", Name: "Mira Chen", Kind: "staff"},
			{CommitRef: "entity-borealis", Name: "Borealis Labs", Kind: "project"},
		},
		Facts: []DerivedGraphFact{{
			CommitRef: "fact-works-at-borealis", EntityCommitRef: "entity-mira", Predicate: "works_at",
			Object: "Borealis Labs", Status: "active", Confidence: 0.95,
		}},
		Relations: []DerivedGraphRelation{{
			CommitRef: "edge-works-at-borealis", FromEntityCommitRef: "entity-mira", ToEntityCommitRef: "entity-borealis",
			Predicate: "works_at", Status: "active", Confidence: 0.95,
		}},
	}
	if err := s.ProjectDerivedGraph(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	facts, err = s.ListFacts(t.Context(), miraID, 100)
	if err != nil || len(facts) != 1 || facts[0].Object != "Borealis Labs" || facts[0].SourceRevision != "r2" {
		t.Fatalf("current facts = %#v, err = %v", facts, err)
	}
	sources, err = s.ListSources(t.Context(), 100)
	if err != nil || len(sources) != 1 || sources[0].Revision != "r2" {
		t.Fatalf("superseded source claim remained visible: %#v, err = %v", sources, err)
	}
	if facts[0].ID == factID {
		t.Fatalf("changed fact reused a different Brain commit ref's ID %q", factID)
	}
	if _, err := s.GetEntity(t.Context(), meridianID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("superseded entity remained visible: %v", err)
	}
	if _, err := s.GraphPath(t.Context(), miraID, meridianID, 4); !errors.Is(err, ErrNotFound) {
		t.Fatalf("superseded relation remained in graph path: %v", err)
	}
	if path, err := s.GraphPath(t.Context(), miraID, "borealis-labs", 4); err != nil || len(path) != 1 {
		t.Fatalf("current graph path = %#v, err = %v", path, err)
	}
	if err := s.ProjectDerivedGraph(t.Context(), first); !errors.Is(err, ErrDocumentSyncSuperseded) {
		t.Fatalf("superseded projection error = %v, want ErrDocumentSyncSuperseded", err)
	}

	if _, err := s.MarkDocumentDeleted(t.Context(), updated.ID, updated.ETag, model.Job{
		ID: identity.NewXID(), Kind: "document.delete", ResourceType: "document", ResourceID: updated.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if facts, err := s.ListFacts(t.Context(), miraID, 100); err != nil || len(facts) != 0 {
		t.Fatalf("deleted document facts = %#v, err = %v", facts, err)
	}
	if relations, err := s.ListRelations(t.Context(), miraID, 100); err != nil || len(relations) != 0 {
		t.Fatalf("deleted document relations = %#v, err = %v", relations, err)
	}
	if sources, err := s.ListSources(t.Context(), 100); err != nil || len(sources) != 0 {
		t.Fatalf("deleted document sources = %#v, err = %v", sources, err)
	}
	if _, err := s.GetEntity(t.Context(), miraID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted-only entity remained visible: %v", err)
	}
}

func TestProjectDerivedGraphRollsBackOnUnmappedRelationEndpoint(t *testing.T) {
	s := openTestStore(t)
	doc := createTestDocument(t, s, "derived-atomic", "markdown", "A connected graph.")
	projection := DerivedGraphProjection{
		DocumentID: doc.ID,
		Revision:   doc.Revision,
		Entities:   []DerivedGraphEntity{{CommitRef: "entity-a", Name: "Entity A", Kind: "thing"}},
		Relations: []DerivedGraphRelation{{
			CommitRef: "edge-invalid", FromEntityCommitRef: "entity-a", ToEntityCommitRef: "missing-entity",
			Predicate: "links_to", Status: "active",
		}},
	}
	if err := s.ProjectDerivedGraph(t.Context(), projection); err == nil {
		t.Fatal("projection succeeded with an unmapped relation endpoint")
	}
	entities, err := s.ListEntities(t.Context(), "", 100)
	if err != nil || len(entities) != 0 {
		t.Fatalf("failed projection left entity rows: %#v, err = %v", entities, err)
	}
}

func TestDerivedFactRemainsCurrentWhileAnotherLiveDocumentClaimsIt(t *testing.T) {
	s := openTestStore(t)
	first := createTestDocument(t, s, "derived-source-a", "markdown", "Mira works at Meridian.")
	second := createTestDocument(t, s, "derived-source-b", "markdown", "Mira works at Meridian too.")
	shared := DerivedGraphProjection{
		DocumentID: first.ID,
		Revision:   first.Revision,
		Entities: []DerivedGraphEntity{
			{CommitRef: "shared-mira", Name: "Mira Chen", Kind: "staff"},
			{CommitRef: "shared-meridian", Name: "Meridian Labs", Kind: "project"},
		},
		Facts: []DerivedGraphFact{{
			CommitRef: "shared-fact", EntityCommitRef: "shared-mira", Predicate: "works_at",
			Object: "Meridian Labs", Status: "active",
		}},
		Relations: []DerivedGraphRelation{{
			CommitRef: "shared-edge", FromEntityCommitRef: "shared-mira", ToEntityCommitRef: "shared-meridian",
			Predicate: "works_at", Status: "active",
		}},
	}
	if err := s.ProjectDerivedGraph(t.Context(), shared); err != nil {
		t.Fatal(err)
	}
	shared.DocumentID = second.ID
	shared.Revision = second.Revision
	if err := s.ProjectDerivedGraph(t.Context(), shared); err != nil {
		t.Fatal(err)
	}

	updated, _, err := s.UpdateDocument(t.Context(), first.ID, first.ETag, "", "Mira works at Borealis.", nil, nil, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: first.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ProjectDerivedGraph(t.Context(), DerivedGraphProjection{
		DocumentID: updated.ID,
		Revision:   updated.Revision,
		Entities: []DerivedGraphEntity{
			{CommitRef: "shared-mira", Name: "Mira Chen", Kind: "staff"},
			{CommitRef: "new-borealis", Name: "Borealis Labs", Kind: "project"},
		},
		Facts: []DerivedGraphFact{{
			CommitRef: "new-fact", EntityCommitRef: "shared-mira", Predicate: "works_at", Object: "Borealis Labs", Status: "active",
		}},
		Relations: []DerivedGraphRelation{{
			CommitRef: "new-edge", FromEntityCommitRef: "shared-mira", ToEntityCommitRef: "new-borealis",
			Predicate: "works_at", Status: "active",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	mira, err := s.GetEntity(t.Context(), "mira-chen")
	if err != nil {
		t.Fatal(err)
	}
	facts, err := s.ListFacts(t.Context(), mira.ID, 100)
	if err != nil || len(facts) != 2 {
		t.Fatalf("both live source claims should keep their facts current: %#v, err = %v", facts, err)
	}
	var oldFactFound bool
	for _, fact := range facts {
		if fact.Object == "Meridian Labs" {
			oldFactFound = true
			if fact.SourceDocumentID != second.ID || fact.SourceRevision != "r1" {
				t.Fatalf("shared fact source did not resolve to surviving claim: %#v", fact)
			}
		}
	}
	if !oldFactFound {
		t.Fatalf("surviving second-document fact is missing: %#v", facts)
	}

	if _, err := s.MarkDocumentDeleted(t.Context(), second.ID, second.ETag, model.Job{
		ID: identity.NewXID(), Kind: "document.delete", ResourceType: "document", ResourceID: second.ID,
	}); err != nil {
		t.Fatal(err)
	}
	facts, err = s.ListFacts(t.Context(), mira.ID, 100)
	if err != nil || len(facts) != 1 || facts[0].Object != "Borealis Labs" {
		t.Fatalf("deleted source left an active shared fact: %#v, err = %v", facts, err)
	}
}

func TestDerivedFactUsesSurvivingSourceSnapshotAfterUpdatedSourceDeletion(t *testing.T) {
	s := openTestStore(t)
	first := createTestDocument(t, s, "a-current-fact-source", "markdown", "Mira works at Meridian.")
	updated := createTestDocument(t, s, "z-updated-fact-source", "markdown", "Mira works at Borealis.")
	projectionFor := func(doc model.Document, object string) DerivedGraphProjection {
		return DerivedGraphProjection{
			DocumentID: doc.ID,
			Revision:   doc.Revision,
			Entities:   []DerivedGraphEntity{{CommitRef: "shared-current-mira", Name: "Mira Chen", Kind: "staff"}},
			Facts: []DerivedGraphFact{{
				CommitRef: "shared-current-fact", EntityCommitRef: "shared-current-mira",
				Predicate: "works_at", Object: object, Status: "active",
			}},
		}
	}
	if err := s.ProjectDerivedGraph(t.Context(), projectionFor(first, "Meridian Labs")); err != nil {
		t.Fatal(err)
	}
	if err := s.ProjectDerivedGraph(t.Context(), projectionFor(updated, "Borealis Labs")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDocumentDeleted(t.Context(), updated.ID, updated.ETag, model.Job{
		ID: identity.NewXID(), Kind: "document.delete", ResourceType: "document", ResourceID: updated.ID,
	}); err != nil {
		t.Fatal(err)
	}
	facts, err := s.ListFacts(t.Context(), "mira-chen", 100)
	if err != nil || len(facts) != 1 || facts[0].Object != "Meridian Labs" ||
		facts[0].SourceDocumentID != first.ID || facts[0].SourceRevision != "r1" {
		t.Fatalf("surviving current source snapshot = %#v, err=%v", facts, err)
	}
	byUpstream, err := s.GetFactByUpstreamID(t.Context(), "shared-current-fact")
	if err != nil || byUpstream.Object != "Meridian Labs" || byUpstream.SourceDocumentID != first.ID {
		t.Fatalf("GetFactByUpstreamID returned deleted-source data: %#v, err=%v", byUpstream, err)
	}
}

func TestDivergentDerivedFactReplayCannotOverwriteRevisionSnapshot(t *testing.T) {
	s := openTestStore(t)
	doc := createTestDocument(t, s, "immutable-derived-replay", "markdown", "Mira works at Meridian.")
	projection := DerivedGraphProjection{
		DocumentID: doc.ID,
		Revision:   doc.Revision,
		Entities:   []DerivedGraphEntity{{CommitRef: "immutable-mira", Name: "Mira Chen", Kind: "staff"}},
		Facts: []DerivedGraphFact{{
			CommitRef: "immutable-fact", EntityCommitRef: "immutable-mira", Predicate: "works_at",
			Object: "Meridian Labs", Status: "active",
		}},
	}
	if err := s.ProjectDerivedGraph(t.Context(), projection); err != nil {
		t.Fatal(err)
	}
	beforeVersion, err := s.StateVersion(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	divergent := projection
	divergent.Facts = append([]DerivedGraphFact(nil), projection.Facts...)
	divergent.Facts[0].Object = "Borealis Labs"
	if err := s.ProjectDerivedGraph(t.Context(), divergent); !errors.Is(err, ErrDerivedGraphSnapshotConflict) {
		t.Fatalf("divergent same-revision replay error = %v, want ErrDerivedGraphSnapshotConflict", err)
	}
	afterVersion, err := s.StateVersion(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.ListFacts(t.Context(), "mira-chen", 100)
	if err != nil || len(current) != 1 || current[0].Object != "Meridian Labs" || afterVersion != beforeVersion {
		t.Fatalf("divergent replay changed current graph/version: facts=%#v version=%d->%d err=%v",
			current, beforeVersion, afterVersion, err)
	}
	historical, err := s.ListFactsByUpstreamID(t.Context(), "immutable-fact", true)
	if err != nil || len(historical) != 1 || historical[0].Object != "Meridian Labs" || historical[0].SourceRevision != "r1" {
		t.Fatalf("divergent replay changed revision history: %#v err=%v", historical, err)
	}
}

func TestDerivedEntitySlugCollisionUsesRepositoryNumericSuffix(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateEntity(t.Context(), model.Entity{ID: "mira-chen", Name: "Mira Chen", Kind: "staff"}); err != nil {
		t.Fatal(err)
	}
	doc := createTestDocument(t, s, "derived-slug-collision", "markdown", "Mira Chen works here.")
	if err := s.ProjectDerivedGraph(t.Context(), DerivedGraphProjection{
		DocumentID: doc.ID,
		Revision:   doc.Revision,
		Entities:   []DerivedGraphEntity{{CommitRef: "brain-mira", Name: "Mira Chen", Kind: "staff"}},
	}); err != nil {
		t.Fatal(err)
	}
	entities, err := s.ListEntities(t.Context(), "Mira Chen", 100)
	if err != nil {
		t.Fatal(err)
	}
	var derivedID string
	for _, entity := range entities {
		if entity.UpstreamID == "brain-mira" {
			derivedID = entity.ID
		}
	}
	if derivedID != "mira-chen-2" {
		t.Fatalf("derived collision slug = %q, want mira-chen-2; entities=%#v", derivedID, entities)
	}
}

func TestDerivedEntityClaimsFollowRenameCyclesAndRollback(t *testing.T) {
	s := openTestStore(t)
	doc := createTestDocument(t, s, "derived-rename", "markdown", "Alpha and Beta.")
	projection := DerivedGraphProjection{
		DocumentID: doc.ID,
		Revision:   doc.Revision,
		Entities: []DerivedGraphEntity{
			{CommitRef: "rename-alpha", Name: "Alpha Entity", Kind: "thing"},
			{CommitRef: "rename-beta", Name: "Beta Entity", Kind: "thing"},
		},
	}
	if err := s.ProjectDerivedGraph(t.Context(), projection); err != nil {
		t.Fatal(err)
	}
	plan, err := s.CreateRenamePlan(t.Context(), identity.NewXID(), []model.RenameOperation{
		{ResourceType: "entity", From: "alpha-entity", To: "beta-entity"},
		{ResourceType: "entity", From: "beta-entity", To: "alpha-entity"},
	})
	if err != nil {
		t.Fatal(err)
	}
	queueTestRename(t, s, plan)
	if _, err := s.ApplyRename(t.Context(), plan.ID); err != nil {
		t.Fatal(err)
	}
	assertEntityCommitRef := func(id, want string) {
		t.Helper()
		entity, err := s.GetEntity(t.Context(), id)
		if err != nil {
			t.Fatalf("GetEntity(%q): %v", id, err)
		}
		if entity.UpstreamID != want {
			t.Fatalf("entity %q upstream id = %q, want %q", id, entity.UpstreamID, want)
		}
	}
	assertEntityCommitRef("beta-entity", "rename-alpha")
	assertEntityCommitRef("alpha-entity", "rename-beta")
	if err := s.ProjectDerivedGraph(t.Context(), projection); err != nil {
		t.Fatalf("retry after rename: %v", err)
	}
	assertEntityCommitRef("beta-entity", "rename-alpha")
	assertEntityCommitRef("alpha-entity", "rename-beta")
	if err := s.RollbackRename(t.Context(), plan.ID, nil); err != nil {
		t.Fatal(err)
	}
	assertEntityCommitRef("alpha-entity", "rename-alpha")
	assertEntityCommitRef("beta-entity", "rename-beta")
}

func TestDerivedEntityAllocationHonorsRenameReservations(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateEntity(t.Context(), model.Entity{ID: "old-entity", Name: "Old Entity", Kind: "thing"}); err != nil {
		t.Fatal(err)
	}
	doc := createTestDocument(t, s, "reserved-derived-slug", "markdown", "Mira Chen.")
	plan, err := s.CreateRenamePlan(t.Context(), identity.NewXID(), []model.RenameOperation{
		{ResourceType: "entity", From: "old-entity", To: "mira-chen"},
	})
	if err != nil {
		t.Fatal(err)
	}
	queueTestRename(t, s, plan)
	if err := s.ProjectDerivedGraph(t.Context(), DerivedGraphProjection{
		DocumentID: doc.ID,
		Revision:   doc.Revision,
		Entities:   []DerivedGraphEntity{{CommitRef: "reserved-mira", Name: "Mira Chen", Kind: "staff"}},
	}); err != nil {
		t.Fatal(err)
	}
	entity, err := s.GetEntity(t.Context(), "mira-chen-2")
	if err != nil {
		t.Fatal(err)
	}
	if entity.UpstreamID != "reserved-mira" {
		t.Fatalf("derived entity used the reserved slug: %#v", entity)
	}
}

func TestIdenticalDerivedProjectionDoesNotInvalidateRenamePlan(t *testing.T) {
	s := openTestStore(t)
	doc := createTestDocument(t, s, "derived-idempotent", "markdown", "Retry entity.")
	projection := DerivedGraphProjection{
		DocumentID: doc.ID,
		Revision:   doc.Revision,
		Entities:   []DerivedGraphEntity{{CommitRef: "retry-entity-brain", Name: "Retry Entity", Kind: "thing"}},
	}
	if err := s.ProjectDerivedGraph(t.Context(), projection); err != nil {
		t.Fatal(err)
	}
	plan, err := s.CreateRenamePlan(t.Context(), identity.NewXID(), []model.RenameOperation{
		{ResourceType: "entity", From: "retry-entity", To: "renamed-entity"},
	})
	if err != nil {
		t.Fatal(err)
	}
	queueTestRename(t, s, plan)
	beforeVersion, err := s.StateVersion(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	beforeEntity, err := s.GetEntity(t.Context(), "retry-entity")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if err := s.ProjectDerivedGraph(t.Context(), projection); err != nil {
		t.Fatal(err)
	}
	afterVersion, err := s.StateVersion(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	afterEntity, err := s.GetEntity(t.Context(), "retry-entity")
	if err != nil {
		t.Fatal(err)
	}
	if afterVersion != beforeVersion || !afterEntity.UpdatedAt.Equal(beforeEntity.UpdatedAt) {
		t.Fatalf("identical projection changed persisted state: version %d->%d, updated_at %s->%s",
			beforeVersion, afterVersion, beforeEntity.UpdatedAt, afterEntity.UpdatedAt)
	}
	if _, err := s.ApplyRename(t.Context(), plan.ID); err != nil {
		t.Fatalf("identical projection made the rename plan stale: %v", err)
	}
}

func TestCreateEntityHonorsSlugReservedForRollback(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateEntity(t.Context(), model.Entity{ID: "reserved-source", Name: "Reserved Source", Kind: "thing"}); err != nil {
		t.Fatal(err)
	}
	plan, err := s.CreateRenamePlan(t.Context(), identity.NewXID(), []model.RenameOperation{
		{ResourceType: "entity", From: "reserved-source", To: "reserved-target"},
	})
	if err != nil {
		t.Fatal(err)
	}
	queueTestRename(t, s, plan)
	if _, err := s.ApplyRename(t.Context(), plan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateEntity(t.Context(), model.Entity{ID: "reserved-source", Name: "Competing Entity", Kind: "thing"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("CreateEntity reused a reserved rollback slug: %v", err)
	}
	if err := s.RollbackRename(t.Context(), plan.ID, nil); err != nil {
		t.Fatal(err)
	}
	entity, err := s.GetEntity(t.Context(), "reserved-source")
	if err != nil || entity.Name != "Reserved Source" {
		t.Fatalf("rollback did not restore the reserved entity: %#v, err=%v", entity, err)
	}
}
