package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/iamwavecut/Manifold/internal/identity"
)

const derivedGraphOrigin = "derived"

var ErrDerivedGraphSnapshotConflict = errors.New("derived graph revision snapshot is immutable")

type DerivedGraphProjection struct {
	DocumentID string
	Revision   string
	Entities   []DerivedGraphEntity
	Facts      []DerivedGraphFact
	Relations  []DerivedGraphRelation
}

type DerivedGraphEntity struct {
	CommitRef string
	Name      string
	Kind      string
	SourceKey string
	OriginKey string
}

type DerivedGraphFact struct {
	CommitRef       string
	EntityCommitRef string
	Predicate       string
	Object          string
	Status          string
	Confidence      float64
	ValidFrom       time.Time
	ValidUntil      *time.Time
	SourceKey       string
	OriginKey       string
}

type DerivedGraphRelation struct {
	CommitRef           string
	FromEntityCommitRef string
	ToEntityCommitRef   string
	Predicate           string
	Status              string
	Confidence          float64
	SourceKey           string
	OriginKey           string
}

func (s *Store) ProjectDerivedGraph(ctx context.Context, projection DerivedGraphProjection) error {
	revisionNumber := revisionNumber(projection.Revision)
	if projection.DocumentID == "" || revisionNumber < 1 || projection.Revision != fmt.Sprintf("r%d", revisionNumber) {
		return errors.New("invalid derived graph source document revision")
	}
	return s.InTx(ctx, func(tx *sql.Tx) error {
		var currentRevision, deleted int
		err := tx.QueryRowContext(ctx, `
			SELECT revision, deleted FROM documents WHERE id = ?`, projection.DocumentID).Scan(&currentRevision, &deleted)
		if errors.Is(err, sql.ErrNoRows) || deleted != 0 {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if currentRevision != revisionNumber {
			return ErrDocumentSyncSuperseded
		}
		timestamp := now()
		changed := false
		expectedClaims := make(map[derivedGraphClaimKey]struct{})

		entities := append([]DerivedGraphEntity(nil), projection.Entities...)
		sort.Slice(entities, func(i, j int) bool {
			left, right := identity.Slug(entities[i].Name), identity.Slug(entities[j].Name)
			if left != right {
				return left < right
			}
			return entities[i].CommitRef < entities[j].CommitRef
		})
		for _, entity := range entities {
			if entity.CommitRef == "" || strings.TrimSpace(entity.Name) == "" {
				return errors.New("derived graph entity is missing its identity or name")
			}
			id, entityChanged, err := projectEntity(ctx, tx, entity, timestamp)
			if err != nil {
				return err
			}
			changed = changed || entityChanged
			expectedClaims[derivedGraphClaimKey{
				kind: "entity", objectID: id, revision: revisionNumber,
				sourceKey: entity.SourceKey, originKey: entity.OriginKey,
			}] = struct{}{}
		}

		for _, fact := range projection.Facts {
			if fact.CommitRef == "" || fact.EntityCommitRef == "" || fact.Predicate == "" {
				return errors.New("derived graph fact is missing its identity, entity, or predicate")
			}
			entityID, err := entityIDByCommitRef(ctx, tx, fact.EntityCommitRef)
			if err != nil {
				return errors.New("derived graph fact references an unmapped entity")
			}
			id, factChanged, err := projectFact(ctx, tx, fact, entityID, projection, timestamp)
			if err != nil {
				return err
			}
			changed = changed || factChanged
			expectedClaims[derivedGraphClaimKey{
				kind: "fact", objectID: id, revision: revisionNumber,
				sourceKey: fact.SourceKey, originKey: fact.OriginKey,
			}] = struct{}{}
			snapshotChanged, err := saveDerivedFactSnapshot(ctx, tx, id, entityID, fact, projection, timestamp)
			if err != nil {
				return err
			}
			changed = changed || snapshotChanged
		}

		for _, relation := range projection.Relations {
			if relation.CommitRef == "" || relation.FromEntityCommitRef == "" ||
				relation.ToEntityCommitRef == "" || relation.Predicate == "" {
				return errors.New("derived graph relation is missing its identity, endpoint, or predicate")
			}
			fromID, err := entityIDByCommitRef(ctx, tx, relation.FromEntityCommitRef)
			if err != nil {
				return errors.New("derived graph relation references an unmapped source entity")
			}
			toID, err := entityIDByCommitRef(ctx, tx, relation.ToEntityCommitRef)
			if err != nil {
				return errors.New("derived graph relation references an unmapped target entity")
			}
			id, relationChanged, err := projectRelation(ctx, tx, relation, fromID, toID, timestamp)
			if err != nil {
				return err
			}
			changed = changed || relationChanged
			expectedClaims[derivedGraphClaimKey{
				kind: "relation", objectID: id, revision: revisionNumber,
				sourceKey: relation.SourceKey, originKey: relation.OriginKey,
			}] = struct{}{}
		}

		deactivated, err := deactivateMissingDerivedGraphClaims(ctx, tx, projection.DocumentID, expectedClaims)
		if err != nil {
			return err
		}
		changed = changed || deactivated
		for key := range expectedClaims {
			claimChanged, err := insertDerivedGraphClaim(ctx, tx, key, projection.DocumentID, timestamp)
			if err != nil {
				return err
			}
			changed = changed || claimChanged
		}
		if !changed {
			return nil
		}
		return bumpVersion(ctx, tx)
	})
}

type derivedGraphClaimKey struct {
	kind      string
	objectID  string
	revision  int
	sourceKey string
	originKey string
}

func projectEntity(ctx context.Context, tx *sql.Tx, entity DerivedGraphEntity, timestamp string) (string, bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM entities WHERE upstream_id = ? LIMIT 1`, entity.CommitRef).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		id, err = availableDerivedEntityID(ctx, tx, entity.Name)
		if err != nil {
			return "", false, err
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO entities(id, name, kind, upstream_id, metadata_json, created_at, updated_at)
			VALUES (?, ?, ?, ?, '{}', ?, ?)`, id, entity.Name, entity.Kind, entity.CommitRef, timestamp, timestamp)
		if err != nil {
			return "", false, err
		}
		return id, resultChanged(result), nil
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE entities SET name = ?, kind = ?, updated_at = ?
		WHERE id = ? AND (name IS NOT ? OR kind IS NOT ?)`,
		entity.Name, entity.Kind, timestamp, id, entity.Name, entity.Kind)
	if err != nil {
		return "", false, err
	}
	return id, resultChanged(result), nil
}

func availableDerivedEntityID(ctx context.Context, tx *sql.Tx, name string) (string, error) {
	base := identity.Slug(name)
	occupied, err := derivedEntityIDUnavailable(ctx, tx, base)
	if err != nil {
		return "", err
	}
	if !occupied {
		return base, nil
	}
	for suffix := 2; suffix < 10_000; suffix++ {
		tail := fmt.Sprintf("-%d", suffix)
		prefix := strings.TrimRight(base[:min(len(base), 63-len(tail))], "-")
		candidate := prefix + tail
		occupied, err := derivedEntityIDUnavailable(ctx, tx, candidate)
		if err != nil {
			return "", err
		}
		if !occupied {
			return candidate, nil
		}
	}
	return "", errors.New("unable to allocate a derived entity ID")
}

func derivedEntityIDUnavailable(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var exists int
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM entities WHERE id = ?)
			OR EXISTS(SELECT 1 FROM rename_reservations WHERE resource_type = 'entity' AND slug = ?)`, id, id).Scan(&exists)
	return exists != 0, err
}

func entityIDByCommitRef(ctx context.Context, tx *sql.Tx, commitRef string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM entities WHERE upstream_id = ? LIMIT 1`, commitRef).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

func projectFact(
	ctx context.Context,
	tx *sql.Tx,
	fact DerivedGraphFact,
	entityID string,
	projection DerivedGraphProjection,
	timestamp string,
) (string, bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM facts WHERE upstream_id = ? AND origin = ? LIMIT 1`, fact.CommitRef, derivedGraphOrigin).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		id = identity.NewXID()
	}
	status := fact.Status
	if status == "" {
		status = "active"
	}
	validFrom := timestamp
	if !fact.ValidFrom.IsZero() {
		validFrom = fact.ValidFrom.UTC().Format(time.RFC3339Nano)
	} else if id != "" {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT valid_from FROM facts WHERE id = ?`, id).Scan(&existing)
		if err == nil {
			validFrom = existing
		} else if !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}
	}
	var validUntil any
	if fact.ValidUntil != nil {
		validUntil = fact.ValidUntil.UTC().Format(time.RFC3339Nano)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO facts(id, entity_id, predicate, object, origin, status, confidence,
			source_document_id, source_revision, upstream_id, valid_from, valid_until, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET entity_id = excluded.entity_id, predicate = excluded.predicate,
			object = excluded.object, origin = excluded.origin, status = excluded.status,
			confidence = excluded.confidence, source_document_id = excluded.source_document_id,
			source_revision = excluded.source_revision, upstream_id = excluded.upstream_id,
			valid_from = excluded.valid_from, valid_until = excluded.valid_until
		WHERE entity_id IS NOT excluded.entity_id OR predicate IS NOT excluded.predicate OR
			object IS NOT excluded.object OR origin IS NOT excluded.origin OR status IS NOT excluded.status OR
			confidence IS NOT excluded.confidence OR source_document_id IS NOT excluded.source_document_id OR
			source_revision IS NOT excluded.source_revision OR upstream_id IS NOT excluded.upstream_id OR
			valid_from IS NOT excluded.valid_from OR valid_until IS NOT excluded.valid_until`,
		id, entityID, fact.Predicate, fact.Object, derivedGraphOrigin, status, fact.Confidence,
		projection.DocumentID, revisionNumber(projection.Revision), fact.CommitRef, validFrom, validUntil, timestamp)
	if err != nil {
		return "", false, err
	}
	return id, resultChanged(result), nil
}

func projectRelation(
	ctx context.Context,
	tx *sql.Tx,
	relation DerivedGraphRelation,
	fromID, toID string,
	timestamp string,
) (string, bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM relations WHERE upstream_id = ? AND origin = ? LIMIT 1`, relation.CommitRef, derivedGraphOrigin).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		id = identity.NewXID()
	}
	status := relation.Status
	if status == "" {
		status = "active"
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO relations(id, from_entity_id, to_entity_id, predicate, origin, status, confidence, upstream_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET from_entity_id = excluded.from_entity_id, to_entity_id = excluded.to_entity_id,
			predicate = excluded.predicate, origin = excluded.origin, status = excluded.status,
			confidence = excluded.confidence, upstream_id = excluded.upstream_id
		WHERE from_entity_id IS NOT excluded.from_entity_id OR to_entity_id IS NOT excluded.to_entity_id OR
			predicate IS NOT excluded.predicate OR origin IS NOT excluded.origin OR status IS NOT excluded.status OR
			confidence IS NOT excluded.confidence OR upstream_id IS NOT excluded.upstream_id`,
		id, fromID, toID, relation.Predicate, derivedGraphOrigin, status, relation.Confidence, relation.CommitRef, timestamp)
	if err != nil {
		return "", false, err
	}
	return id, resultChanged(result), nil
}

func saveDerivedFactSnapshot(
	ctx context.Context,
	tx *sql.Tx,
	objectID, entityID string,
	fact DerivedGraphFact,
	projection DerivedGraphProjection,
	timestamp string,
) (bool, error) {
	status := fact.Status
	if status == "" {
		status = "active"
	}
	validFrom := timestamp
	if !fact.ValidFrom.IsZero() {
		validFrom = fact.ValidFrom.UTC().Format(time.RFC3339Nano)
	} else {
		var existing string
		err := tx.QueryRowContext(ctx, `
			SELECT valid_from FROM facts WHERE upstream_id = ? AND origin = ?`, fact.CommitRef, derivedGraphOrigin).Scan(&existing)
		if err == nil {
			validFrom = existing
		} else if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	var validUntil any
	if fact.ValidUntil != nil {
		validUntil = fact.ValidUntil.UTC().Format(time.RFC3339Nano)
	}
	var entityName string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM entities WHERE id = ?`, entityID).Scan(&entityName); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO derived_graph_fact_snapshots(object_id, document_id, revision, source_key, origin_key,
			entity_id, entity_name, predicate, object, status, confidence, valid_from, valid_until)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		objectID, projection.DocumentID, revisionNumber(projection.Revision), fact.SourceKey, fact.OriginKey,
		entityID, entityName, fact.Predicate, fact.Object, status, fact.Confidence, validFrom, validUntil)
	if err != nil {
		return false, err
	}
	var storedEntityID, storedEntityName, storedPredicate, storedObject, storedStatus, storedValidFrom string
	var storedConfidence float64
	var storedValidUntil sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT entity_id, entity_name, predicate, object, status, confidence, valid_from, valid_until
		FROM derived_graph_fact_snapshots
		WHERE object_id = ? AND document_id = ? AND revision = ? AND source_key = ? AND origin_key = ?`,
		objectID, projection.DocumentID, revisionNumber(projection.Revision), fact.SourceKey, fact.OriginKey).Scan(
		&storedEntityID, &storedEntityName, &storedPredicate, &storedObject, &storedStatus,
		&storedConfidence, &storedValidFrom, &storedValidUntil,
	); err != nil {
		return false, err
	}
	wantValidUntil := sql.NullString{}
	if validUntil != nil {
		wantValidUntil = sql.NullString{String: validUntil.(string), Valid: true}
	}
	if storedEntityID != entityID || storedEntityName != entityName || storedPredicate != fact.Predicate ||
		storedObject != fact.Object || storedStatus != status || storedConfidence != fact.Confidence ||
		storedValidFrom != validFrom || storedValidUntil != wantValidUntil {
		return false, ErrDerivedGraphSnapshotConflict
	}
	return resultChanged(result), nil
}

func deactivateMissingDerivedGraphClaims(
	ctx context.Context,
	tx *sql.Tx,
	documentID string,
	expected map[derivedGraphClaimKey]struct{},
) (bool, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT object_kind, object_id, revision, source_key, origin_key
		FROM derived_graph_claims WHERE document_id = ? AND active = 1`, documentID)
	if err != nil {
		return false, err
	}
	var stale []derivedGraphClaimKey
	for rows.Next() {
		var key derivedGraphClaimKey
		if err := rows.Scan(&key.kind, &key.objectID, &key.revision, &key.sourceKey, &key.originKey); err != nil {
			rows.Close()
			return false, err
		}
		if _, ok := expected[key]; !ok {
			stale = append(stale, key)
		}
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	changed := false
	for _, key := range stale {
		result, err := tx.ExecContext(ctx, `
			UPDATE derived_graph_claims SET active = 0
			WHERE object_kind = ? AND object_id = ? AND document_id = ? AND revision = ?
				AND source_key = ? AND origin_key = ? AND active = 1`,
			key.kind, key.objectID, documentID, key.revision, key.sourceKey, key.originKey)
		if err != nil {
			return false, err
		}
		changed = changed || resultChanged(result)
	}
	return changed, nil
}

func insertDerivedGraphClaim(
	ctx context.Context,
	tx *sql.Tx,
	key derivedGraphClaimKey,
	documentID, timestamp string,
) (bool, error) {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO derived_graph_claims(object_kind, object_id, document_id, revision, source_key, origin_key, active, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT(object_kind, object_id, document_id, revision, source_key, origin_key)
		DO UPDATE SET active = 1 WHERE derived_graph_claims.active = 0`,
		key.kind, key.objectID, documentID, key.revision, key.sourceKey, key.originKey, timestamp)
	if err != nil {
		return false, err
	}
	return resultChanged(result), nil
}

func currentDerivedGraphClaim(objectAlias, kind string) string {
	return fmt.Sprintf(`EXISTS (
		SELECT 1 FROM derived_graph_claims c
		JOIN documents source ON source.id = c.document_id
		WHERE c.object_kind = '%s' AND c.object_id = %s.id AND c.active = 1
			AND source.deleted = 0 AND source.revision = c.revision
	)`, kind, objectAlias)
}

func resultChanged(result sql.Result) bool {
	if result == nil {
		return false
	}
	affected, err := result.RowsAffected()
	return err == nil && affected > 0
}
