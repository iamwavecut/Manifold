package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/model"
)

func (s *Store) CreateEntity(ctx context.Context, entity model.Entity) (model.Entity, error) {
	timestamp := now()
	entity.CreatedAt = parseTime(timestamp)
	entity.UpdatedAt = entity.CreatedAt
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO entities(id, name, kind, upstream_id, metadata_json, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			entity.ID, entity.Name, entity.Kind, entity.UpstreamID, jsonString(entity.Metadata), timestamp, timestamp)
		if err != nil {
			return err
		}
		return bumpVersion(ctx, tx)
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return model.Entity{}, ErrConflict
		}
		return model.Entity{}, err
	}
	return entity, nil
}

func (s *Store) GetEntity(ctx context.Context, id string) (model.Entity, error) {
	var entity model.Entity
	var metadata, created, updated string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, kind, upstream_id, metadata_json, created_at, updated_at
		FROM entities WHERE id = ?`, id).Scan(
		&entity.ID, &entity.Name, &entity.Kind, &entity.UpstreamID, &metadata, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Entity{}, ErrNotFound
	}
	if err != nil {
		return model.Entity{}, err
	}
	entity.Metadata = parseMap(metadata)
	entity.CreatedAt = parseTime(created)
	entity.UpdatedAt = parseTime(updated)
	return entity, nil
}

func (s *Store) ListEntities(ctx context.Context, query string, limit int) ([]model.Entity, error) {
	sqlQuery := `
		SELECT id, name, kind, upstream_id, metadata_json, created_at, updated_at
		FROM entities`
	args := []any{}
	if query != "" {
		sqlQuery += ` WHERE id LIKE ? OR name LIKE ?`
		value := "%" + query + "%"
		args = append(args, value, value)
	}
	sqlQuery += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entities []model.Entity
	for rows.Next() {
		var entity model.Entity
		var metadata, created, updated string
		if err := rows.Scan(&entity.ID, &entity.Name, &entity.Kind, &entity.UpstreamID, &metadata, &created, &updated); err != nil {
			return nil, err
		}
		entity.Metadata = parseMap(metadata)
		entity.CreatedAt = parseTime(created)
		entity.UpdatedAt = parseTime(updated)
		entities = append(entities, entity)
	}
	return entities, rows.Err()
}

func (s *Store) CreateFact(ctx context.Context, fact model.Fact) (model.Fact, error) {
	timestamp := now()
	fact.CreatedAt = parseTime(timestamp)
	var validUntil any
	if fact.ValidUntil != nil {
		validUntil = fact.ValidUntil.UTC().Format(time.RFC3339Nano)
	}
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		var conflictingID string
		queryErr := tx.QueryRowContext(ctx, `
			SELECT id FROM facts
			WHERE entity_id = ? AND predicate = ? AND object <> ? AND status = 'active'
			ORDER BY created_at DESC LIMIT 1`,
			fact.EntityID, fact.Predicate, fact.Object).Scan(&conflictingID)
		if queryErr != nil && !errors.Is(queryErr, sql.ErrNoRows) {
			return queryErr
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO facts(id, entity_id, predicate, object, origin, status, confidence,
				source_document_id, source_revision, upstream_id, valid_from, valid_until, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?)`,
			fact.ID, fact.EntityID, fact.Predicate, fact.Object, fact.Origin, fact.Status, fact.Confidence,
			fact.SourceDocumentID, nullableRevision(fact.SourceRevision), fact.UpstreamID,
			fact.ValidFrom.UTC().Format(time.RFC3339Nano), validUntil, timestamp); err != nil {
			return err
		}
		if conflictingID != "" {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO conflicts(id, fact_a_id, fact_b_id, status, created_at, updated_at)
				VALUES (?, ?, ?, 'unresolved', ?, ?)`,
				identity.NewXID(), conflictingID, fact.ID, timestamp, timestamp); err != nil {
				return err
			}
		}
		return bumpVersion(ctx, tx)
	})
	if err != nil {
		return model.Fact{}, err
	}
	return fact, nil
}

func (s *Store) ListFacts(ctx context.Context, entityID string, limit int) ([]model.Fact, error) {
	query := `
		SELECT id, entity_id, predicate, object, origin, status, confidence,
			COALESCE(source_document_id, ''), source_revision, upstream_id, valid_from, valid_until, created_at
		FROM facts`
	args := []any{}
	if entityID != "" {
		query += ` WHERE entity_id = ?`
		args = append(args, entityID)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var facts []model.Fact
	for rows.Next() {
		var fact model.Fact
		var sourceRevision sql.NullInt64
		var validFrom, created string
		var validUntil sql.NullString
		if err := rows.Scan(&fact.ID, &fact.EntityID, &fact.Predicate, &fact.Object, &fact.Origin, &fact.Status,
			&fact.Confidence, &fact.SourceDocumentID, &sourceRevision, &fact.UpstreamID, &validFrom, &validUntil, &created); err != nil {
			return nil, err
		}
		fact.SourceRevision = optionalRevision(sourceRevision)
		fact.ValidFrom = parseTime(validFrom)
		fact.ValidUntil = parseOptionalTime(validUntil)
		fact.CreatedAt = parseTime(created)
		facts = append(facts, fact)
	}
	return facts, rows.Err()
}

func (s *Store) CreateRelation(ctx context.Context, relation model.Relation) (model.Relation, error) {
	timestamp := now()
	relation.CreatedAt = parseTime(timestamp)
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO relations(id, from_entity_id, to_entity_id, predicate, origin, status, confidence, upstream_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			relation.ID, relation.FromID, relation.ToID, relation.Predicate, relation.Origin, relation.Status,
			relation.Confidence, relation.UpstreamID, timestamp); err != nil {
			return err
		}
		return bumpVersion(ctx, tx)
	})
	if err != nil {
		return model.Relation{}, err
	}
	return relation, nil
}

func (s *Store) ListRelations(ctx context.Context, entityID string, limit int) ([]model.Relation, error) {
	query := `
		SELECT id, from_entity_id, to_entity_id, predicate, origin, status, confidence, upstream_id, created_at
		FROM relations`
	args := []any{}
	if entityID != "" {
		query += ` WHERE from_entity_id = ? OR to_entity_id = ?`
		args = append(args, entityID, entityID)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var relations []model.Relation
	for rows.Next() {
		var relation model.Relation
		var created string
		if err := rows.Scan(&relation.ID, &relation.FromID, &relation.ToID, &relation.Predicate, &relation.Origin,
			&relation.Status, &relation.Confidence, &relation.UpstreamID, &created); err != nil {
			return nil, err
		}
		relation.CreatedAt = parseTime(created)
		relations = append(relations, relation)
	}
	return relations, rows.Err()
}

func (s *Store) DeleteRelation(ctx context.Context, id string) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE id = ?`, id)
		if err != nil {
			return err
		}
		count, _ := res.RowsAffected()
		if count == 0 {
			return ErrNotFound
		}
		return bumpVersion(ctx, tx)
	})
}

func (s *Store) GraphPath(ctx context.Context, from, to string, maxDepth int) ([]model.Relation, error) {
	type step struct {
		entity string
		path   []model.Relation
	}
	queue := []step{{entity: from}}
	visited := map[string]struct{}{from: {}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if len(current.path) >= maxDepth {
			continue
		}
		relations, err := s.ListRelations(ctx, current.entity, 1000)
		if err != nil {
			return nil, err
		}
		for _, relation := range relations {
			next := relation.ToID
			if next == current.entity {
				next = relation.FromID
			}
			path := append(slices.Clone(current.path), relation)
			if next == to {
				return path, nil
			}
			if _, ok := visited[next]; ok {
				continue
			}
			visited[next] = struct{}{}
			queue = append(queue, step{entity: next, path: path})
		}
	}
	return nil, ErrNotFound
}

func (s *Store) SearchDocuments(ctx context.Context, query string, limit int) ([]model.SearchHit, error) {
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return nil, nil
	}
	match := strings.Join(terms, " AND ")
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id, d.title, snippet(document_fts, 2, '<mark>', '</mark>', '…', 24), bm25(document_fts), d.revision
		FROM document_fts JOIN documents d ON d.id = document_fts.id
		WHERE document_fts MATCH ? AND d.deleted = 0
		ORDER BY bm25(document_fts) LIMIT ?`, match, limit)
	if err != nil {
		value := "%" + query + "%"
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, title, substr(content, 1, 320), 1.0, revision FROM documents
			WHERE deleted = 0 AND (title LIKE ? OR content LIKE ?) ORDER BY updated_at DESC LIMIT ?`,
			value, value, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []model.SearchHit
	for rows.Next() {
		var hit model.SearchHit
		var rank float64
		var revision int
		if err := rows.Scan(&hit.ID, &hit.Title, &hit.Snippet, &rank, &revision); err != nil {
			return nil, err
		}
		hit.Kind = "document"
		hit.Score = 1 / (1 + max(rank, 0))
		hit.Source = "manifold"
		hit.Revision = fmt.Sprintf("r%d", revision)
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

func nullableRevision(value string) any {
	if value == "" {
		return nil
	}
	return revisionNumber(value)
}

func optionalRevision(value sql.NullInt64) string {
	if !value.Valid {
		return ""
	}
	return fmt.Sprintf("r%d", value.Int64)
}
