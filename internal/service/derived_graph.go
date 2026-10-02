package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/selector"
	"github.com/iamwavecut/Manifold/internal/store"
	"github.com/iamwavecut/Manifold/internal/upstream"
)

var (
	ErrDerivedGraphIncomplete     = errors.New("Brain graph result omitted committed record details")
	ErrDerivedGraphSourceMismatch = errors.New("Brain graph result source does not match the captured document revision")
)

func (s *Service) materializeGraphDocument(ctx context.Context, doc model.Document, result upstream.GraphIngestResult) error {
	if result.Revision != "" && result.Revision != doc.Revision {
		return store.ErrDocumentSyncSuperseded
	}
	expectedOrigin := fmt.Sprintf("manifold://documents/%s@%s", doc.ID, doc.Revision)
	if result.OriginURI != "" && result.OriginURI != expectedOrigin {
		return ErrDerivedGraphSourceMismatch
	}
	if !declaredGraphIDsHaveDetails(result.EntityIDs, graphEntityIDs(result.Entities)) ||
		!declaredGraphIDsHaveDetails(result.FactIDs, graphFactIDs(result.Facts)) ||
		!declaredGraphIDsHaveDetails(result.EdgeIDs, graphRelationIDs(result.Relations)) {
		return ErrDerivedGraphIncomplete
	}

	projection := store.DerivedGraphProjection{
		DocumentID: doc.ID,
		Revision:   doc.Revision,
		Entities:   make([]store.DerivedGraphEntity, 0, len(result.Entities)),
		Facts:      make([]store.DerivedGraphFact, 0, len(result.Facts)),
		Relations:  make([]store.DerivedGraphRelation, 0, len(result.Relations)),
	}
	for _, entity := range result.Entities {
		projection.Entities = append(projection.Entities, store.DerivedGraphEntity{
			CommitRef: entity.BrainID,
			Name:      entity.Name,
			Kind:      entity.Type,
		})
	}
	for _, fact := range result.Facts {
		projection.Facts = append(projection.Facts, store.DerivedGraphFact{
			CommitRef:       fact.BrainID,
			EntityCommitRef: fact.EntityBrainID,
			Predicate:       fact.Predicate,
			Object:          fact.Object,
			Status:          fact.Status,
			Confidence:      fact.Confidence,
			ValidFrom:       fact.ValidFrom,
			ValidUntil:      fact.ValidUntil,
		})
	}
	for _, relation := range result.Relations {
		projection.Relations = append(projection.Relations, store.DerivedGraphRelation{
			CommitRef:           relation.BrainID,
			FromEntityCommitRef: relation.FromEntityBrainID,
			ToEntityCommitRef:   relation.ToEntityBrainID,
			Predicate:           relation.Predicate,
			Status:              relation.Status,
			Confidence:          relation.Confidence,
		})
	}
	return s.store.ProjectDerivedGraph(ctx, projection)
}

func (s *Service) canonicalizeProjectedFactHit(ctx context.Context, hit model.SearchHit) (model.SearchHit, bool, error) {
	return s.canonicalizeProjectedFactHitWithOptions(ctx, hit, false, "")
}

func (s *Service) canonicalizeProjectedFactHitWithOptions(
	ctx context.Context,
	hit model.SearchHit,
	includeHistory bool,
	scopeGlob string,
) (model.SearchHit, bool, error) {
	records, err := s.store.ListDerivedFactSnapshotsByUpstreamID(ctx, hit.ID, includeHistory)
	if errors.Is(err, store.ErrNotFound) {
		return model.SearchHit{}, false, nil
	}
	if err != nil {
		return model.SearchHit{}, false, err
	}
	type candidate struct {
		record store.DerivedFactSnapshot
		path   string
	}
	candidates := make([]candidate, 0, len(records))
	for _, record := range records {
		fact := record.Fact
		if hit.Revision != "" && fact.SourceRevision != hit.Revision {
			continue
		}
		path := ""
		if fact.SourceDocumentID != "" {
			path, err = s.store.DocumentPath(ctx, fact.SourceDocumentID)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return model.SearchHit{}, false, err
			}
		}
		if scopeGlob != "" && (path == "" || !selector.Match(scopeGlob, path)) {
			continue
		}
		candidates = append(candidates, candidate{record: record, path: path})
	}
	if len(candidates) == 0 {
		return model.SearchHit{}, false, nil
	}
	selected := candidates[0]
	if hit.Revision == "" && strings.TrimSpace(hit.Snippet) != "" {
		searchSnippet := strings.ToLower(strings.TrimSpace(hit.Snippet))
		for _, candidate := range candidates {
			snapshotSnippet := strings.ToLower(strings.TrimSpace(fmt.Sprintf(
				"%s %s", candidate.record.Fact.Predicate, candidate.record.Fact.Object)))
			if snapshotSnippet == searchSnippet {
				selected = candidate
				break
			}
		}
	}
	fact := selected.record.Fact
	mapped := hit
	mapped.Kind = "fact"
	mapped.ID = fact.ID
	mapped.Title = selected.record.EntityName
	if mapped.Title == "" {
		mapped.Title = fact.EntityID
	}
	mapped.Snippet = fmt.Sprintf("%s %s", fact.Predicate, fact.Object)
	mapped.CanonicalRef = "manifold://facts/" + fact.ID
	mapped.Revision = fact.SourceRevision
	mapped.Path = selected.path
	mapped.UpstreamSourceRef = ""
	return mapped, true, nil
}

func declaredGraphIDsHaveDetails(declared, details []string) bool {
	if len(declared) > len(details) {
		return false
	}
	known := make(map[string]struct{}, len(details))
	for _, id := range details {
		if id != "" {
			known[id] = struct{}{}
		}
	}
	for _, id := range declared {
		if id == "" {
			return false
		}
		if _, ok := known[id]; !ok {
			return false
		}
	}
	return true
}

func graphEntityIDs(entities []upstream.GraphEntity) []string {
	ids := make([]string, 0, len(entities))
	for _, entity := range entities {
		ids = append(ids, entity.BrainID)
	}
	return ids
}

func graphFactIDs(facts []upstream.GraphFact) []string {
	ids := make([]string, 0, len(facts))
	for _, fact := range facts {
		ids = append(ids, fact.BrainID)
	}
	return ids
}

func graphRelationIDs(relations []upstream.GraphRelation) []string {
	ids := make([]string, 0, len(relations))
	for _, relation := range relations {
		ids = append(ids, relation.BrainID)
	}
	return ids
}
