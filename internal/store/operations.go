package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/iamwavecut/Manifold/internal/model"
)

func (s *Store) EnqueueJob(ctx context.Context, job model.Job) (model.Job, error) {
	timestamp := now()
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		if err := insertJob(ctx, tx, job, timestamp); err != nil {
			return err
		}
		return bumpVersion(ctx, tx)
	})
	if err != nil {
		return model.Job{}, err
	}
	job.Status = model.JobAccepted
	job.CreatedAt = parseTime(timestamp)
	job.UpdatedAt = job.CreatedAt
	return job, nil
}

func (s *Store) GetDocumentAny(ctx context.Context, id string) (model.Document, bool, error) {
	var doc model.Document
	var metadata, tags, created, updated string
	var revision, etag, deleted int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, COALESCE(folder_id, ''), title, format, content, content_hash, revision, status,
			ov_uri, brain_id, metadata_json, tags_json, etag, deleted, created_at, updated_at
		FROM documents WHERE id = ?`, id).Scan(
		&doc.ID, &doc.FolderID, &doc.Title, &doc.Format, &doc.Content, &doc.ContentHash, &revision, &doc.Status,
		&doc.OVURI, &doc.BrainID, &metadata, &tags, &etag, &deleted, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Document{}, false, ErrNotFound
	}
	if err != nil {
		return model.Document{}, false, err
	}
	doc.Revision = fmt.Sprintf("r%d", revision)
	doc.Metadata = parseMap(metadata)
	doc.Tags = parseStrings(tags)
	doc.ETag = fmt.Sprintf(`"v%d"`, etag)
	doc.CreatedAt = parseTime(created)
	doc.UpdatedAt = parseTime(updated)
	return doc, deleted != 0, nil
}

func (s *Store) ListAllDocuments(ctx context.Context) ([]model.Document, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM documents WHERE deleted = 0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var docs []model.Document
	for _, id := range ids {
		doc, err := s.GetDocument(ctx, id, true)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

func (s *Store) SetDocumentOVURI(ctx context.Context, id, uri string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE documents SET ov_uri = ?, updated_at = ? WHERE id = ?`, uri, now(), id)
	return err
}

func (s *Store) UpdateFactUpstream(ctx context.Context, id, upstreamID, status string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE facts SET upstream_id = CASE WHEN ? = '' THEN upstream_id ELSE ? END,
			status = CASE WHEN ? = '' THEN status ELSE ? END WHERE id = ?`,
		upstreamID, upstreamID, status, status, id)
	return err
}

func (s *Store) UpdateRelationUpstream(ctx context.Context, id, upstreamID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE relations SET upstream_id = ? WHERE id = ?`, upstreamID, id)
	return err
}

func (s *Store) GetFact(ctx context.Context, id string) (model.Fact, error) {
	facts, err := s.ListFacts(ctx, "", 10_000)
	if err != nil {
		return model.Fact{}, err
	}
	for _, fact := range facts {
		if fact.ID == id {
			return fact, nil
		}
	}
	return model.Fact{}, ErrNotFound
}

func (s *Store) RetractFact(ctx context.Context, id string) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE facts SET status = 'retracted' WHERE id = ?`, id)
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

func (s *Store) ListConflicts(ctx context.Context, limit int) ([]model.Conflict, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, fact_a_id, fact_b_id, status, resolution, created_at, updated_at
		FROM conflicts ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var conflicts []model.Conflict
	for rows.Next() {
		var conflict model.Conflict
		var created, updated string
		if err := rows.Scan(&conflict.ID, &conflict.FactAID, &conflict.FactBID, &conflict.Status,
			&conflict.Resolution, &created, &updated); err != nil {
			return nil, err
		}
		conflict.CreatedAt = parseTime(created)
		conflict.UpdatedAt = parseTime(updated)
		conflicts = append(conflicts, conflict)
	}
	return conflicts, rows.Err()
}

func (s *Store) ResolveConflict(ctx context.Context, id, resolution string) (model.Conflict, error) {
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE conflicts SET status = 'resolved', resolution = ?, updated_at = ? WHERE id = ?`,
			resolution, now(), id)
		if err != nil {
			return err
		}
		count, _ := res.RowsAffected()
		if count == 0 {
			return ErrNotFound
		}
		return bumpVersion(ctx, tx)
	})
	if err != nil {
		return model.Conflict{}, err
	}
	conflicts, err := s.ListConflicts(ctx, 10_000)
	if err != nil {
		return model.Conflict{}, err
	}
	for _, conflict := range conflicts {
		if conflict.ID == id {
			return conflict, nil
		}
	}
	return model.Conflict{}, ErrNotFound
}

func (s *Store) ListSources(ctx context.Context, limit int) ([]model.Source, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.source_document_id, f.source_revision, MAX(f.object), MAX(f.origin),
			MAX(f.confidence), MAX(f.created_at)
		FROM facts f WHERE f.source_document_id IS NOT NULL
		GROUP BY f.source_document_id, f.source_revision
		ORDER BY MAX(f.created_at) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sources []model.Source
	for rows.Next() {
		var source model.Source
		var revision sql.NullInt64
		var created string
		if err := rows.Scan(&source.DocumentID, &revision, &source.Excerpt, &source.Method,
			&source.Confidence, &created); err != nil {
			return nil, err
		}
		source.Revision = optionalRevision(revision)
		source.ID = source.DocumentID
		if source.Revision != "" {
			source.ID += "-" + source.Revision
		}
		source.IngestedAt = parseTime(created)
		sources = append(sources, source)
	}
	return sources, rows.Err()
}
