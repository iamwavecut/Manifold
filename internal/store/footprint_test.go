//go:build footprint

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"
)

// TestWriteFootprintCorpus writes a synthetic canonical database shaped like
// the production corpus of 2026-11-28 to $FOOTPRINT_DB, for footprint
// measurements of the index. Run with -tags footprint.
func TestWriteFootprintCorpus(t *testing.T) {
	path := os.Getenv("FOOTPRINT_DB")
	if path == "" {
		t.Skip("FOOTPRINT_DB is not set")
	}
	const (
		documents, revisions            = 214, 766
		textBytes                       = 8_400_000
		entities, facts, relations      = 9_098, 46_260, 11_036
		activeFacts, supersededFacts    = 19_245, 4_676
		folders, predicates, vocabulary = 20, 300, 6_000
	)
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rng := rand.New(rand.NewPCG(1, 2))
	words := make([]string, vocabulary)
	for i := range words {
		var b strings.Builder
		for range 3 + rng.IntN(7) {
			b.WriteByte(byte('a' + rng.IntN(26)))
		}
		words[i] = b.String()
	}
	// Zipf-like word choice, as in natural text.
	word := func() string { return words[int(float64(vocabulary)*rng.Float64()*rng.Float64()*rng.Float64())] }
	phrase := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = word()
		}
		return strings.Join(parts, " ")
	}
	text := func(size int) string {
		var b strings.Builder
		for section := 1; b.Len() < size; section++ {
			fmt.Fprintf(&b, "## Section %d %s\n\n", section, phrase(3))
			for paragraph := 0; paragraph < 4 && b.Len() < size; paragraph++ {
				for sentence := 0; sentence < 5; sentence++ {
					b.WriteString(phrase(8 + rng.IntN(12)))
					b.WriteString(". ")
				}
				b.WriteString("\n\n")
			}
		}
		return b.String()
	}
	stamp := func(offset int) string {
		return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(offset) * time.Minute).Format(time.RFC3339Nano)
	}
	hash := func(content string) string {
		sum := sha256.Sum256([]byte(content))
		return hex.EncodeToString(sum[:])
	}
	err = s.InTx(ctx, func(tx *sql.Tx) error {
		for f := range folders {
			if _, err := tx.ExecContext(ctx, `INSERT INTO folders(id, name, created_at, updated_at) VALUES (?, ?, ?, ?)`,
				fmt.Sprintf("area-%d", f), fmt.Sprintf("Area %d", f), stamp(0), stamp(0)); err != nil {
				return err
			}
		}
		currentRevision := make([]int, documents)
		for d := range documents {
			currentRevision[d] = 1
		}
		for extra := revisions - documents; extra > 0; extra-- {
			currentRevision[rng.IntN(documents)]++
		}
		for d := range documents {
			id := fmt.Sprintf("doc-%d", d)
			folder := fmt.Sprintf("area-%d", d%folders)
			uri := "viking://resources/manifold/" + folder + "/" + id + ".md"
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO documents(id, folder_id, title, format, content, content_hash, revision, status, ov_uri,
					created_at, updated_at)
				VALUES (?, ?, ?, 'markdown', '', '', 1, 'ready', ?, ?, ?)`,
				id, folder, "Document "+phrase(3), uri, stamp(d*10), stamp(d*10)); err != nil {
				return err
			}
			var content string
			for r := 1; r <= currentRevision[d]; r++ {
				content = text(textBytes / documents)
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO revisions(document_id, revision, content, content_hash, ov_uri, snapshot_oid, created_at)
					VALUES (?, ?, ?, ?, ?, ?, ?)`, id, r, content, hash(content), uri, fmt.Sprintf("snapshot-%d-%d", d, r), stamp(d*10+r)); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE documents SET content = ?, content_hash = ?, revision = ? WHERE id = ?`,
				content, hash(content), currentRevision[d], id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO document_fts(id, title, content) VALUES (?, ?, ?)`, id, id, content); err != nil {
				return err
			}
		}
		kinds := []string{"project", "person", "organization", "topic", "asset", "other"}
		for e := range entities {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO entities(id, name, kind, upstream_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
				fmt.Sprintf("entity-%d", e), strings.ToUpper(phrase(2)[:1])+phrase(2)[1:], kinds[e%len(kinds)], fmt.Sprintf("knowledge_entity:%d", e),
				stamp(0), stamp(0)); err != nil {
				return err
			}
		}
		claimedEntities := map[[2]int]bool{}
		for f := range facts {
			status := "retracted"
			switch {
			case f < activeFacts:
				status = "active"
			case f < activeFacts+supersededFacts:
				status = "superseded"
			}
			entity, document := rng.IntN(entities), rng.IntN(documents)
			id, object, predicate := fmt.Sprintf("fact%016d", f), phrase(1+rng.IntN(4)), fmt.Sprintf("predicate_%d", rng.IntN(predicates))
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO facts(id, entity_id, predicate, object, origin, status, confidence, source_document_id,
					source_revision, upstream_id, valid_from, created_at)
				VALUES (?, ?, ?, ?, 'derived', ?, 0.9, ?, ?, ?, ?, ?)`,
				id, fmt.Sprintf("entity-%d", entity), predicate, object, status, fmt.Sprintf("doc-%d", document),
				currentRevision[document], fmt.Sprintf("knowledge_fact:%d", f), stamp(f), stamp(f)); err != nil {
				return err
			}
			if status != "active" {
				continue
			}
			docID, revision := fmt.Sprintf("doc-%d", document), currentRevision[document]
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO derived_graph_claims(object_kind, object_id, document_id, revision, created_at)
				VALUES ('fact', ?, ?, ?, ?)`, id, docID, revision, stamp(f)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO derived_graph_fact_snapshots(object_id, document_id, revision, entity_id, entity_name,
					predicate, object, status, confidence, valid_from)
				SELECT ?, ?, ?, id, name, ?, ?, 'active', 0.9, ? FROM entities WHERE id = ?`,
				id, docID, revision, predicate, object, stamp(f), fmt.Sprintf("entity-%d", entity)); err != nil {
				return err
			}
			if !claimedEntities[[2]int{entity, document}] {
				claimedEntities[[2]int{entity, document}] = true
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO derived_graph_claims(object_kind, object_id, document_id, revision, created_at)
					VALUES ('entity', ?, ?, ?, ?)`, fmt.Sprintf("entity-%d", entity), docID, revision, stamp(f)); err != nil {
					return err
				}
			}
		}
		for r := range relations {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO relations(id, from_entity_id, to_entity_id, predicate, origin, status, confidence, upstream_id, created_at)
				VALUES (?, ?, ?, ?, 'derived', 'active', 0.8, ?, ?)`,
				fmt.Sprintf("relation%012d", r), fmt.Sprintf("entity-%d", rng.IntN(entities)), fmt.Sprintf("entity-%d", rng.IntN(entities)),
				fmt.Sprintf("predicate_%d", rng.IntN(predicates)), fmt.Sprintf("knowledge_edge:%d", r), stamp(r)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
