package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/problem"
)

type APIKeyRecord struct {
	ID           string
	SecretHash   string
	Capabilities []string
	CreatedAt    time.Time
}

type SessionRecord struct {
	APIKeyID     string
	Capabilities []string
	CSRFHash     string
	ExpiresAt    time.Time
}

func (s *Store) APIKeyCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys`).Scan(&count)
	return count, err
}

func (s *Store) CreateAPIKey(ctx context.Context, id, secretHash string, capabilities []string) (model.APIKey, error) {
	timestamp := now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys(id, secret_hash, capabilities_json, created_at) VALUES (?, ?, ?, ?)`,
		id, secretHash, jsonString(capabilities), timestamp)
	if err != nil {
		return model.APIKey{}, err
	}
	return model.APIKey{ID: id, Capabilities: capabilities, CreatedAt: parseTime(timestamp)}, nil
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]model.APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, capabilities_json, created_at FROM api_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []model.APIKey
	for rows.Next() {
		var key model.APIKey
		var capabilities, created string
		if err := rows.Scan(&key.ID, &capabilities, &created); err != nil {
			return nil, err
		}
		key.Capabilities = parseStrings(capabilities)
		key.CreatedAt = parseTime(created)
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) FindAPIKeys(ctx context.Context) ([]APIKeyRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, secret_hash, capabilities_json, created_at FROM api_keys`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []APIKeyRecord
	for rows.Next() {
		var record APIKeyRecord
		var capabilities, created string
		if err := rows.Scan(&record.ID, &record.SecretHash, &capabilities, &created); err != nil {
			return nil, err
		}
		record.Capabilities = parseStrings(capabilities)
		record.CreatedAt = parseTime(created)
		keys = append(keys, record)
	}
	return keys, rows.Err()
}

func (s *Store) DeleteAPIKey(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return err
	}
	count, _ := res.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CreateSession(ctx context.Context, token, csrf, apiKeyID string, capabilities []string, expires time.Time) error {
	tokenHash := sha256.Sum256([]byte(token))
	csrfHash := sha256.Sum256([]byte(csrf))
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions(token_hash, api_key_id, csrf_hash, capabilities_json, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		hex.EncodeToString(tokenHash[:]), apiKeyID, hex.EncodeToString(csrfHash[:]),
		jsonString(capabilities), expires.UTC().Format(time.RFC3339Nano), now())
	return err
}

func (s *Store) GetSession(ctx context.Context, token string) (SessionRecord, error) {
	tokenHash := sha256.Sum256([]byte(token))
	var record SessionRecord
	var capabilities, expires string
	err := s.db.QueryRowContext(ctx, `
		SELECT api_key_id, csrf_hash, capabilities_json, expires_at
		FROM sessions WHERE token_hash = ?`,
		hex.EncodeToString(tokenHash[:])).Scan(&record.APIKeyID, &record.CSRFHash, &capabilities, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionRecord{}, ErrNotFound
	}
	if err != nil {
		return SessionRecord{}, err
	}
	record.Capabilities = parseStrings(capabilities)
	record.ExpiresAt = parseTime(expires)
	if time.Now().After(record.ExpiresAt) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hex.EncodeToString(tokenHash[:]))
		return SessionRecord{}, ErrNotFound
	}
	return record, nil
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	tokenHash := sha256.Sum256([]byte(token))
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hex.EncodeToString(tokenHash[:]))
	return err
}

func (s *Store) NextJob(ctx context.Context) (model.Job, error) {
	var job model.Job
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		var errorJSON, started, finished sql.NullString
		var payload string
		err := tx.QueryRowContext(ctx, `
			SELECT id, kind, resource_type, resource_id, status, attempts, error_json, payload_json,
				created_at, updated_at, started_at, finished_at
			FROM jobs
			WHERE status = 'accepted' AND (lease_until IS NULL OR lease_until < ?)
			ORDER BY created_at LIMIT 1`, now()).Scan(
			&job.ID, &job.Kind, &job.ResourceType, &job.ResourceID, &job.Status, &job.Attempts,
			&errorJSON, &payload, new(string), new(string), &started, &finished)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		timestamp := now()
		lease := time.Now().UTC().Add(2 * time.Minute).Format(time.RFC3339Nano)
		res, err := tx.ExecContext(ctx, `
			UPDATE jobs SET status = 'indexing', attempts = attempts + 1, started_at = COALESCE(started_at, ?),
				lease_until = ?, updated_at = ? WHERE id = ? AND status = 'accepted'`,
			timestamp, lease, timestamp, job.ID)
		if err != nil {
			return err
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			return ErrConflict
		}
		job.Status = model.JobIndexing
		job.Attempts++
		job.Payload = json.RawMessage(payload)
		job.Error = decodeProblem(errorJSON)
		return nil
	})
	if err != nil {
		return model.Job{}, err
	}
	return s.GetJob(ctx, job.ID)
}

func (s *Store) GetJob(ctx context.Context, id string) (model.Job, error) {
	var job model.Job
	var errorJSON, started, finished sql.NullString
	var payload, created, updated string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, kind, resource_type, resource_id, status, attempts, error_json, payload_json,
			created_at, updated_at, started_at, finished_at
		FROM jobs WHERE id = ?`, id).Scan(
		&job.ID, &job.Kind, &job.ResourceType, &job.ResourceID, &job.Status, &job.Attempts,
		&errorJSON, &payload, &created, &updated, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Job{}, ErrNotFound
	}
	if err != nil {
		return model.Job{}, err
	}
	job.Error = decodeProblem(errorJSON)
	job.Payload = json.RawMessage(payload)
	job.CreatedAt = parseTime(created)
	job.UpdatedAt = parseTime(updated)
	job.StartedAt = parseOptionalTime(started)
	job.FinishedAt = parseOptionalTime(finished)
	return job, nil
}

func (s *Store) ListJobs(ctx context.Context, limit int) ([]model.Job, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM jobs ORDER BY created_at DESC LIMIT ?`, limit)
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
	var jobs []model.Job
	for _, id := range ids {
		job, err := s.GetJob(ctx, id)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (s *Store) SetJobStatus(ctx context.Context, id string, status model.JobStatus, semanticError *problem.Error) error {
	var errorJSON any
	if semanticError != nil {
		errorJSON = jsonString(semanticError)
	}
	finished := any(nil)
	if status == model.JobReady || status == model.JobPartiallyReady || status == model.JobFailed {
		finished = now()
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = ?, error_json = ?, lease_until = NULL, updated_at = ?,
			finished_at = COALESCE(?, finished_at) WHERE id = ?`,
		status, errorJSON, now(), finished, id)
	return err
}

func (s *Store) RetryJob(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = 'accepted', error_json = NULL, lease_until = NULL, finished_at = NULL, updated_at = ?
		WHERE id = ? AND status IN ('failed', 'partially_ready')`, now(), id)
	if err != nil {
		return err
	}
	count, _ := res.RowsAffected()
	if count == 0 {
		return ErrConflict
	}
	return nil
}

func (s *Store) GetIdempotency(ctx context.Context, key, method, path string) (int, []byte, string, error) {
	var status int
	var body []byte
	var requestHash string
	err := s.db.QueryRowContext(ctx, `
		SELECT status, body, request_hash FROM idempotency WHERE key = ? AND method = ? AND path = ?`,
		key, method, path).Scan(&status, &body, &requestHash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, "", ErrNotFound
	}
	return status, body, requestHash, err
}

func (s *Store) SaveIdempotency(
	ctx context.Context,
	key, method, path, requestHash string,
	status int,
	body []byte,
) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO idempotency(key, method, path, request_hash, status, body, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, key, method, path, requestHash, status, body, now())
	return err
}

func (s *Store) Metrics(ctx context.Context) (map[string]int, error) {
	result := map[string]int{}
	for name, query := range map[string]string{
		"folders":     `SELECT COUNT(*) FROM folders`,
		"documents":   `SELECT COUNT(*) FROM documents WHERE deleted = 0`,
		"entities":    `SELECT COUNT(*) FROM entities`,
		"facts":       `SELECT COUNT(*) FROM facts`,
		"relations":   `SELECT COUNT(*) FROM relations`,
		"jobs":        `SELECT COUNT(*) FROM jobs`,
		"failed_jobs": `SELECT COUNT(*) FROM jobs WHERE status = 'failed'`,
	} {
		var count int
		if err := s.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		result[name] = count
	}
	return result, nil
}
