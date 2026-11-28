package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/store"
)

// CatchUp brings into the index and the consistent public graph what an
// earlier release, or an interrupted run, left behind. Manifold runs it at
// every start; once everything is caught up it changes nothing.
//
//   - A live ready document whose current revision has no graph result in
//     the index gets an ordinary sync job. Releases before data version 2
//     kept the derived graph outside Manifold, so their documents are
//     extracted again from the canonical text.
//   - A fact or relation recorded through the API without an index ID is
//     added to the index.
//   - A derived fact or relation that no live document claims any more, and
//     whose retraction failed, is retracted for each settled document that
//     claimed it.
func (s *Service) CatchUp(ctx context.Context) error {
	queued, queueErr := s.queueMissingExtractions(ctx)
	indexed, indexErr := s.indexUnmappedRecords(ctx)
	cleaned, orphanErr := s.retractSettledOrphans(ctx)
	if queued+indexed+cleaned > 0 {
		s.logger.Info("index catch-up finished", "documents_queued", queued,
			"records_indexed", indexed, "documents_with_orphans", cleaned)
	}
	return errors.Join(queueErr, indexErr, orphanErr)
}

func (s *Service) queueMissingExtractions(ctx context.Context) (int, error) {
	revisions, err := s.store.SettledDocumentRevisions(ctx)
	if err != nil {
		return 0, fmt.Errorf("list settled documents: %w", err)
	}
	queued := 0
	for _, revision := range revisions {
		stored, err := s.graph.HasStoredResult(ctx, revision.ID, revision.Revision)
		if err != nil {
			return queued, fmt.Errorf("check the graph result of %s@%s: %w", revision.ID, revision.Revision, err)
		}
		if stored {
			continue
		}
		payload, _ := json.Marshal(map[string]any{"create": false, "revision": revision.Revision})
		if _, err := s.store.EnqueueJob(ctx, model.Job{
			ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: revision.ID,
			Payload: payload,
		}); err != nil {
			return queued, fmt.Errorf("queue the extraction of %s@%s: %w", revision.ID, revision.Revision, err)
		}
		queued++
	}
	return queued, nil
}

func (s *Service) indexUnmappedRecords(ctx context.Context) (int, error) {
	facts, err := s.store.UnindexedExplicitFacts(ctx)
	if err != nil {
		return 0, fmt.Errorf("list unindexed facts: %w", err)
	}
	indexed := 0
	var failure error
	for _, fact := range facts {
		entity, err := s.store.GetEntity(ctx, fact.EntityID)
		if errors.Is(err, store.ErrNotFound) {
			// The public graph hides the entity; so is the fact.
			continue
		}
		if err == nil {
			var upstreamID string
			if upstreamID, _, err = s.graph.IngestFact(ctx, fact, entity); err == nil {
				err = s.store.UpdateFactUpstream(ctx, fact.ID, upstreamID, "")
			}
		}
		if err != nil {
			failure = errors.Join(failure, fmt.Errorf("index fact %s: %w", fact.ID, err))
			continue
		}
		indexed++
	}
	relations, err := s.store.UnindexedExplicitRelations(ctx)
	if err != nil {
		return indexed, errors.Join(failure, fmt.Errorf("list unindexed relations: %w", err))
	}
	for _, relation := range relations {
		from, err := s.store.GetEntity(ctx, relation.FromID)
		var to model.Entity
		if err == nil {
			to, err = s.store.GetEntity(ctx, relation.ToID)
		}
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err == nil {
			var upstreamID string
			if upstreamID, err = s.graph.IngestRelation(ctx, relation, from, to); err == nil {
				err = s.store.UpdateRelationUpstream(ctx, relation.ID, upstreamID)
			}
		}
		if err != nil {
			failure = errors.Join(failure, fmt.Errorf("index relation %s: %w", relation.ID, err))
			continue
		}
		indexed++
	}
	return indexed, failure
}

func (s *Service) retractSettledOrphans(ctx context.Context) (int, error) {
	documents, err := s.store.DocumentsWithOrphanedClaims(ctx)
	if err != nil {
		return 0, fmt.Errorf("list documents with orphaned claims: %w", err)
	}
	var failure error
	for _, id := range documents {
		if err := s.retractOrphanedDerivedGraph(ctx, id); err != nil {
			failure = errors.Join(failure, fmt.Errorf("retract orphans of %s: %w", id, err))
		}
	}
	return len(documents), failure
}
