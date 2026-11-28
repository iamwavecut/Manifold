package engine

import (
	"context"
	"database/sql"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/iamwavecut/Manifold/internal/upstream"
)

// The seed fills a new index from Manifold's own database: the graph that
// Manifold projected from its earlier engine, with the same upstream IDs,
// and the text of live documents. It needs no model call; vectors follow
// from the embedder.

// SeedEntity is one entity of Manifold's graph. Key links the facts and
// relations of the entity. UpstreamID is the index ID to keep; it is empty
// for an entity Manifold created itself, which the index finds by name, as
// it does for every API call about that entity.
type SeedEntity struct {
	Key, UpstreamID, Name, Kind string
}

type SeedFact struct {
	UpstreamID       string
	EntityKey        string
	Predicate        string
	Object           string
	Status           string
	Confidence       float64
	ValidFrom        time.Time
	ValidUntil       *time.Time
	SourceDocumentID string
}

type SeedRelation struct {
	UpstreamID string
	FromKey    string
	ToKey      string
	Predicate  string
	Status     string
	Confidence float64
}

type SeedDocument struct {
	URI, Content string
}

// SeedIngest is the graph result Manifold already projected for a document
// revision. A repeated ingest of that revision returns it unchanged, so it
// needs no model call and matches Manifold's stored snapshot.
type SeedIngest struct {
	DocumentID string
	Revision   int
	Result     upstream.GraphIngestResult
}

type SeedSource interface {
	SeedEntities(context.Context, func(SeedEntity) error) error
	SeedFacts(context.Context, func(SeedFact) error) error
	SeedRelations(context.Context, func(SeedRelation) error) error
	// SeedFolders yields folder locators, each ending with a slash.
	SeedFolders(context.Context, func(string) error) error
	SeedDocuments(context.Context, func(SeedDocument) error) error
	SeedIngests(context.Context, func(SeedIngest) error) error
}

// Seeded reports whether the index was seeded.
func (e *Engine) Seeded(ctx context.Context) (bool, error) {
	value, err := metaValue(ctx, e.db, "seeded_at")
	return value != "", err
}

// Seed copies src into the index in one transaction and marks it seeded.
// Records the index holds already stay as they are.
func (e *Engine) Seed(ctx context.Context, src SeedSource) error {
	err := e.update(ctx, func(tx *sql.Tx) error {
		created := nowText()
		entities := map[string]int64{}
		err := src.SeedEntities(ctx, func(entity SeedEntity) error {
			key := entity.Key
			if key == "" {
				key = entity.UpstreamID
			}
			if key == "" || strings.TrimSpace(entity.Name) == "" {
				return nil
			}
			kind := entity.Kind
			if kind == "" {
				kind = "other"
			}
			if entity.UpstreamID == "" {
				ref, err := resolveEntity(ctx, tx, entity.Name, kind, nil)
				if err != nil {
					return err
				}
				entities[key] = ref.rid
				return nil
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO entities(id, name, norm, kind, created_at) VALUES (?, ?, ?, ?, ?)`,
				entity.UpstreamID, entity.Name, normalize(entity.Name), kind, created); err != nil {
				return err
			}
			var rid int64
			if err := tx.QueryRowContext(ctx, `SELECT rid FROM entities WHERE id = ?`, entity.UpstreamID).Scan(&rid); err != nil {
				return err
			}
			entities[key] = rid
			return indexEntity(ctx, tx, rid)
		})
		if err != nil {
			return err
		}
		err = src.SeedFacts(ctx, func(fact SeedFact) error {
			entity, ok := entities[fact.EntityKey]
			if fact.UpstreamID == "" || !ok || fact.Predicate == "" {
				return nil
			}
			status := fact.Status
			if status != "superseded" && status != "retracted" {
				status = "active"
			}
			objectNorm := normalize(fact.Object)
			if objectNorm == "" {
				objectNorm = strings.ToLower(strings.TrimSpace(fact.Object))
			}
			validFrom := fact.ValidFrom
			if validFrom.IsZero() {
				validFrom = time.Now()
			}
			var validUntil any
			if fact.ValidUntil != nil {
				validUntil = timestamp(*fact.ValidUntil)
			}
			result, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO facts(id, entity_rid, predicate, object, object_norm, status, confidence,
					valid_from, valid_until, source_document_id, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				fact.UpstreamID, entity, fact.Predicate, fact.Object, objectNorm, status, fact.Confidence,
				timestamp(validFrom), validUntil, fact.SourceDocumentID, created)
			if err != nil {
				return err
			}
			if n, _ := result.RowsAffected(); n == 0 || status == "retracted" {
				return nil
			}
			rid, err := result.LastInsertId()
			if err != nil {
				return err
			}
			return indexFact(ctx, tx, rid)
		})
		if err != nil {
			return err
		}
		if err := linkSeededSupersession(ctx, tx); err != nil {
			return err
		}
		err = src.SeedRelations(ctx, func(relation SeedRelation) error {
			from, fromOK := entities[relation.FromKey]
			to, toOK := entities[relation.ToKey]
			if relation.UpstreamID == "" || !fromOK || !toOK || relation.Predicate == "" {
				return nil
			}
			status := relation.Status
			if status != "retracted" {
				status = "active"
			}
			_, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO relations(id, from_rid, to_rid, predicate, status, confidence, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				relation.UpstreamID, from, to, relation.Predicate, status, relation.Confidence, created)
			return err
		})
		if err != nil {
			return err
		}
		err = src.SeedFolders(ctx, func(uri string) error {
			_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO paths(uri, kind) VALUES (?, 'folder')`, uri)
			return err
		})
		if err != nil {
			return err
		}
		err = src.SeedDocuments(ctx, func(document SeedDocument) error {
			result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO paths(uri, kind) VALUES (?, 'file')`, document.URI)
			if err != nil {
				return err
			}
			if n, _ := result.RowsAffected(); n == 0 {
				return nil
			}
			return writeChunks(ctx, tx, document.URI, document.Content)
		})
		if err != nil {
			return err
		}
		err = src.SeedIngests(ctx, func(ingest SeedIngest) error {
			encoded, err := encodeResult(ingest.Result)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO ingests(document_id, revision, result, created_at) VALUES (?, ?, ?, ?)`,
				ingest.DocumentID, ingest.Revision, encoded, created)
			return err
		})
		if err != nil {
			return err
		}
		return setMeta(ctx, tx, "seeded_at", created)
	})
	if err == nil {
		e.notify()
	}
	return err
}

// linkSeededSupersession rebuilds which fact superseded which for the
// single-valued predicates. Manifold's database keeps only the statuses, and
// without the links retracting a current value would not revive the value
// before it. Each superseded value points to the next newer value of the same
// entity and predicate.
func linkSeededSupersession(ctx context.Context, tx *sql.Tx) error {
	predicates := slices.Sorted(maps.Keys(singleValued))
	_, err := tx.ExecContext(ctx, `
		UPDATE facts SET superseded_by = (
			SELECT n.rid FROM facts n
			WHERE n.entity_rid = facts.entity_rid AND n.predicate = facts.predicate AND n.rid <> facts.rid
				AND n.status IN ('active', 'superseded')
				AND (n.valid_from > facts.valid_from OR (n.valid_from = facts.valid_from AND n.rid > facts.rid))
			ORDER BY n.valid_from, n.rid LIMIT 1)
		WHERE status = 'superseded' AND superseded_by IS NULL
			AND predicate IN (?`+strings.Repeat(", ?", len(predicates)-1)+`)`,
		anySlice(predicates)...)
	return err
}

func anySlice(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}
