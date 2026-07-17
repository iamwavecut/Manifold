package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/problem"
)

func (s *Store) CreateRenamePlan(ctx context.Context, id string, operations []model.RenameOperation) (model.RenamePlan, error) {
	version, err := s.StateVersion(ctx)
	if err != nil {
		return model.RenamePlan{}, err
	}
	preview := model.RenamePreview{
		Operations:   operations,
		StateVersion: version,
	}
	replacements := map[string]*model.TextReplacement{}
	sources := map[string]map[string]struct{}{}
	targets := map[string]map[string]struct{}{}
	for _, operation := range operations {
		if sources[operation.ResourceType] == nil {
			sources[operation.ResourceType] = map[string]struct{}{}
			targets[operation.ResourceType] = map[string]struct{}{}
		}
		if _, duplicate := sources[operation.ResourceType][operation.From]; duplicate {
			return model.RenamePlan{}, fmt.Errorf("%w: duplicate rename source", ErrConflict)
		}
		if _, duplicate := targets[operation.ResourceType][operation.To]; duplicate {
			return model.RenamePlan{}, fmt.Errorf("%w: duplicate rename target", ErrConflict)
		}
		sources[operation.ResourceType][operation.From] = struct{}{}
		targets[operation.ResourceType][operation.To] = struct{}{}
	}
	for _, operation := range operations {
		if !identity.IsSlug(operation.From) || !identity.IsSlug(operation.To) {
			return model.RenamePlan{}, fmt.Errorf("%w: invalid slug", ErrConflict)
		}
		table, err := slugTable(operation.ResourceType)
		if err != nil {
			return model.RenamePlan{}, err
		}
		var exists int
		if err := s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE id = ?)`, table), operation.From).Scan(&exists); err != nil {
			return model.RenamePlan{}, err
		}
		if exists == 0 {
			return model.RenamePlan{}, ErrNotFound
		}
		if _, partOfCycle := sources[operation.ResourceType][operation.To]; !partOfCycle {
			var occupied int
			if err := s.db.QueryRowContext(ctx,
				fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE id = ?)`, table),
				operation.To).Scan(&occupied); err != nil {
				return model.RenamePlan{}, err
			}
			if occupied != 0 {
				preview.Conflicts = append(preview.Conflicts, problem.Blocker{
					Resource: operation.ResourceType + "s/" + operation.To,
					Reason:   "The requested target slug is already active and is not another source in this batch.",
				})
			}
		}
		preview.StructuredReferences += s.structuredReferenceCount(ctx, operation)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, revision, format, title, content, metadata_json, tags_json
		FROM documents WHERE deleted = 0`)
	if err != nil {
		return model.RenamePlan{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var documentID, format, title, content, metadata, tags string
		var revision int
		if err := rows.Scan(&documentID, &revision, &format, &title, &content, &metadata, &tags); err != nil {
			return model.RenamePlan{}, err
		}
		total := 0
		for _, operation := range operations {
			total += countToken(title, operation.From)
			total += countToken(content, operation.From)
			total += countToken(metadata, operation.From)
			total += countToken(tags, operation.From)
		}
		if total == 0 {
			continue
		}
		contentOccurrences := 0
		for _, operation := range operations {
			contentOccurrences += countToken(content, operation.From)
		}
		if contentOccurrences > 0 && !isRewritable(format) {
			preview.Blockers = append(preview.Blockers, problem.Blocker{
				Resource: "documents/" + documentID,
				Reason:   "The document contains a matching slug but its binary format cannot be rewritten safely.",
				Location: fmt.Sprintf("%s@r%d", documentID, revision),
			})
			continue
		}
		replacements[documentID] = &model.TextReplacement{
			DocumentID:  documentID,
			Revision:    fmt.Sprintf("r%d", revision),
			Occurrences: total,
		}
	}
	for _, replacement := range replacements {
		preview.Replacements = append(preview.Replacements, *replacement)
	}
	timestamp := now()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO rename_plans(id, status, state_version, operations_json, preview_json, created_at)
		VALUES (?, 'previewed', ?, ?, ?, ?)`,
		id, version, jsonString(operations), jsonString(preview), timestamp)
	if err != nil {
		return model.RenamePlan{}, err
	}
	return model.RenamePlan{ID: id, Status: "previewed", Preview: preview, CreatedAt: parseTime(timestamp)}, nil
}

func (s *Store) GetRenamePlan(ctx context.Context, id string) (model.RenamePlan, error) {
	var plan model.RenamePlan
	var previewJSON, errorJSON, created string
	var applied sql.NullString
	var semanticError sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, status, preview_json, COALESCE(error_json, ''), created_at, applied_at
		FROM rename_plans WHERE id = ?`, id).Scan(
		&plan.ID, &plan.Status, &previewJSON, &errorJSON, &created, &applied)
	if errors.Is(err, sql.ErrNoRows) {
		return model.RenamePlan{}, ErrNotFound
	}
	if err != nil {
		return model.RenamePlan{}, err
	}
	_ = semanticError
	if err := json.Unmarshal([]byte(previewJSON), &plan.Preview); err != nil {
		return model.RenamePlan{}, err
	}
	if errorJSON != "" {
		var p problem.Error
		if json.Unmarshal([]byte(errorJSON), &p) == nil {
			plan.Error = &p
		}
	}
	plan.CreatedAt = parseTime(created)
	plan.AppliedAt = parseOptionalTime(applied)
	return plan, nil
}

func (s *Store) SetRenamePlanStatus(ctx context.Context, id, status string, semanticError *problem.Error) error {
	var errorJSON any
	if semanticError != nil {
		errorJSON = jsonString(semanticError)
	}
	var applied any
	if status == "applied" {
		applied = now()
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE rename_plans SET status = ?, error_json = ?, applied_at = COALESCE(?, applied_at) WHERE id = ?`,
		status, errorJSON, applied, id)
	return err
}

func (s *Store) ApplyRename(ctx context.Context, planID string) ([]model.Document, error) {
	plan, err := s.GetRenamePlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	if len(plan.Preview.Blockers) > 0 || len(plan.Preview.Conflicts) > 0 {
		return nil, fmt.Errorf("%w: unrewritable references", ErrConflict)
	}
	currentVersion, err := s.StateVersion(ctx)
	if err != nil {
		return nil, err
	}
	if currentVersion != plan.Preview.StateVersion {
		return nil, fmt.Errorf("%w: stale rename plan", ErrConflict)
	}
	changedIDs := map[string]struct{}{}
	err = s.InTx(ctx, func(tx *sql.Tx) error {
		backup, err := readRenameDocumentBackups(ctx, tx)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE rename_plans SET status = 'applying', backup_json = ? WHERE id = ? AND status = 'accepted'`,
			jsonString(backup), planID)
		if err != nil {
			return err
		}
		affected, _ := result.RowsAffected()
		if affected != 1 {
			return ErrConflict
		}
		finalTargets := map[string]map[string]struct{}{}
		for _, operation := range plan.Preview.Operations {
			if finalTargets[operation.ResourceType] == nil {
				finalTargets[operation.ResourceType] = map[string]struct{}{}
			}
			if _, duplicate := finalTargets[operation.ResourceType][operation.To]; duplicate {
				return fmt.Errorf("%w: duplicate rename target", ErrConflict)
			}
			finalTargets[operation.ResourceType][operation.To] = struct{}{}
		}
		for index, operation := range plan.Preview.Operations {
			table, err := slugTable(operation.ResourceType)
			if err != nil {
				return err
			}
			temp := fmt.Sprintf("rename-%s-%d", planID, index)
			if len(temp) > 63 {
				temp = temp[:63]
			}
			query := fmt.Sprintf(`UPDATE %s SET id = ? WHERE id = ?`, table)
			result, err := tx.ExecContext(ctx, query, temp, operation.From)
			if err != nil {
				return err
			}
			affected, _ := result.RowsAffected()
			if affected != 1 {
				return ErrNotFound
			}
		}
		for index, operation := range plan.Preview.Operations {
			table, _ := slugTable(operation.ResourceType)
			temp := fmt.Sprintf("rename-%s-%d", planID, index)
			if len(temp) > 63 {
				temp = temp[:63]
			}
			query := fmt.Sprintf(`UPDATE %s SET id = ? WHERE id = ?`, table)
			if _, err := tx.ExecContext(ctx, query, operation.To, temp); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO rename_audit(plan_id, resource_type, old_slug, new_slug, created_at)
				VALUES (?, ?, ?, ?, ?)`, planID, operation.ResourceType, operation.From, operation.To, now()); err != nil {
				return err
			}
		}

		mapping := map[string]string{}
		for _, operation := range plan.Preview.Operations {
			mapping[operation.From] = operation.To
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT id, title, content, metadata_json, tags_json, revision, ov_uri
			FROM documents WHERE deleted = 0`)
		if err != nil {
			return err
		}
		type changedDoc struct {
			id, title, content, metadata, tags, ovURI string
			revision                                  int
		}
		var changed []changedDoc
		for rows.Next() {
			var doc changedDoc
			if err := rows.Scan(
				&doc.id, &doc.title, &doc.content, &doc.metadata, &doc.tags, &doc.revision, &doc.ovURI,
			); err != nil {
				rows.Close()
				return err
			}
			nextContent := replaceTokens(doc.content, mapping)
			nextTitle := replaceTokens(doc.title, mapping)
			nextMetadata := replaceTokens(doc.metadata, mapping)
			nextTags := replaceTokens(doc.tags, mapping)
			if nextContent != doc.content || nextTitle != doc.title ||
				nextMetadata != doc.metadata || nextTags != doc.tags {
				doc.content = nextContent
				doc.title = nextTitle
				doc.metadata = nextMetadata
				doc.tags = nextTags
				changed = append(changed, doc)
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, doc := range changed {
			nextRevision := doc.revision + 1
			hash := contentHash(doc.content)
			if _, err := tx.ExecContext(ctx, `
				UPDATE documents SET title = ?, content = ?, content_hash = ?, metadata_json = ?, tags_json = ?,
					revision = ?, etag = etag + 1, status = 'accepted', updated_at = ? WHERE id = ?`,
				doc.title, doc.content, hash, doc.metadata, doc.tags, nextRevision, now(), doc.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO revisions(document_id, revision, content, content_hash, ov_uri, created_at)
				VALUES (?, ?, ?, ?, ?, ?)`, doc.id, nextRevision, doc.content, hash, doc.ovURI, now()); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM document_fts WHERE id = ?`, doc.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO document_fts(id, title, content) VALUES (?, ?, ?)`,
				doc.id, doc.title, doc.content); err != nil {
				return err
			}
			changedIDs[doc.id] = struct{}{}
		}
		return bumpVersion(ctx, tx)
	})
	if err != nil {
		return nil, err
	}
	var docs []model.Document
	for id := range changedIDs {
		doc, err := s.GetDocument(ctx, id, true)
		if err == nil {
			docs = append(docs, doc)
		}
	}
	return docs, nil
}

type renameDocumentBackup struct {
	ID          string `json:"id"`
	FolderID    string `json:"folder_id"`
	Title       string `json:"title"`
	Format      string `json:"format"`
	Content     string `json:"content"`
	ContentHash string `json:"content_hash"`
	Revision    int    `json:"revision"`
	Status      string `json:"status"`
	OVURI       string `json:"ov_uri"`
	BrainID     string `json:"brain_id"`
	Metadata    string `json:"metadata_json"`
	Tags        string `json:"tags_json"`
	ETag        int    `json:"etag"`
	Deleted     int    `json:"deleted"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func readRenameDocumentBackups(ctx context.Context, tx *sql.Tx) ([]renameDocumentBackup, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, COALESCE(folder_id, ''), title, format, content, content_hash, revision, status,
			ov_uri, brain_id, metadata_json, tags_json, etag, deleted, created_at, updated_at
		FROM documents`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var backups []renameDocumentBackup
	for rows.Next() {
		var backup renameDocumentBackup
		if err := rows.Scan(
			&backup.ID, &backup.FolderID, &backup.Title, &backup.Format, &backup.Content,
			&backup.ContentHash, &backup.Revision, &backup.Status, &backup.OVURI, &backup.BrainID,
			&backup.Metadata, &backup.Tags, &backup.ETag, &backup.Deleted, &backup.CreatedAt,
			&backup.UpdatedAt,
		); err != nil {
			return nil, err
		}
		backups = append(backups, backup)
	}
	return backups, rows.Err()
}

func (s *Store) QueueRename(ctx context.Context, plan model.RenamePlan, job model.Job) (model.Job, error) {
	timestamp := now()
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		for _, operation := range plan.Preview.Operations {
			for _, slug := range []string{operation.From, operation.To} {
				if _, err := tx.ExecContext(ctx, `
					INSERT OR IGNORE INTO rename_reservations(resource_type, slug, plan_id) VALUES (?, ?, ?)`,
					operation.ResourceType, slug, plan.ID); err != nil {
					return fmt.Errorf("%w: rename slug reservation failed", ErrConflict)
				}
				var owner string
				if err := tx.QueryRowContext(ctx, `
					SELECT plan_id FROM rename_reservations WHERE resource_type = ? AND slug = ?`,
					operation.ResourceType, slug).Scan(&owner); err != nil {
					return err
				}
				if owner != plan.ID {
					return fmt.Errorf("%w: slug is reserved by another rename plan", ErrConflict)
				}
			}
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE rename_plans SET status = 'accepted' WHERE id = ? AND status = 'previewed'`,
			plan.ID)
		if err != nil {
			return err
		}
		affected, _ := result.RowsAffected()
		if affected != 1 {
			return ErrConflict
		}
		return insertJob(ctx, tx, job, timestamp)
	})
	if err != nil {
		return model.Job{}, err
	}
	job.Status = model.JobAccepted
	job.CreatedAt = parseTime(timestamp)
	job.UpdatedAt = job.CreatedAt
	return job, nil
}

func (s *Store) FinalizeRename(ctx context.Context, planID string) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE rename_plans
			SET status = 'applied', applied_at = ?, backup_json = '[]', progress_json = '{}'
			WHERE id = ?`,
			now(), planID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM rename_reservations WHERE plan_id = ?`, planID)
		return err
	})
}

func (s *Store) CancelQueuedRename(ctx context.Context, planID string, semanticError *problem.Error) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM rename_reservations WHERE plan_id = ?`, planID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE rename_plans SET status = 'failed', error_json = ? WHERE id = ? AND status = 'accepted'`,
			jsonString(semanticError), planID)
		return err
	})
}

func (s *Store) RenameBackupDocuments(ctx context.Context, planID string) ([]model.Document, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT backup_json FROM rename_plans WHERE id = ?`, planID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var backups []renameDocumentBackup
	if err := json.Unmarshal([]byte(raw), &backups); err != nil {
		return nil, err
	}
	result := make([]model.Document, 0, len(backups))
	for _, backup := range backups {
		result = append(result, model.Document{
			ID: backup.ID, FolderID: backup.FolderID, Title: backup.Title, Format: backup.Format,
			Content: backup.Content, ContentHash: backup.ContentHash, Revision: fmt.Sprintf("r%d", backup.Revision),
			Status: model.JobStatus(backup.Status), OVURI: backup.OVURI, BrainID: backup.BrainID,
			Metadata: parseMap(backup.Metadata), Tags: parseStrings(backup.Tags), ETag: fmt.Sprintf(`"v%d"`, backup.ETag),
			CreatedAt: parseTime(backup.CreatedAt), UpdatedAt: parseTime(backup.UpdatedAt),
		})
	}
	return result, nil
}

func (s *Store) RollbackRename(ctx context.Context, planID string, semanticError *problem.Error) error {
	plan, err := s.GetRenamePlan(ctx, planID)
	if err != nil {
		return err
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT backup_json FROM rename_plans WHERE id = ?`, planID).Scan(&raw); err != nil {
		return err
	}
	var backups []renameDocumentBackup
	if err := json.Unmarshal([]byte(raw), &backups); err != nil {
		return err
	}
	return s.InTx(ctx, func(tx *sql.Tx) error {
		for index, operation := range plan.Preview.Operations {
			table, err := slugTable(operation.ResourceType)
			if err != nil {
				return err
			}
			temp := fmt.Sprintf("rollback-%s-%d", planID, index)
			if len(temp) > 63 {
				temp = temp[:63]
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET id = ? WHERE id = ?`, table), temp, operation.To); err != nil {
				return err
			}
		}
		for index, operation := range plan.Preview.Operations {
			table, _ := slugTable(operation.ResourceType)
			temp := fmt.Sprintf("rollback-%s-%d", planID, index)
			if len(temp) > 63 {
				temp = temp[:63]
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET id = ? WHERE id = ?`, table), operation.From, temp); err != nil {
				return err
			}
		}
		for _, backup := range backups {
			if _, err := tx.ExecContext(ctx, `
				UPDATE documents SET folder_id = NULLIF(?, ''), title = ?, format = ?, content = ?,
					content_hash = ?, revision = ?, status = ?, ov_uri = ?, brain_id = ?, metadata_json = ?,
					tags_json = ?, etag = ?, deleted = ?, created_at = ?, updated_at = ? WHERE id = ?`,
				backup.FolderID, backup.Title, backup.Format, backup.Content, backup.ContentHash,
				backup.Revision, backup.Status, backup.OVURI, backup.BrainID, backup.Metadata, backup.Tags,
				backup.ETag, backup.Deleted, backup.CreatedAt, backup.UpdatedAt, backup.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM revisions WHERE document_id = ? AND revision > ?`,
				backup.ID, backup.Revision); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM document_fts WHERE id = ?`, backup.ID); err != nil {
				return err
			}
			if backup.Deleted == 0 {
				if _, err := tx.ExecContext(ctx, `INSERT INTO document_fts(id, title, content) VALUES (?, ?, ?)`,
					backup.ID, backup.Title, backup.Content); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM rename_audit WHERE plan_id = ?`, planID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM rename_reservations WHERE plan_id = ?`, planID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE rename_plans
			SET status = 'failed', error_json = ?, backup_json = '[]', progress_json = '{}'
			WHERE id = ?`,
			jsonString(semanticError), planID); err != nil {
			return err
		}
		return bumpVersion(ctx, tx)
	})
}

func (s *Store) RenameProgress(ctx context.Context, planID string) (map[string]int, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx,
		`SELECT progress_json FROM rename_plans WHERE id = ?`, planID,
	).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	progress := map[string]int{}
	if err := json.Unmarshal([]byte(raw), &progress); err != nil {
		return nil, err
	}
	return progress, nil
}

func (s *Store) SetRenameProgress(ctx context.Context, planID, key string, phase int) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		var raw string
		if err := tx.QueryRowContext(ctx,
			`SELECT progress_json FROM rename_plans WHERE id = ?`, planID,
		).Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		progress := map[string]int{}
		if err := json.Unmarshal([]byte(raw), &progress); err != nil {
			return err
		}
		progress[key] = phase
		_, err := tx.ExecContext(ctx,
			`UPDATE rename_plans SET progress_json = ? WHERE id = ?`,
			jsonString(progress), planID)
		return err
	})
}

func (s *Store) structuredReferenceCount(ctx context.Context, operation model.RenameOperation) int {
	queries := map[string][]string{
		"folder": {
			`SELECT COUNT(*) FROM folders WHERE parent_id = ?`,
			`SELECT COUNT(*) FROM documents WHERE folder_id = ?`,
		},
		"document": {
			`SELECT COUNT(*) FROM facts WHERE source_document_id = ?`,
			`SELECT COUNT(*) FROM revisions WHERE document_id = ?`,
		},
		"entity": {
			`SELECT COUNT(*) FROM facts WHERE entity_id = ?`,
			`SELECT COUNT(*) FROM relations WHERE from_entity_id = ? OR to_entity_id = ?`,
		},
	}
	total := 0
	for _, query := range queries[operation.ResourceType] {
		var count int
		args := []any{operation.From}
		if strings.Count(query, "?") == 2 {
			args = append(args, operation.From)
		}
		if s.db.QueryRowContext(ctx, query, args...).Scan(&count) == nil {
			total += count
		}
	}
	return total
}

func countToken(content, token string) int {
	count := 0
	for _, current := range splitTokens(content) {
		if current == token {
			count++
		}
	}
	return count
}

func replaceTokens(content string, mapping map[string]string) string {
	var out strings.Builder
	for _, token := range splitPreserving(content) {
		if replacement, ok := mapping[token]; ok {
			out.WriteString(replacement)
		} else {
			out.WriteString(token)
		}
	}
	return out.String()
}

func splitTokens(content string) []string {
	var tokens []string
	for _, token := range splitPreserving(content) {
		if isSlugRuneString(token) {
			tokens = append(tokens, token)
		}
	}
	return tokens
}

func splitPreserving(content string) []string {
	var parts []string
	start := 0
	inToken := false
	for index, r := range content {
		tokenRune := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if index == 0 {
			inToken = tokenRune
			continue
		}
		if tokenRune != inToken {
			parts = append(parts, content[start:index])
			start = index
			inToken = tokenRune
		}
	}
	if start < len(content) {
		parts = append(parts, content[start:])
	}
	return parts
}

func isSlugRuneString(value string) bool {
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return value != ""
}

func isRewritable(format string) bool {
	switch strings.ToLower(format) {
	case "markdown", "md", "text", "txt", "json", "yaml", "yml", "toml", "javascript", "typescript", "python":
		return true
	default:
		return false
	}
}
