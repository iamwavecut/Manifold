package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/model"
)

func (s *Store) SlugAvailable(ctx context.Context, resourceType, slug string) (bool, error) {
	table, err := slugTable(resourceType)
	if err != nil {
		return false, err
	}
	var exists int
	query := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE id = ?)`, table)
	if err := s.db.QueryRowContext(ctx, query, slug).Scan(&exists); err != nil {
		return false, err
	}
	if exists != 0 {
		return false, nil
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM rename_reservations WHERE resource_type = ? AND slug = ?)`,
		resourceType, slug).Scan(&exists); err != nil {
		return false, err
	}
	return exists == 0, nil
}

func (s *Store) AvailableSlug(ctx context.Context, resourceType, base string) (string, error) {
	base = identity.Slug(base)
	for suffix := 1; suffix < 10_000; suffix++ {
		candidate := base
		if suffix > 1 {
			tail := fmt.Sprintf("-%d", suffix)
			candidate = strings.TrimRight(base[:min(len(base), 63-len(tail))], "-") + tail
		}
		available, err := s.SlugAvailable(ctx, resourceType, candidate)
		if err != nil {
			return "", err
		}
		if available {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("unable to find an available slug")
}

func slugTable(resourceType string) (string, error) {
	switch resourceType {
	case "folder":
		return "folders", nil
	case "document":
		return "documents", nil
	case "entity":
		return "entities", nil
	case "api_key":
		return "api_keys", nil
	default:
		return "", fmt.Errorf("unsupported slug resource type %q", resourceType)
	}
}

func (s *Store) CreateFolder(ctx context.Context, folder model.Folder) (model.Folder, error) {
	timestamp := now()
	folder.CreatedAt = parseTime(timestamp)
	folder.UpdatedAt = folder.CreatedAt
	folder.ETag = `"v1"`
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO folders(id, name, parent_id, summary, metadata_json, created_at, updated_at)
			VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?)`,
			folder.ID, folder.Name, folder.ParentID, folder.Summary, jsonString(folder.Metadata), timestamp, timestamp)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return ErrConflict
			}
			return err
		}
		return bumpVersion(ctx, tx)
	})
	if err != nil {
		return model.Folder{}, err
	}
	folder.Path, _ = s.FolderPath(ctx, folder.ID)
	return folder, nil
}

func (s *Store) GetFolder(ctx context.Context, id string) (model.Folder, error) {
	var folder model.Folder
	var parent sql.NullString
	var metadata, created, updated string
	var etag int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, parent_id, summary, metadata_json, etag, created_at, updated_at
		FROM folders WHERE id = ?`, id).Scan(
		&folder.ID, &folder.Name, &parent, &folder.Summary, &metadata, &etag, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Folder{}, ErrNotFound
	}
	if err != nil {
		return model.Folder{}, err
	}
	folder.ParentID = parent.String
	folder.Metadata = parseMap(metadata)
	folder.ETag = fmt.Sprintf(`"v%d"`, etag)
	folder.CreatedAt = parseTime(created)
	folder.UpdatedAt = parseTime(updated)
	folder.Path, _ = s.FolderPath(ctx, folder.ID)
	return folder, nil
}

func (s *Store) ListFolders(ctx context.Context) ([]model.Folder, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, COALESCE(parent_id, ''), summary, metadata_json, etag, created_at, updated_at
		FROM folders ORDER BY parent_id, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var folders []model.Folder
	for rows.Next() {
		var folder model.Folder
		var metadata, created, updated string
		var etag int
		if err := rows.Scan(&folder.ID, &folder.Name, &folder.ParentID, &folder.Summary, &metadata, &etag, &created, &updated); err != nil {
			return nil, err
		}
		folder.Metadata = parseMap(metadata)
		folder.ETag = fmt.Sprintf(`"v%d"`, etag)
		folder.CreatedAt = parseTime(created)
		folder.UpdatedAt = parseTime(updated)
		folders = append(folders, folder)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range folders {
		folders[index].Path, err = s.FolderPath(ctx, folders[index].ID)
		if err != nil {
			return nil, err
		}
	}
	return folders, nil
}

func (s *Store) FolderPath(ctx context.Context, id string) (string, error) {
	const query = `
WITH RECURSIVE ancestors(id, parent_id, path) AS (
	SELECT id, parent_id, id FROM folders WHERE id = ?
	UNION ALL
	SELECT f.id, f.parent_id, f.id || '/' || a.path
	FROM folders f JOIN ancestors a ON a.parent_id = f.id
)
SELECT '/' || path FROM ancestors WHERE parent_id IS NULL LIMIT 1`
	var path string
	if err := s.db.QueryRowContext(ctx, query, id).Scan(&path); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	return path, nil
}

func (s *Store) DocumentPath(ctx context.Context, id string) (string, error) {
	doc, err := s.GetDocument(ctx, id, false)
	if err != nil {
		return "", err
	}
	if doc.FolderID == "" {
		return doc.ID, nil
	}
	folderPath, err := s.FolderPath(ctx, doc.FolderID)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(folderPath, "/") + "/" + doc.ID, nil
}

func (s *Store) GetDocumentByOVURI(ctx context.Context, uri string) (model.Document, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM documents WHERE ov_uri = ? AND deleted = 0`, uri).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Document{}, ErrNotFound
	}
	if err != nil {
		return model.Document{}, err
	}
	return s.GetDocument(ctx, id, false)
}

func (s *Store) GetDocumentByUpstreamRef(ctx context.Context, ref string) (model.Document, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM documents
		WHERE deleted = 0 AND (id = ? OR ov_uri = ? OR brain_id = ?)
		LIMIT 1`, ref, ref, ref).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Document{}, ErrNotFound
	}
	if err != nil {
		return model.Document{}, err
	}
	return s.GetDocument(ctx, id, false)
}

func (s *Store) DeleteFolder(ctx context.Context, id string) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM folders WHERE id = ?`, id)
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

func (s *Store) FolderEmpty(ctx context.Context, id string) (bool, error) {
	var children, documents int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM folders WHERE parent_id = ?`, id).Scan(&children); err != nil {
		return false, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents WHERE folder_id = ? AND deleted = 0`, id).Scan(&documents); err != nil {
		return false, err
	}
	return children == 0 && documents == 0, nil
}

func (s *Store) CreateDocument(ctx context.Context, doc model.Document, job model.Job) (model.Document, model.Job, error) {
	timestamp := now()
	prepareDocumentCreate(&doc, &job, timestamp)
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		if err := insertDocumentCreate(ctx, tx, doc, job, timestamp); err != nil {
			return err
		}
		return bumpVersion(ctx, tx)
	})
	if err != nil {
		return model.Document{}, model.Job{}, err
	}
	return doc, job, nil
}

func (s *Store) CreateDocumentAtPath(
	ctx context.Context,
	segments []string,
	doc model.Document,
	job model.Job,
) (model.Document, model.Job, []model.Folder, error) {
	timestamp := now()
	prepareDocumentCreate(&doc, &job, timestamp)
	created := make([]model.Folder, 0, len(segments))
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		parentID := ""
		for index, segment := range segments {
			var existingParent sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT parent_id FROM folders WHERE id = ?`, segment).Scan(&existingParent)
			switch {
			case err == nil:
				if existingParent.String != parentID {
					existingPath, _ := folderPathTx(ctx, tx, segment)
					return &FolderPathConflictError{
						Segment: segment, ExistingPath: strings.TrimPrefix(existingPath, "/"),
						RequestedPath: strings.Join(segments[:index+1], "/"),
					}
				}
			case errors.Is(err, sql.ErrNoRows):
				var reserved int
				if err := tx.QueryRowContext(ctx, `
					SELECT EXISTS(SELECT 1 FROM rename_reservations WHERE resource_type = 'folder' AND slug = ?)`,
					segment).Scan(&reserved); err != nil {
					return err
				}
				if reserved != 0 {
					return ErrConflict
				}
				folder := model.Folder{
					ID: segment, Name: segment, ParentID: parentID,
					Path: "/" + strings.Join(segments[:index+1], "/"), ETag: `"v1"`,
					CreatedAt: parseTime(timestamp), UpdatedAt: parseTime(timestamp),
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO folders(id, name, parent_id, summary, metadata_json, created_at, updated_at)
					VALUES (?, ?, NULLIF(?, ''), '', '{}', ?, ?)`,
					folder.ID, folder.Name, folder.ParentID, timestamp, timestamp); err != nil {
					return err
				}
				created = append(created, folder)
			default:
				return err
			}
			parentID = segment
		}
		doc.FolderID = parentID
		if err := insertDocumentCreate(ctx, tx, doc, job, timestamp); err != nil {
			return err
		}
		return bumpVersion(ctx, tx)
	})
	if err != nil {
		return model.Document{}, model.Job{}, nil, err
	}
	return doc, job, created, nil
}

func folderPathTx(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	const query = `
WITH RECURSIVE ancestors(id, parent_id, path) AS (
	SELECT id, parent_id, id FROM folders WHERE id = ?
	UNION ALL
	SELECT f.id, f.parent_id, f.id || '/' || a.path
	FROM folders f JOIN ancestors a ON a.parent_id = f.id
)
SELECT '/' || path FROM ancestors WHERE parent_id IS NULL LIMIT 1`
	var path string
	if err := tx.QueryRowContext(ctx, query, id).Scan(&path); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	return path, nil
}

func prepareDocumentCreate(doc *model.Document, job *model.Job, timestamp string) {
	doc.Revision = "r1"
	doc.Status = model.JobAccepted
	doc.ContentHash = contentHash(doc.Content)
	doc.ETag = `"v1"`
	doc.CreatedAt = parseTime(timestamp)
	doc.UpdatedAt = doc.CreatedAt
	job.Status = model.JobAccepted
	job.CreatedAt = doc.CreatedAt
	job.UpdatedAt = doc.CreatedAt
}

func insertDocumentCreate(ctx context.Context, tx *sql.Tx, doc model.Document, job model.Job, timestamp string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO documents(id, folder_id, title, format, content, content_hash, revision, status, ov_uri,
			metadata_json, tags_json, created_at, updated_at)
		VALUES (?, NULLIF(?, ''), ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?)`,
		doc.ID, doc.FolderID, doc.Title, doc.Format, doc.Content, doc.ContentHash, doc.Status, doc.OVURI,
		jsonString(doc.Metadata), jsonString(doc.Tags), timestamp, timestamp)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrConflict
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO revisions(document_id, revision, content, content_hash, ov_uri, created_at)
		VALUES (?, 1, ?, ?, ?, ?)`, doc.ID, doc.Content, doc.ContentHash, doc.OVURI, timestamp); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO document_fts(id, title, content) VALUES (?, ?, ?)`,
		doc.ID, doc.Title, doc.Content); err != nil {
		return err
	}
	return insertJob(ctx, tx, job, timestamp)
}

func (s *Store) GetDocument(ctx context.Context, id string, includeContent bool) (model.Document, error) {
	var doc model.Document
	var metadata, tags, created, updated string
	var revision, etag int
	var deleted int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, COALESCE(folder_id, ''), title, format, content, content_hash, revision, status,
			ov_uri, brain_id, metadata_json, tags_json, etag, deleted, created_at, updated_at
		FROM documents WHERE id = ?`, id).Scan(
		&doc.ID, &doc.FolderID, &doc.Title, &doc.Format, &doc.Content, &doc.ContentHash, &revision, &doc.Status,
		&doc.OVURI, &doc.BrainID, &metadata, &tags, &etag, &deleted, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) || deleted != 0 {
		return model.Document{}, ErrNotFound
	}
	if err != nil {
		return model.Document{}, err
	}
	if !includeContent {
		doc.Content = ""
	}
	doc.Revision = fmt.Sprintf("r%d", revision)
	doc.Metadata = parseMap(metadata)
	doc.Tags = parseStrings(tags)
	doc.ETag = fmt.Sprintf(`"v%d"`, etag)
	doc.CreatedAt = parseTime(created)
	doc.UpdatedAt = parseTime(updated)
	return doc, nil
}

func (s *Store) ListDocuments(ctx context.Context, folderID string) ([]model.Document, error) {
	query := `
		SELECT id, COALESCE(folder_id, ''), title, format, content_hash, revision, status,
			metadata_json, tags_json, etag, created_at, updated_at
		FROM documents WHERE deleted = 0`
	args := []any{}
	if folderID != "" {
		query += ` AND folder_id = ?`
		args = append(args, folderID)
	}
	query += ` ORDER BY updated_at DESC, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []model.Document
	for rows.Next() {
		var doc model.Document
		var metadata, tags, created, updated string
		var revision, etag int
		if err := rows.Scan(&doc.ID, &doc.FolderID, &doc.Title, &doc.Format, &doc.ContentHash, &revision,
			&doc.Status, &metadata, &tags, &etag, &created, &updated); err != nil {
			return nil, err
		}
		doc.Revision = fmt.Sprintf("r%d", revision)
		doc.Metadata = parseMap(metadata)
		doc.Tags = parseStrings(tags)
		doc.ETag = fmt.Sprintf(`"v%d"`, etag)
		doc.CreatedAt = parseTime(created)
		doc.UpdatedAt = parseTime(updated)
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

func (s *Store) UpdateDocument(ctx context.Context, id, ifMatch, title, content string, metadata map[string]any, tags []string, job model.Job) (model.Document, model.Job, error) {
	current, err := s.GetDocument(ctx, id, true)
	if err != nil {
		return model.Document{}, model.Job{}, err
	}
	if ifMatch != current.ETag {
		return current, model.Job{}, ErrConflict
	}
	if title == "" {
		title = current.Title
	}
	if content == "" {
		content = current.Content
	}
	if metadata == nil {
		metadata = current.Metadata
	}
	if tags == nil {
		tags = current.Tags
	}
	timestamp := now()
	revision := revisionNumber(current.Revision) + 1
	hash := contentHash(content)
	job.Status = model.JobAccepted
	err = s.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE documents SET title = ?, content = ?, content_hash = ?, revision = ?, status = ?,
				metadata_json = ?, tags_json = ?, etag = etag + 1, updated_at = ?
			WHERE id = ? AND etag = ? AND deleted = 0`,
			title, content, hash, revision, model.JobAccepted, jsonString(metadata), jsonString(tags), timestamp,
			id, revisionNumber(strings.Trim(current.ETag, `"`)[1:]))
		if err != nil {
			return err
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			return ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO revisions(document_id, revision, content, content_hash, ov_uri, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, id, revision, content, hash, current.OVURI, timestamp); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM document_fts WHERE id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO document_fts(id, title, content) VALUES (?, ?, ?)`, id, title, content); err != nil {
			return err
		}
		if err := insertJob(ctx, tx, job, timestamp); err != nil {
			return err
		}
		return bumpVersion(ctx, tx)
	})
	if err != nil {
		return current, model.Job{}, err
	}
	updated, err := s.GetDocument(ctx, id, true)
	return updated, job, err
}

func (s *Store) MarkDocumentDeleted(ctx context.Context, id, ifMatch string, job model.Job) (model.Job, error) {
	current, err := s.GetDocument(ctx, id, true)
	if err != nil {
		return model.Job{}, err
	}
	if current.ETag != ifMatch {
		return model.Job{}, ErrConflict
	}
	timestamp := now()
	job.Status = model.JobAccepted
	err = s.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE documents SET deleted = 1, status = ?, etag = etag + 1, updated_at = ? WHERE id = ?`,
			model.JobAccepted, timestamp, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM document_fts WHERE id = ?`, id); err != nil {
			return err
		}
		if err := insertJob(ctx, tx, job, timestamp); err != nil {
			return err
		}
		return bumpVersion(ctx, tx)
	})
	return job, err
}

func (s *Store) ListRevisions(ctx context.Context, documentID string) ([]model.Revision, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT document_id, revision, content, content_hash, ov_uri, snapshot_oid, created_at
		FROM revisions WHERE document_id = ? ORDER BY revision DESC`, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var revisions []model.Revision
	for rows.Next() {
		var rev model.Revision
		var number int
		var created string
		if err := rows.Scan(&rev.DocumentID, &number, &rev.Content, &rev.ContentHash, &rev.OVURI, &rev.SnapshotOID, &created); err != nil {
			return nil, err
		}
		rev.Revision = fmt.Sprintf("r%d", number)
		rev.CreatedAt = parseTime(created)
		revisions = append(revisions, rev)
	}
	return revisions, rows.Err()
}

func (s *Store) GetRevision(ctx context.Context, documentID string, revision int) (model.Revision, error) {
	var result model.Revision
	var created string
	err := s.db.QueryRowContext(ctx, `
		SELECT document_id, revision, content, content_hash, ov_uri, snapshot_oid, created_at
		FROM revisions WHERE document_id = ? AND revision = ?`, documentID, revision).Scan(
		&result.DocumentID, &revision, &result.Content, &result.ContentHash, &result.OVURI,
		&result.SnapshotOID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Revision{}, ErrNotFound
	}
	if err != nil {
		return model.Revision{}, err
	}
	result.Revision = fmt.Sprintf("r%d", revision)
	result.CreatedAt = parseTime(created)
	return result, nil
}

func (s *Store) SetDocumentSync(ctx context.Context, id, status, brainID, snapshotOID string) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE documents SET status = ?, brain_id = CASE WHEN ? = '' THEN brain_id ELSE ? END, updated_at = ?
			WHERE id = ?`, status, brainID, brainID, now(), id); err != nil {
			return err
		}
		if snapshotOID != "" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE revisions SET snapshot_oid = ? WHERE document_id = ? AND revision =
					(SELECT revision FROM documents WHERE id = ?)`, snapshotOID, id, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func revisionNumber(value string) int {
	value = strings.TrimPrefix(value, "r")
	n, _ := strconv.Atoi(value)
	return n
}

func insertJob(ctx context.Context, tx *sql.Tx, job model.Job, timestamp string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO jobs(id, kind, resource_type, resource_id, status, payload_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Kind, job.ResourceType, job.ResourceID, model.JobAccepted, string(job.Payload), timestamp, timestamp)
	return err
}
