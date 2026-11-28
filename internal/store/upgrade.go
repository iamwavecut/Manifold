package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/iamwavecut/Manifold/internal/model"
)

// The queries below find what an earlier release, or an interrupted run,
// left outside the index or the consistent public graph. Manifold's catch-up
// pass reads them at every start.

// DocumentRevision names the current revision of a document.
type DocumentRevision struct {
	ID       string
	Revision string
}

// unfinishedDocumentJob is true while a sync or delete job of the document
// aliased as alias is waiting or running.
func unfinishedDocumentJob(alias string) string {
	return fmt.Sprintf(`EXISTS (
		SELECT 1 FROM jobs j
		WHERE j.resource_type = 'document' AND j.resource_id = %s.id
			AND j.kind IN ('document.sync', 'document.delete')
			AND j.status IN ('accepted', 'indexing', 'extracting')
	)`, alias)
}

// SettledDocumentRevisions lists the current revision of every live ready
// document that no sync or delete job is about to change.
func (s *Store) SettledDocumentRevisions(ctx context.Context) ([]DocumentRevision, error) {
	var revisions []DocumentRevision
	err := eachRow(ctx, s.db, `
		SELECT d.id, d.revision FROM documents d
		WHERE d.deleted = 0 AND d.status = ? AND NOT `+unfinishedDocumentJob("d")+`
		ORDER BY d.id`,
		func(rows *sql.Rows) error {
			var id string
			var revision int
			if err := rows.Scan(&id, &revision); err != nil {
				return err
			}
			revisions = append(revisions, DocumentRevision{ID: id, Revision: fmt.Sprintf("r%d", revision)})
			return nil
		}, string(model.JobReady))
	return revisions, err
}

// UnindexedExplicitFacts lists the active facts recorded through the API
// that have no index ID, because the graph call failed when they were made.
func (s *Store) UnindexedExplicitFacts(ctx context.Context) ([]model.Fact, error) {
	var facts []model.Fact
	err := eachRow(ctx, s.db, `
		SELECT id, entity_id, predicate, object, origin, status, confidence,
			COALESCE(source_document_id, ''), valid_from, valid_until
		FROM facts WHERE origin <> ? AND upstream_id = '' AND status = 'active'
		ORDER BY created_at, id`,
		func(rows *sql.Rows) error {
			var fact model.Fact
			var validFrom string
			var validUntil sql.NullString
			if err := rows.Scan(&fact.ID, &fact.EntityID, &fact.Predicate, &fact.Object, &fact.Origin, &fact.Status,
				&fact.Confidence, &fact.SourceDocumentID, &validFrom, &validUntil); err != nil {
				return err
			}
			fact.ValidFrom = parseTime(validFrom)
			fact.ValidUntil = parseOptionalTime(validUntil)
			facts = append(facts, fact)
			return nil
		}, derivedGraphOrigin)
	return facts, err
}

// UnindexedExplicitRelations lists the active relations recorded through the
// API that have no index ID.
func (s *Store) UnindexedExplicitRelations(ctx context.Context) ([]model.Relation, error) {
	var relations []model.Relation
	err := eachRow(ctx, s.db, `
		SELECT id, from_entity_id, to_entity_id, predicate, origin, status, confidence
		FROM relations WHERE origin <> ? AND upstream_id = '' AND status = 'active'
		ORDER BY created_at, id`,
		func(rows *sql.Rows) error {
			var relation model.Relation
			if err := rows.Scan(&relation.ID, &relation.FromID, &relation.ToID, &relation.Predicate,
				&relation.Origin, &relation.Status, &relation.Confidence); err != nil {
				return err
			}
			relations = append(relations, relation)
			return nil
		}, derivedGraphOrigin)
	return relations, err
}

// DocumentsWithOrphanedClaims lists the documents that once claimed a derived
// fact or relation that no live document claims now and that is still not
// retracted. It leaves out a document that a waiting or running job is about
// to change: until that job projects the current revision, the claims of the
// previous one are not current but not stale either.
func (s *Store) DocumentsWithOrphanedClaims(ctx context.Context) ([]string, error) {
	var documents []string
	err := eachRow(ctx, s.db, `
		SELECT DISTINCT c.document_id FROM derived_graph_claims c
		JOIN documents d ON d.id = c.document_id
		LEFT JOIN facts f ON c.object_kind = 'fact' AND f.id = c.object_id
		LEFT JOIN relations r ON c.object_kind = 'relation' AND r.id = c.object_id
		WHERE (d.deleted = 1 OR d.status = ?) AND NOT `+unfinishedDocumentJob("d")+`
			AND (
				(f.id IS NOT NULL AND f.origin = ? AND f.status <> 'retracted' AND f.upstream_id <> ''
					AND NOT `+currentDerivedGraphClaim("f", "fact")+`)
				OR (r.id IS NOT NULL AND r.origin = ? AND r.status <> 'retracted' AND r.upstream_id <> ''
					AND NOT `+currentDerivedGraphClaim("r", "relation")+`)
			)
		ORDER BY c.document_id`,
		func(rows *sql.Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			documents = append(documents, id)
			return nil
		}, string(model.JobReady), derivedGraphOrigin, derivedGraphOrigin)
	return documents, err
}
