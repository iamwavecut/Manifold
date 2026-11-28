package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/iamwavecut/Manifold/internal/engine"
	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/upstream"
)

// The store is the seed source of a new embedded index: it yields the graph
// records, the live folder tree and documents, and the graph result already
// projected for each ready document revision. A fact or relation needs its
// upstream ID, which search hits are mapped back by. An entity without one
// was created through the API; seedKey links its facts by its own ID.

// seedKey is the SQL expression of an entity's seed key.
const seedKey = `CASE WHEN %[1]s.upstream_id != '' THEN %[1]s.upstream_id ELSE 'manifold:' || %[1]s.id END`

var _ engine.SeedSource = (*Store)(nil)

func (s *Store) SeedEntities(ctx context.Context, fn func(engine.SeedEntity) error) error {
	// Mapped entities come first, so an unmapped one finds them by name.
	return eachRow(ctx, s.db, `
		SELECT `+fmt.Sprintf(seedKey, "e")+`, e.upstream_id, e.name, e.kind FROM entities e
		ORDER BY e.upstream_id = '', e.upstream_id, e.id`,
		func(rows *sql.Rows) error {
			var entity engine.SeedEntity
			if err := rows.Scan(&entity.Key, &entity.UpstreamID, &entity.Name, &entity.Kind); err != nil {
				return err
			}
			return fn(entity)
		})
}

func (s *Store) SeedFacts(ctx context.Context, fn func(engine.SeedFact) error) error {
	// A Brain fact can back both an explicit and a derived record; the derived
	// one comes first and wins.
	return eachRow(ctx, s.db, `
		SELECT f.upstream_id, `+fmt.Sprintf(seedKey, "e")+`, f.predicate, f.object, f.status, f.confidence,
			f.valid_from, f.valid_until, COALESCE(f.source_document_id, '')
		FROM facts f JOIN entities e ON e.id = f.entity_id
		WHERE f.upstream_id != ''
		ORDER BY f.upstream_id, f.origin != 'derived', f.id`,
		func(rows *sql.Rows) error {
			var fact engine.SeedFact
			var validFrom string
			var validUntil sql.NullString
			if err := rows.Scan(&fact.UpstreamID, &fact.EntityKey, &fact.Predicate, &fact.Object,
				&fact.Status, &fact.Confidence, &validFrom, &validUntil, &fact.SourceDocumentID); err != nil {
				return err
			}
			fact.ValidFrom = parseTime(validFrom)
			fact.ValidUntil = parseOptionalTime(validUntil)
			return fn(fact)
		})
}

func (s *Store) SeedRelations(ctx context.Context, fn func(engine.SeedRelation) error) error {
	return eachRow(ctx, s.db, `
		SELECT r.upstream_id, `+fmt.Sprintf(seedKey, "a")+`, `+fmt.Sprintf(seedKey, "b")+`,
			r.predicate, r.status, r.confidence
		FROM relations r
		JOIN entities a ON a.id = r.from_entity_id
		JOIN entities b ON b.id = r.to_entity_id
		WHERE r.upstream_id != ''
		ORDER BY r.upstream_id, r.origin != 'derived', r.id`,
		func(rows *sql.Rows) error {
			var relation engine.SeedRelation
			if err := rows.Scan(&relation.UpstreamID, &relation.FromKey, &relation.ToKey,
				&relation.Predicate, &relation.Status, &relation.Confidence); err != nil {
				return err
			}
			return fn(relation)
		})
}

func (s *Store) SeedFolders(ctx context.Context, fn func(string) error) error {
	return eachRow(ctx, s.db, `
		WITH RECURSIVE tree(id, path) AS (
			SELECT id, id FROM folders WHERE parent_id IS NULL
			UNION ALL
			SELECT f.id, t.path || '/' || f.id FROM folders f JOIN tree t ON f.parent_id = t.id
		)
		SELECT path FROM tree ORDER BY path`,
		func(rows *sql.Rows) error {
			var path string
			if err := rows.Scan(&path); err != nil {
				return err
			}
			return fn("viking://resources/manifold/" + path + "/")
		})
}

func (s *Store) SeedDocuments(ctx context.Context, fn func(engine.SeedDocument) error) error {
	return eachRow(ctx, s.db, `
		SELECT ov_uri, content FROM documents WHERE deleted = 0 AND ov_uri != '' ORDER BY id`,
		func(rows *sql.Rows) error {
			var document engine.SeedDocument
			if err := rows.Scan(&document.URI, &document.Content); err != nil {
				return err
			}
			return fn(document)
		})
}

// SeedIngests yields the graph result of every ready document at its
// current revision, rebuilt from the active claims and fact snapshots, so a
// repeated ingest projects exactly what the store holds.
func (s *Store) SeedIngests(ctx context.Context, fn func(engine.SeedIngest) error) error {
	type readyDocument struct {
		id       string
		revision int
	}
	var documents []readyDocument
	err := eachRow(ctx, s.db, `
		SELECT id, revision FROM documents WHERE deleted = 0 AND status = ? ORDER BY id`,
		func(rows *sql.Rows) error {
			var document readyDocument
			if err := rows.Scan(&document.id, &document.revision); err != nil {
				return err
			}
			documents = append(documents, document)
			return nil
		}, string(model.JobReady))
	if err != nil {
		return err
	}
	for _, document := range documents {
		result, err := s.projectedResult(ctx, document.id, document.revision)
		if err != nil {
			return fmt.Errorf("rebuild graph result of %s@r%d: %w", document.id, document.revision, err)
		}
		if err := fn(engine.SeedIngest{DocumentID: document.id, Revision: document.revision, Result: result}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) projectedResult(ctx context.Context, documentID string, revision int) (upstream.GraphIngestResult, error) {
	revisionName := fmt.Sprintf("r%d", revision)
	result := upstream.GraphIngestResult{
		DocumentID: documentID,
		OriginURI:  "manifold://documents/" + documentID + "@" + revisionName,
		Revision:   revisionName,
		Entities:   []upstream.GraphEntity{},
		Facts:      []upstream.GraphFact{},
		Relations:  []upstream.GraphRelation{},
	}
	err := eachRow(ctx, s.db, `
		SELECT DISTINCT e.upstream_id, e.name, e.kind
		FROM derived_graph_claims c JOIN entities e ON e.id = c.object_id
		WHERE c.document_id = ? AND c.revision = ? AND c.active = 1 AND c.object_kind = 'entity'
			AND e.upstream_id != ''
		ORDER BY e.upstream_id`,
		func(rows *sql.Rows) error {
			var entity upstream.GraphEntity
			if err := rows.Scan(&entity.BrainID, &entity.Name, &entity.Type); err != nil {
				return err
			}
			result.Entities = append(result.Entities, entity)
			result.EntityIDs = append(result.EntityIDs, entity.BrainID)
			return nil
		}, documentID, revision)
	if err != nil {
		return result, err
	}
	seenFacts := map[string]bool{}
	err = eachRow(ctx, s.db, `
		SELECT f.upstream_id, e.upstream_id, snap.predicate, snap.object, snap.status, snap.confidence,
			snap.valid_from, snap.valid_until
		FROM derived_graph_claims c
		JOIN derived_graph_fact_snapshots snap ON snap.object_id = c.object_id
			AND snap.document_id = c.document_id AND snap.revision = c.revision
			AND snap.source_key = c.source_key AND snap.origin_key = c.origin_key
		JOIN facts f ON f.id = c.object_id
		JOIN entities e ON e.id = snap.entity_id
		WHERE c.document_id = ? AND c.revision = ? AND c.active = 1 AND c.object_kind = 'fact'
			AND f.upstream_id != '' AND e.upstream_id != ''
		ORDER BY f.upstream_id, c.source_key, c.origin_key`,
		func(rows *sql.Rows) error {
			fact := upstream.GraphFact{SourceDocumentID: documentID}
			var validFrom string
			var validUntil sql.NullString
			if err := rows.Scan(&fact.BrainID, &fact.EntityBrainID, &fact.Predicate, &fact.Object,
				&fact.Status, &fact.Confidence, &validFrom, &validUntil); err != nil {
				return err
			}
			if seenFacts[fact.BrainID] {
				return nil
			}
			seenFacts[fact.BrainID] = true
			fact.ValidFrom = parseTime(validFrom)
			fact.ValidUntil = parseOptionalTime(validUntil)
			result.Facts = append(result.Facts, fact)
			result.FactIDs = append(result.FactIDs, fact.BrainID)
			return nil
		}, documentID, revision)
	if err != nil {
		return result, err
	}
	err = eachRow(ctx, s.db, `
		SELECT DISTINCT r.upstream_id, a.upstream_id, b.upstream_id, r.predicate, r.status, r.confidence
		FROM derived_graph_claims c
		JOIN relations r ON r.id = c.object_id
		JOIN entities a ON a.id = r.from_entity_id
		JOIN entities b ON b.id = r.to_entity_id
		WHERE c.document_id = ? AND c.revision = ? AND c.active = 1 AND c.object_kind = 'relation'
			AND r.upstream_id != '' AND a.upstream_id != '' AND b.upstream_id != ''
		ORDER BY r.upstream_id`,
		func(rows *sql.Rows) error {
			relation := upstream.GraphRelation{SourceDocumentID: documentID}
			if err := rows.Scan(&relation.BrainID, &relation.FromEntityBrainID, &relation.ToEntityBrainID,
				&relation.Predicate, &relation.Status, &relation.Confidence); err != nil {
				return err
			}
			result.Relations = append(result.Relations, relation)
			result.EdgeIDs = append(result.EdgeIDs, relation.BrainID)
			return nil
		}, documentID, revision)
	return result, err
}

// eachRow calls fn for every row of query. The store has one connection, so
// fn must not query the store.
func eachRow(ctx context.Context, db *sql.DB, query string, fn func(*sql.Rows) error, args ...any) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
