// Package engine is Manifold's embedded knowledge engine. One SQLite file
// holds the derived graph (entities, facts, relations), the semantic index
// of document text, and their int8 vectors. Everything in it derives from
// Manifold's canonical database and can be rebuilt from there.
package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	_ "modernc.org/sqlite/vec"
)

// Model is the language model the engine calls: JSON extraction and text
// embeddings.
type Model interface {
	JSON(ctx context.Context, name string, schema map[string]any, system, user string, out any) error
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

type Options struct {
	// Path is the index file. ":memory:" keeps it in memory.
	Path string
	// EmbeddingModel and Dimensions identify the vector space. A change
	// rebuilds every vector.
	EmbeddingModel string
	Dimensions     int
	Model          Model
	Logger         *slog.Logger
	// ExtractionConcurrency bounds the model calls of one document.
	ExtractionConcurrency int
}

type Engine struct {
	db          *sql.DB
	model       Model
	dims        int
	logger      *slog.Logger
	concurrency int
	// write serializes write transactions; SQLite allows one writer.
	write   sync.Mutex
	queries *queryCache
	wake    chan struct{}
	// answered receives a signal when the provider answers a call; it ends
	// the embedder's backoff after a provider failure.
	answered chan struct{}

	vocabularyMu sync.Mutex
	vocabulary   []string
	vocabularyAt time.Time
}

const schemaVersion = "1"

func Open(ctx context.Context, opts Options) (*Engine, error) {
	if opts.Dimensions <= 0 || opts.Dimensions > 4096 {
		return nil, fmt.Errorf("embedding dimensions must be between 1 and 4096, got %d", opts.Dimensions)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.ExtractionConcurrency <= 0 {
		opts.ExtractionConcurrency = 4
	}
	memory := opts.Path == ":memory:"
	if !memory {
		if err := os.MkdirAll(filepath.Dir(opts.Path), 0o750); err != nil {
			return nil, fmt.Errorf("create index directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dsn(opts.Path))
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	if memory {
		// Every connection to ":memory:" is a separate database.
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(4)
	}
	db.SetConnMaxIdleTime(5 * time.Minute)
	e := &Engine{
		db: db, model: opts.Model, dims: opts.Dimensions, logger: opts.Logger,
		concurrency: opts.ExtractionConcurrency, queries: newQueryCache(256),
		wake: make(chan struct{}, 1), answered: make(chan struct{}, 1),
	}
	if opts.Model != nil {
		e.model = answeringModel{Model: opts.Model, answered: e.providerAnswered}
	}
	if err := e.migrate(ctx, opts.EmbeddingModel); err != nil {
		db.Close()
		return nil, err
	}
	return e, nil
}

func dsn(path string) string {
	pragmas := url.Values{}
	for _, pragma := range []string{
		"busy_timeout(10000)", "journal_mode(WAL)", "synchronous(NORMAL)",
		"foreign_keys(OFF)", "cache_size(-4000)", "temp_store(MEMORY)",
		// A seed or a large document grows the WAL file; shrink it back to
		// 4 MiB when the log restarts instead of keeping its peak size.
		"journal_size_limit(4194304)",
	} {
		pragmas.Add("_pragma", pragma)
	}
	pragmas.Set("_txlock", "immediate")
	if path == ":memory:" {
		return "file::memory:?" + pragmas.Encode()
	}
	return "file:" + filepath.ToSlash(path) + "?" + pragmas.Encode()
}

func (e *Engine) Close() error { return e.db.Close() }

func (e *Engine) Ping(ctx context.Context) error { return e.db.PingContext(ctx) }

// Documents is the engine's document index.
func (e *Engine) Documents() *Documents { return &Documents{e: e} }

// Graph is the engine's knowledge graph.
func (e *Engine) Graph() *Graph { return &Graph{e: e} }

const schema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS entities(
	rid INTEGER PRIMARY KEY,
	id TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL,
	norm TEXT NOT NULL,
	kind TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS entities_norm ON entities(norm);
CREATE TABLE IF NOT EXISTS entity_aliases(
	norm TEXT NOT NULL,
	entity_rid INTEGER NOT NULL,
	PRIMARY KEY(norm, entity_rid)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS entity_aliases_entity ON entity_aliases(entity_rid);

CREATE TABLE IF NOT EXISTS facts(
	rid INTEGER PRIMARY KEY,
	id TEXT NOT NULL UNIQUE,
	entity_rid INTEGER NOT NULL,
	predicate TEXT NOT NULL,
	object TEXT NOT NULL,
	object_norm TEXT NOT NULL,
	status TEXT NOT NULL,
	confidence REAL NOT NULL,
	valid_from TEXT NOT NULL,
	valid_until TEXT,
	superseded_by INTEGER,
	retract_reason TEXT,
	source_document_id TEXT NOT NULL DEFAULT '',
	evidence TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	embedded INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS facts_key ON facts(entity_rid, predicate, object_norm);
CREATE INDEX IF NOT EXISTS facts_superseded_by ON facts(superseded_by) WHERE superseded_by IS NOT NULL;
CREATE INDEX IF NOT EXISTS facts_unembedded ON facts(rid) WHERE embedded = 0 AND status <> 'retracted';

CREATE TABLE IF NOT EXISTS relations(
	rid INTEGER PRIMARY KEY,
	id TEXT NOT NULL UNIQUE,
	from_rid INTEGER NOT NULL,
	to_rid INTEGER NOT NULL,
	predicate TEXT NOT NULL,
	status TEXT NOT NULL,
	confidence REAL NOT NULL,
	source_document_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS relations_key ON relations(from_rid, to_rid, predicate);

CREATE VIRTUAL TABLE IF NOT EXISTS facts_fts USING fts5(
	text, content='', contentless_delete=1, tokenize='unicode61 remove_diacritics 2');
CREATE VIRTUAL TABLE IF NOT EXISTS entities_fts USING fts5(
	text, content='', contentless_delete=1, tokenize='unicode61 remove_diacritics 2');

CREATE TABLE IF NOT EXISTS ingests(
	document_id TEXT NOT NULL,
	revision INTEGER NOT NULL,
	result TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY(document_id, revision)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS paths(
	uri TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS chunks(
	rid INTEGER PRIMARY KEY,
	uri TEXT NOT NULL,
	seq INTEGER NOT NULL,
	text TEXT NOT NULL,
	embedded INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS chunks_uri ON chunks(uri, seq);
CREATE INDEX IF NOT EXISTS chunks_unembedded ON chunks(rid) WHERE embedded = 0;
`

func (e *Engine) migrate(ctx context.Context, embeddingModel string) error {
	e.write.Lock()
	defer e.write.Unlock()
	return e.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("create index schema: %w", err)
		}
		space := embeddingModel + "/" + strconv.Itoa(e.dims)
		current, err := metaValue(ctx, tx, "vector_space")
		if err != nil {
			return err
		}
		if current != space {
			// Vectors from another model or size are not comparable.
			for _, table := range []string{"fact_vec", "chunk_vec"} {
				if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
					return fmt.Errorf("drop %s: %w", table, err)
				}
			}
			for _, statement := range []string{
				`UPDATE facts SET embedded = 0 WHERE embedded <> 0`,
				`UPDATE chunks SET embedded = 0 WHERE embedded <> 0`,
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
		}
		for _, table := range []string{"fact_vec", "chunk_vec"} {
			statement := fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS %s USING vec0(
				rid INTEGER PRIMARY KEY, emb int8[%d] distance_metric=cosine)`, table, e.dims)
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("create %s: %w", table, err)
			}
		}
		if err := setMeta(ctx, tx, "vector_space", space); err != nil {
			return err
		}
		return setMeta(ctx, tx, "schema_version", schemaVersion)
	})
}

func metaValue(ctx context.Context, q queryer, key string) (string, error) {
	var value string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func setMeta(ctx context.Context, tx *sql.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO meta(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// inTx runs fn in one transaction. The caller holds e.write.
func (e *Engine) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// update runs fn in a write transaction.
func (e *Engine) update(ctx context.Context, fn func(*sql.Tx) error) error {
	e.write.Lock()
	defer e.write.Unlock()
	return e.inTx(ctx, fn)
}

// notify wakes the embedder without blocking.
func (e *Engine) notify() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) providerAnswered() {
	select {
	case e.answered <- struct{}{}:
	default:
	}
}

// answeringModel reports every call the provider answers.
type answeringModel struct {
	Model
	answered func()
}

func (m answeringModel) JSON(ctx context.Context, name string, schema map[string]any, system, user string, out any) error {
	err := m.Model.JSON(ctx, name, schema, system, user, out)
	if err == nil {
		m.answered()
	}
	return err
}

func (m answeringModel) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	vectors, err := m.Model.Embed(ctx, inputs)
	if err == nil {
		m.answered()
	}
	return vectors, err
}

// timestampLayout has a fixed width, so stored times sort as text.
// RFC3339Nano drops trailing zeros and does not.
const timestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

func timestamp(t time.Time) string { return t.UTC().Format(timestampLayout) }

func nowText() string { return timestamp(time.Now()) }

func parseTimestamp(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

func isLimitedText(value string, max int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len([]rune(value)) <= max
}
