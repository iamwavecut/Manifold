package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iamwavecut/Manifold/internal/problem"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("resource not found")
	ErrConflict = errors.New("resource conflict")
)

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE jobs
		SET status = 'accepted', started_at = NULL, lease_until = NULL, updated_at = ?
		WHERE status IN ('indexing', 'extracting')`, now()); err != nil {
		db.Close()
		return nil, fmt.Errorf("recover interrupted jobs: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS system_state (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
INSERT OR IGNORE INTO system_state(key, value) VALUES ('version', '0');

CREATE TABLE IF NOT EXISTS folders (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	parent_id TEXT REFERENCES folders(id) ON UPDATE CASCADE ON DELETE CASCADE,
	summary TEXT NOT NULL DEFAULT '',
	metadata_json TEXT NOT NULL DEFAULT '{}',
	etag INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS documents (
	id TEXT PRIMARY KEY,
	folder_id TEXT REFERENCES folders(id) ON UPDATE CASCADE ON DELETE SET NULL,
	title TEXT NOT NULL,
	format TEXT NOT NULL,
	content TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	revision INTEGER NOT NULL,
	status TEXT NOT NULL,
	ov_uri TEXT NOT NULL,
	brain_id TEXT NOT NULL DEFAULT '',
	metadata_json TEXT NOT NULL DEFAULT '{}',
	tags_json TEXT NOT NULL DEFAULT '[]',
	etag INTEGER NOT NULL DEFAULT 1,
	deleted INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS revisions (
	document_id TEXT NOT NULL REFERENCES documents(id) ON UPDATE CASCADE ON DELETE CASCADE,
	revision INTEGER NOT NULL,
	content TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	ov_uri TEXT NOT NULL,
	snapshot_oid TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	PRIMARY KEY(document_id, revision)
);

CREATE VIRTUAL TABLE IF NOT EXISTS document_fts USING fts5(
	id UNINDEXED,
	title,
	content,
	tokenize = 'unicode61'
);

CREATE TABLE IF NOT EXISTS jobs (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	resource_type TEXT NOT NULL,
	resource_id TEXT NOT NULL,
	status TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	error_json TEXT,
	payload_json TEXT NOT NULL DEFAULT '{}',
	lease_until TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	started_at TEXT,
	finished_at TEXT
);
CREATE INDEX IF NOT EXISTS jobs_status_idx ON jobs(status, created_at);

CREATE TABLE IF NOT EXISTS entities (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	kind TEXT NOT NULL,
	upstream_id TEXT NOT NULL DEFAULT '',
	metadata_json TEXT NOT NULL DEFAULT '{}',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS facts (
	id TEXT PRIMARY KEY,
	entity_id TEXT NOT NULL REFERENCES entities(id) ON UPDATE CASCADE ON DELETE CASCADE,
	predicate TEXT NOT NULL,
	object TEXT NOT NULL,
	origin TEXT NOT NULL,
	status TEXT NOT NULL,
	confidence REAL NOT NULL,
	source_document_id TEXT REFERENCES documents(id) ON UPDATE CASCADE ON DELETE SET NULL,
	source_revision INTEGER,
	upstream_id TEXT NOT NULL DEFAULT '',
	valid_from TEXT NOT NULL,
	valid_until TEXT,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS relations (
	id TEXT PRIMARY KEY,
	from_entity_id TEXT NOT NULL REFERENCES entities(id) ON UPDATE CASCADE ON DELETE CASCADE,
	to_entity_id TEXT NOT NULL REFERENCES entities(id) ON UPDATE CASCADE ON DELETE CASCADE,
	predicate TEXT NOT NULL,
	origin TEXT NOT NULL,
	status TEXT NOT NULL,
	confidence REAL NOT NULL,
	upstream_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS conflicts (
	id TEXT PRIMARY KEY,
	fact_a_id TEXT NOT NULL REFERENCES facts(id) ON UPDATE CASCADE ON DELETE CASCADE,
	fact_b_id TEXT NOT NULL REFERENCES facts(id) ON UPDATE CASCADE ON DELETE CASCADE,
	status TEXT NOT NULL,
	resolution TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
	id TEXT PRIMARY KEY,
	secret_hash TEXT NOT NULL,
	capabilities_json TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	api_key_id TEXT NOT NULL REFERENCES api_keys(id) ON UPDATE CASCADE ON DELETE CASCADE,
	csrf_hash TEXT NOT NULL,
	capabilities_json TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS idempotency (
	key TEXT NOT NULL,
	method TEXT NOT NULL,
	path TEXT NOT NULL,
	request_hash TEXT NOT NULL,
	status INTEGER NOT NULL,
	body BLOB NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY(key, method, path)
);
UPDATE idempotency SET body = X''
WHERE method = 'POST' AND path = '/api/v1/api-keys';

CREATE TABLE IF NOT EXISTS rename_plans (
	id TEXT PRIMARY KEY,
	status TEXT NOT NULL,
	state_version INTEGER NOT NULL,
	operations_json TEXT NOT NULL,
	preview_json TEXT NOT NULL,
	backup_json TEXT NOT NULL DEFAULT '[]',
	progress_json TEXT NOT NULL DEFAULT '{}',
	error_json TEXT,
	created_at TEXT NOT NULL,
	applied_at TEXT
);

CREATE TABLE IF NOT EXISTS rename_reservations (
	resource_type TEXT NOT NULL,
	slug TEXT NOT NULL,
	plan_id TEXT NOT NULL REFERENCES rename_plans(id) ON UPDATE CASCADE ON DELETE CASCADE,
	PRIMARY KEY(resource_type, slug)
);

CREATE TABLE IF NOT EXISTS rename_audit (
	plan_id TEXT NOT NULL REFERENCES rename_plans(id) ON UPDATE CASCADE ON DELETE CASCADE,
	resource_type TEXT NOT NULL,
	old_slug TEXT NOT NULL,
	new_slug TEXT NOT NULL,
	created_at TEXT NOT NULL
);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("apply database schema: %w", err)
	}
	return nil
}

func (s *Store) InTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func bumpVersion(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE system_state SET value = CAST(value AS INTEGER) + 1 WHERE key = 'version'`)
	return err
}

func (s *Store) StateVersion(ctx context.Context) (int64, error) {
	var value string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM system_state WHERE key = 'version'`).Scan(&value); err != nil {
		return 0, err
	}
	return strconv.ParseInt(value, 10, 64)
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, value)
	return t
}

func parseOptionalTime(value sql.NullString) *time.Time {
	if !value.Valid {
		return nil
	}
	t := parseTime(value.String)
	return &t
}

func jsonString(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func parseMap(value string) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal([]byte(value), &out)
	return out
}

func parseStrings(value string) []string {
	var out []string
	_ = json.Unmarshal([]byte(value), &out)
	return out
}

func decodeProblem(value sql.NullString) *problem.Error {
	if !value.Valid || value.String == "" {
		return nil
	}
	var out problem.Error
	if json.Unmarshal([]byte(value.String), &out) != nil {
		return nil
	}
	return &out
}
