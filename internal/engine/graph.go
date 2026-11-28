package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rs/xid"

	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/upstream"
)

// Graph is the derived knowledge graph. It implements upstream.GraphStore.
type Graph struct{ e *Engine }

func (g *Graph) Health(ctx context.Context) error { return g.e.Ping(ctx) }

// IngestDocument extracts entities, facts and relations from one document
// revision and merges them into the graph. A fact the graph already holds
// keeps its ID, so a document restating it claims the same fact again. The
// result of a revision is stored: repeating it returns the same answer
// without a model call.
func (g *Graph) IngestDocument(ctx context.Context, document upstream.GraphDocument) (upstream.GraphIngestResult, error) {
	revision, err := revisionNumber(document.Revision)
	if err != nil {
		return upstream.GraphIngestResult{}, err
	}
	if result, ok, err := g.storedResult(ctx, document.ID, revision); err != nil || ok {
		return result, err
	}
	occurred, err := time.Parse(time.RFC3339, document.OccurredAt)
	if err != nil {
		occurred = time.Now()
	}
	occurred = occurred.UTC()
	vocabulary, err := g.e.knownPredicates(ctx)
	if err != nil {
		return upstream.GraphIngestResult{}, err
	}
	sections := splitSections(document.Content, extractionChunkSize)
	parts := make([][]cleanPart, len(sections))
	errs := make([]error, len(sections))
	extractCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	slots := make(chan struct{}, g.e.concurrency)
	var wg sync.WaitGroup
	for i, part := range sections {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			parts[i], errs[i] = g.e.extractPart(extractCtx, document, part, i, len(sections), vocabulary, occurred, 0)
			if errs[i] != nil {
				cancel()
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return upstream.GraphIngestResult{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return upstream.GraphIngestResult{}, err
	}
	merged := merge(slices.Concat(parts...))

	result := upstream.GraphIngestResult{DocumentID: document.ID, OriginURI: document.OriginURI, Revision: document.Revision}
	err = g.e.update(ctx, func(tx *sql.Tx) error {
		refs := make([]entityRef, len(merged.entities))
		for i, entity := range merged.entities {
			ref, err := resolveEntity(ctx, tx, entity.name, entity.kind, entity.aliases)
			if err != nil {
				return err
			}
			refs[i] = ref
		}
		var factRIDs []int64
		for _, fact := range merged.facts {
			row, _, err := upsertFact(ctx, tx, factInput{
				entity: refs[fact.entity], predicate: fact.predicate, object: fact.value,
				confidence: fact.confidence, validFrom: fact.validFrom,
				sourceDocumentID: document.ID, evidence: fact.evidence,
			})
			if err != nil {
				return err
			}
			if !slices.Contains(factRIDs, row.rid) {
				factRIDs = append(factRIDs, row.rid)
			}
		}
		seenEntities := map[int64]bool{}
		for _, ref := range refs {
			if !seenEntities[ref.rid] {
				seenEntities[ref.rid] = true
				result.Entities = append(result.Entities, upstream.GraphEntity{BrainID: ref.id, Name: ref.name, Type: ref.kind})
			}
		}
		// Read statuses after every write: a later fact of this document can
		// supersede an earlier one.
		for _, rid := range factRIDs {
			fact, err := readFact(ctx, tx, rid)
			if err != nil {
				return err
			}
			fact.SourceDocumentID = document.ID
			result.Facts = append(result.Facts, fact)
		}
		seenRelations := map[int64]bool{}
		for _, relation := range merged.relations {
			row, err := upsertRelation(ctx, tx, refs[relation.from], refs[relation.to], relation.predicate,
				relation.confidence, document.ID)
			if err != nil {
				return err
			}
			if seenRelations[row.rid] {
				continue
			}
			seenRelations[row.rid] = true
			result.Relations = append(result.Relations, upstream.GraphRelation{
				BrainID: row.id, FromEntityBrainID: refs[relation.from].id, ToEntityBrainID: refs[relation.to].id,
				Predicate: row.predicate, Status: row.status, Confidence: row.confidence, SourceDocumentID: document.ID,
			})
		}
		for _, entity := range result.Entities {
			result.EntityIDs = append(result.EntityIDs, entity.BrainID)
		}
		for _, fact := range result.Facts {
			result.FactIDs = append(result.FactIDs, fact.BrainID)
		}
		for _, relation := range result.Relations {
			result.EdgeIDs = append(result.EdgeIDs, relation.BrainID)
		}
		encoded, err := encodeResult(result)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ingests(document_id, revision, result, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(document_id, revision) DO UPDATE SET result = excluded.result`,
			document.ID, revision, encoded, nowText()); err != nil {
			return err
		}
		// A rename rollback can repeat the previous revisions; older ones
		// are never asked for again.
		_, err = tx.ExecContext(ctx, `DELETE FROM ingests WHERE document_id = ? AND revision < ?`, document.ID, revision-2)
		return err
	})
	if err != nil {
		return upstream.GraphIngestResult{}, err
	}
	g.e.notify()
	return result, nil
}

// HasStoredResult reports whether the index holds the graph result of the
// document revision, so a repeated ingest of it needs no model call.
func (g *Graph) HasStoredResult(ctx context.Context, documentID, revision string) (bool, error) {
	number, err := revisionNumber(revision)
	if err != nil {
		return false, err
	}
	var found int
	err = g.e.db.QueryRowContext(ctx, `SELECT 1 FROM ingests WHERE document_id = ? AND revision = ?`,
		documentID, number).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (g *Graph) storedResult(ctx context.Context, documentID string, revision int) (upstream.GraphIngestResult, bool, error) {
	var encoded string
	err := g.e.db.QueryRowContext(ctx, `SELECT result FROM ingests WHERE document_id = ? AND revision = ?`,
		documentID, revision).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return upstream.GraphIngestResult{}, false, nil
	}
	if err != nil {
		return upstream.GraphIngestResult{}, false, err
	}
	var result upstream.GraphIngestResult
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		return upstream.GraphIngestResult{}, false, fmt.Errorf("decode stored ingest result: %w", err)
	}
	return result, true, nil
}

type entityRef struct {
	rid            int64
	id, name, kind string
}

// resolveEntity finds the stored entity that name or an alias names, the
// same kind first, or creates it. It records new aliases.
func resolveEntity(ctx context.Context, tx *sql.Tx, name, kind string, aliases []string) (entityRef, error) {
	norm := normalize(name)
	norms := []string{norm}
	for _, alias := range aliases {
		if aliasNorm := normalize(alias); aliasNorm != "" && !slices.Contains(norms, aliasNorm) {
			norms = append(norms, aliasNorm)
		}
	}
	ref, found, err := findEntity(ctx, tx, norms, kind)
	if err != nil {
		return entityRef{}, err
	}
	if !found {
		ref = entityRef{id: xid.New().String(), name: name, kind: kind}
		inserted, err := tx.ExecContext(ctx, `INSERT INTO entities(id, name, norm, kind, created_at) VALUES (?, ?, ?, ?, ?)`,
			ref.id, name, norm, kind, nowText())
		if err != nil {
			return entityRef{}, err
		}
		if ref.rid, err = inserted.LastInsertId(); err != nil {
			return entityRef{}, err
		}
	}
	added := !found
	for _, aliasNorm := range norms {
		var stored string
		if err := tx.QueryRowContext(ctx, `SELECT norm FROM entities WHERE rid = ?`, ref.rid).Scan(&stored); err != nil {
			return entityRef{}, err
		}
		if aliasNorm == stored {
			continue
		}
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO entity_aliases(norm, entity_rid) VALUES (?, ?)`, aliasNorm, ref.rid)
		if err != nil {
			return entityRef{}, err
		}
		if n, _ := result.RowsAffected(); n > 0 {
			added = true
		}
	}
	if added {
		if err := indexEntity(ctx, tx, ref.rid); err != nil {
			return entityRef{}, err
		}
	}
	return ref, nil
}

func findEntity(ctx context.Context, tx *sql.Tx, norms []string, kind string) (entityRef, bool, error) {
	for _, query := range []string{
		`SELECT rid, id, name, kind FROM entities WHERE norm = ? ORDER BY rid`,
		`SELECT e.rid, e.id, e.name, e.kind FROM entity_aliases a JOIN entities e ON e.rid = a.entity_rid
			WHERE a.norm = ? ORDER BY e.rid`,
	} {
		for _, norm := range norms {
			rows, err := tx.QueryContext(ctx, query, norm)
			if err != nil {
				return entityRef{}, false, err
			}
			var candidates []entityRef
			for rows.Next() {
				var ref entityRef
				if err := rows.Scan(&ref.rid, &ref.id, &ref.name, &ref.kind); err != nil {
					rows.Close()
					return entityRef{}, false, err
				}
				candidates = append(candidates, ref)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return entityRef{}, false, err
			}
			if len(candidates) == 0 {
				continue
			}
			for _, candidate := range candidates {
				if candidate.kind == kind {
					return candidate, true, nil
				}
			}
			return candidates[0], true, nil
		}
	}
	return entityRef{}, false, nil
}

// indexEntity writes the entity's name and aliases to its search row.
func indexEntity(ctx context.Context, tx *sql.Tx, rid int64) error {
	var text string
	if err := tx.QueryRowContext(ctx, `
		SELECT e.name || coalesce(' ' || (SELECT group_concat(norm, ' ') FROM entity_aliases WHERE entity_rid = e.rid), '')
		FROM entities e WHERE e.rid = ?`, rid).Scan(&text); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM entities_fts WHERE rowid = ?`, rid); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO entities_fts(rowid, text) VALUES (?, ?)`, rid, text)
	return err
}

type factInput struct {
	entity           entityRef
	predicate        string
	object           string
	confidence       float64
	validFrom        time.Time
	validUntil       *time.Time
	sourceDocumentID string
	evidence         string
}

type factRow struct {
	rid       int64
	id        string
	status    string
	validFrom string
}

// upsertFact stores a statement. The same entity, predicate and normalized
// value reuse the stored fact and make a retracted one active again; a new
// value of a single-valued predicate supersedes the older value.
func upsertFact(ctx context.Context, tx *sql.Tx, in factInput) (factRow, string, error) {
	objectNorm := normalize(in.object)
	if objectNorm == "" {
		objectNorm = strings.ToLower(strings.TrimSpace(in.object))
	}
	var row factRow
	err := tx.QueryRowContext(ctx, `
		SELECT rid, id, status, valid_from FROM facts
		WHERE entity_rid = ? AND predicate = ? AND object_norm = ?
		ORDER BY CASE status WHEN 'active' THEN 0 WHEN 'superseded' THEN 1 ELSE 2 END, rid LIMIT 1`,
		in.entity.rid, in.predicate, objectNorm).Scan(&row.rid, &row.id, &row.status, &row.validFrom)
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx, `UPDATE facts SET confidence = max(confidence, ?) WHERE rid = ?`,
			in.confidence, row.rid); err != nil {
			return factRow{}, "", err
		}
		if row.status != "retracted" {
			return row, "corroborated", nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE facts SET status = 'active', retract_reason = NULL, superseded_by = NULL, valid_until = NULL,
				embedded = 0, source_document_id = ?
			WHERE rid = ?`, in.sourceDocumentID, row.rid); err != nil {
			return factRow{}, "", err
		}
		row.status = "active"
		if err := indexFact(ctx, tx, row.rid); err != nil {
			return factRow{}, "", err
		}
		if err := applySingleValued(ctx, tx, in.entity.rid, in.predicate, &row); err != nil {
			return factRow{}, "", err
		}
		return row, "corroborated", nil
	case !errors.Is(err, sql.ErrNoRows):
		return factRow{}, "", err
	}
	row = factRow{id: xid.New().String(), status: "active", validFrom: timestamp(in.validFrom)}
	var validUntil any
	if in.validUntil != nil {
		validUntil = timestamp(*in.validUntil)
	}
	inserted, err := tx.ExecContext(ctx, `
		INSERT INTO facts(id, entity_rid, predicate, object, object_norm, status, confidence, valid_from, valid_until,
			source_document_id, evidence, created_at)
		VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?, ?, ?, ?)`,
		row.id, in.entity.rid, in.predicate, in.object, objectNorm, in.confidence, row.validFrom, validUntil,
		in.sourceDocumentID, in.evidence, nowText())
	if err != nil {
		return factRow{}, "", err
	}
	if row.rid, err = inserted.LastInsertId(); err != nil {
		return factRow{}, "", err
	}
	if err := indexFact(ctx, tx, row.rid); err != nil {
		return factRow{}, "", err
	}
	if err := applySingleValued(ctx, tx, in.entity.rid, in.predicate, &row); err != nil {
		return factRow{}, "", err
	}
	return row, "created", nil
}

// applySingleValued keeps one active value of a single-valued predicate:
// the one valid from the latest time.
func applySingleValued(ctx context.Context, tx *sql.Tx, entityRID int64, predicate string, row *factRow) error {
	if !singleValued[predicate] || row.status != "active" {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT rid, valid_from FROM facts
		WHERE entity_rid = ? AND predicate = ? AND status = 'active' AND rid <> ?
		ORDER BY valid_from DESC, rid DESC`, entityRID, predicate, row.rid)
	if err != nil {
		return err
	}
	type active struct {
		rid       int64
		validFrom string
	}
	var others []active
	for rows.Next() {
		var other active
		if err := rows.Scan(&other.rid, &other.validFrom); err != nil {
			rows.Close()
			return err
		}
		others = append(others, other)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(others) > 0 && others[0].validFrom > row.validFrom {
		// A newer value is current already; this statement is history.
		_, err := tx.ExecContext(ctx, `
			UPDATE facts SET status = 'superseded', superseded_by = ?, valid_until = ? WHERE rid = ?`,
			others[0].rid, others[0].validFrom, row.rid)
		row.status = "superseded"
		return err
	}
	for _, other := range others {
		if _, err := tx.ExecContext(ctx, `
			UPDATE facts SET status = 'superseded', superseded_by = ?, valid_until = ? WHERE rid = ?`,
			row.rid, row.validFrom, other.rid); err != nil {
			return err
		}
	}
	return nil
}

// indexFact writes the fact's search row.
func indexFact(ctx context.Context, tx *sql.Tx, rid int64) error {
	text, err := factText(ctx, tx, rid)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM facts_fts WHERE rowid = ?`, rid); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO facts_fts(rowid, text) VALUES (?, ?)`, rid, text)
	return err
}

// factText is the text that search and embeddings see for a fact.
func factText(ctx context.Context, q queryer, rid int64) (string, error) {
	var name, predicate, object string
	if err := q.QueryRowContext(ctx, `
		SELECT e.name, f.predicate, f.object FROM facts f JOIN entities e ON e.rid = f.entity_rid WHERE f.rid = ?`,
		rid).Scan(&name, &predicate, &object); err != nil {
		return "", err
	}
	return name + " " + predicateWords(predicate) + " " + object, nil
}

func readFact(ctx context.Context, q queryer, rid int64) (upstream.GraphFact, error) {
	var fact upstream.GraphFact
	var validFrom string
	var validUntil sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT f.id, e.id, f.predicate, f.object, f.status, f.confidence, f.valid_from, f.valid_until, f.source_document_id
		FROM facts f JOIN entities e ON e.rid = f.entity_rid WHERE f.rid = ?`, rid).Scan(
		&fact.BrainID, &fact.EntityBrainID, &fact.Predicate, &fact.Object, &fact.Status, &fact.Confidence,
		&validFrom, &validUntil, &fact.SourceDocumentID)
	if err != nil {
		return upstream.GraphFact{}, err
	}
	fact.ValidFrom = parseTimestamp(validFrom)
	if validUntil.Valid {
		until := parseTimestamp(validUntil.String)
		fact.ValidUntil = &until
	}
	return fact, nil
}

type relationRow struct {
	rid        int64
	id         string
	predicate  string
	status     string
	confidence float64
}

func upsertRelation(ctx context.Context, tx *sql.Tx, from, to entityRef, predicate string, confidence float64,
	sourceDocumentID string) (relationRow, error) {
	row := relationRow{predicate: predicate}
	err := tx.QueryRowContext(ctx, `
		SELECT rid, id, status, confidence FROM relations WHERE from_rid = ? AND to_rid = ? AND predicate = ?
		ORDER BY CASE status WHEN 'active' THEN 0 ELSE 1 END, rid LIMIT 1`,
		from.rid, to.rid, predicate).Scan(&row.rid, &row.id, &row.status, &row.confidence)
	switch {
	case err == nil:
		row.status, row.confidence = "active", max(row.confidence, confidence)
		_, err := tx.ExecContext(ctx, `UPDATE relations SET status = 'active', confidence = ? WHERE rid = ?`,
			row.confidence, row.rid)
		return row, err
	case !errors.Is(err, sql.ErrNoRows):
		return relationRow{}, err
	}
	row.id, row.status, row.confidence = xid.New().String(), "active", confidence
	inserted, err := tx.ExecContext(ctx, `
		INSERT INTO relations(id, from_rid, to_rid, predicate, status, confidence, source_document_id, created_at)
		VALUES (?, ?, ?, ?, 'active', ?, ?, ?)`, row.id, from.rid, to.rid, predicate, confidence, sourceDocumentID, nowText())
	if err != nil {
		return relationRow{}, err
	}
	row.rid, err = inserted.LastInsertId()
	return row, err
}

// RetractFact withdraws a fact or a relation. Retracting the current value
// of a single-valued predicate makes the value it superseded current again
// and returns that fact's ID. Retracting twice changes nothing.
func (g *Graph) RetractFact(ctx context.Context, id, reason string) ([]string, error) {
	var revived []string
	err := g.e.update(ctx, func(tx *sql.Tx) error {
		var rid int64
		var status string
		var successor sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT rid, status, superseded_by FROM facts WHERE id = ?`, id).Scan(&rid, &status, &successor)
		if errors.Is(err, sql.ErrNoRows) {
			result, err := tx.ExecContext(ctx, `UPDATE relations SET status = 'retracted' WHERE id = ?`, id)
			if err != nil {
				return err
			}
			if n, _ := result.RowsAffected(); n == 0 {
				return indexError("retract", http.StatusNotFound, errors.New("no such fact or relation"))
			}
			return nil
		}
		if err != nil || status == "retracted" {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE facts SET status = 'retracted', retract_reason = ?, embedded = 0 WHERE rid = ?`, reason, rid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM facts_fts WHERE rowid = ?`, rid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM fact_vec WHERE rid = ?`, rid); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT rid, id FROM facts WHERE superseded_by = ? AND status = 'superseded'
			ORDER BY valid_from DESC, rid DESC`, rid)
		if err != nil {
			return err
		}
		var predecessors []int64
		var predecessorIDs []string
		for rows.Next() {
			var predecessor int64
			var predecessorID string
			if err := rows.Scan(&predecessor, &predecessorID); err != nil {
				rows.Close()
				return err
			}
			predecessors = append(predecessors, predecessor)
			predecessorIDs = append(predecessorIDs, predecessorID)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(predecessors) == 0 {
			return err
		}
		if status == "superseded" {
			// The retracted fact was history itself: its predecessors now
			// follow its successor.
			_, err := tx.ExecContext(ctx, `UPDATE facts SET superseded_by = ? WHERE superseded_by = ?`, successor, rid)
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE facts SET status = 'active', superseded_by = NULL, valid_until = NULL WHERE rid = ?`,
			predecessors[0]); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE facts SET superseded_by = ? WHERE superseded_by = ?`,
			predecessors[0], rid); err != nil {
			return err
		}
		revived = append(revived, predecessorIDs[0])
		return nil
	})
	return revived, err
}

// IngestFact stores a fact recorded through the API and returns its ID and
// whether it was "created" or "corroborated".
func (g *Graph) IngestFact(ctx context.Context, fact model.Fact, entity model.Entity) (string, string, error) {
	var id, outcome string
	err := g.e.update(ctx, func(tx *sql.Tx) error {
		ref, err := resolveAPIEntity(ctx, tx, entity)
		if err != nil {
			return err
		}
		validFrom := fact.ValidFrom
		if validFrom.IsZero() {
			validFrom = time.Now()
		}
		confidence := fact.Confidence
		if confidence <= 0 || confidence > 1 {
			confidence = 1
		}
		row, result, err := upsertFact(ctx, tx, factInput{
			entity: ref, predicate: strings.TrimSpace(fact.Predicate), object: strings.TrimSpace(fact.Object),
			confidence: confidence, validFrom: validFrom, validUntil: fact.ValidUntil,
			sourceDocumentID: fact.SourceDocumentID,
		})
		id, outcome = row.id, result
		return err
	})
	if err == nil {
		g.e.notify()
	}
	return id, outcome, err
}

// IngestRelation stores a relation recorded through the API.
func (g *Graph) IngestRelation(ctx context.Context, relation model.Relation, from, to model.Entity) (string, error) {
	var id string
	err := g.e.update(ctx, func(tx *sql.Tx) error {
		fromRef, err := resolveAPIEntity(ctx, tx, from)
		if err != nil {
			return err
		}
		toRef, err := resolveAPIEntity(ctx, tx, to)
		if err != nil {
			return err
		}
		confidence := relation.Confidence
		if confidence <= 0 || confidence > 1 {
			confidence = 1
		}
		row, err := upsertRelation(ctx, tx, fromRef, toRef, strings.TrimSpace(relation.Predicate), confidence, "")
		id = row.id
		return err
	})
	return id, err
}

// resolveAPIEntity maps a Manifold entity to the graph: by the upstream ID
// a derived entity carries, else by name.
func resolveAPIEntity(ctx context.Context, tx *sql.Tx, entity model.Entity) (entityRef, error) {
	if entity.UpstreamID != "" {
		var ref entityRef
		err := tx.QueryRowContext(ctx, `SELECT rid, id, name, kind FROM entities WHERE id = ?`, entity.UpstreamID).Scan(
			&ref.rid, &ref.id, &ref.name, &ref.kind)
		if err == nil {
			return ref, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return entityRef{}, err
		}
	}
	name := entity.Name
	if strings.TrimSpace(name) == "" {
		name = entity.ID
	}
	kind := entity.Kind
	if kind == "" {
		kind = "other"
	}
	return resolveEntity(ctx, tx, name, kind, nil)
}

// maxVectorDistance drops vector neighbours that share nothing with the
// query: a cosine distance above it means unrelated text.
const maxVectorDistance = 0.8

// Search ranks facts by words and by meaning and fuses both lists. Without
// history it returns current facts only. When the query cannot be embedded,
// the word ranking alone answers.
func (g *Graph) Search(ctx context.Context, query string, limit int, includeHistory bool) ([]model.SearchHit, error) {
	if limit <= 0 {
		return nil, nil
	}
	statuses := `('active')`
	if includeHistory {
		statuses = `('active', 'superseded')`
	}
	fetch := limit*3 + 10
	var lists [][]int64
	if match := ftsQuery(query, 32); match != "" {
		lexical, err := g.ridList(ctx, `
			SELECT f.rid FROM (SELECT rowid, rank FROM facts_fts WHERE facts_fts MATCH ? ORDER BY rank LIMIT ?) m
			JOIN facts f ON f.rid = m.rowid WHERE f.status IN `+statuses+` ORDER BY m.rank`, match, fetch)
		if err != nil {
			return nil, err
		}
		lists = append(lists, lexical)
	}
	if vector, err := g.e.queries.vector(ctx, query, g.e.model.Embed); err == nil {
		semantic, err := g.ridList(ctx, `
			SELECT f.rid FROM (SELECT rid, distance FROM fact_vec WHERE emb MATCH vec_int8(?) AND k = ?) v
			JOIN facts f ON f.rid = v.rid WHERE f.status IN `+statuses+` AND v.distance <= ? ORDER BY v.distance`,
			vector, fetch, maxVectorDistance)
		if err != nil {
			return nil, err
		}
		lists = append(lists, semantic)
	} else if ctx.Err() != nil {
		return nil, ctx.Err()
	} else {
		g.e.logger.Warn("graph search without query vector", "error", err)
	}
	ranked := fuse(lists, limit)
	hits := make([]model.SearchHit, 0, len(ranked))
	for _, item := range ranked {
		var hit model.SearchHit
		var predicate, object string
		if err := g.e.db.QueryRowContext(ctx, `
			SELECT f.id, e.name, f.predicate, f.object FROM facts f JOIN entities e ON e.rid = f.entity_rid WHERE f.rid = ?`,
			item.rid).Scan(&hit.ID, &hit.Title, &predicate, &object); err != nil {
			return nil, err
		}
		hit.Kind, hit.Source, hit.Score = "fact", "graph", item.score
		hit.Snippet = predicate + " " + object
		hits = append(hits, hit)
	}
	return hits, nil
}

func (g *Graph) ridList(ctx context.Context, query string, args ...any) ([]int64, error) {
	rows, err := g.e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rids []int64
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			return nil, err
		}
		rids = append(rids, rid)
	}
	return rids, rows.Err()
}

type rankedRID struct {
	rid   int64
	score float64
}

// fuse merges ranked lists by reciprocal rank fusion.
func fuse(lists [][]int64, limit int) []rankedRID {
	scores := map[int64]float64{}
	var order []int64
	for _, list := range lists {
		for rank, rid := range list {
			if _, ok := scores[rid]; !ok {
				order = append(order, rid)
			}
			scores[rid] += 1 / float64(60+rank+1)
		}
	}
	ranked := make([]rankedRID, 0, len(order))
	for _, rid := range order {
		ranked = append(ranked, rankedRID{rid: rid, score: scores[rid]})
	}
	slices.SortStableFunc(ranked, func(a, b rankedRID) int {
		switch {
		case a.score > b.score:
			return -1
		case a.score < b.score:
			return 1
		default:
			return 0
		}
	})
	return ranked[:min(limit, len(ranked))]
}
