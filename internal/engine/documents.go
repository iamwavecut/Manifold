package engine

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/rs/xid"

	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/upstream"
)

// searchChunkSize is the largest piece of a document one vector covers.
const searchChunkSize = 1500

// Documents is the document index: the path tree of canonical documents and
// their text cut into embedded chunks. It implements upstream.DocumentStore.
// Folder locators end with a slash.
type Documents struct{ e *Engine }

func indexError(operation string, status int, cause error) error {
	return &upstream.DependencyError{Dependency: "index", Operation: operation, Status: status, Cause: cause}
}

func (d *Documents) Health(ctx context.Context) error { return d.e.Ping(ctx) }

func (d *Documents) Exists(ctx context.Context, uri string) (bool, error) {
	var exists bool
	err := d.e.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paths WHERE uri = ?)`, uri).Scan(&exists)
	return exists, err
}

func (d *Documents) Mkdir(ctx context.Context, uri, description string) error {
	return d.e.update(ctx, func(tx *sql.Tx) error {
		var kind string
		err := tx.QueryRowContext(ctx, `SELECT kind FROM paths WHERE uri = ?`, uri).Scan(&kind)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.ExecContext(ctx, `INSERT INTO paths(uri, kind, description) VALUES (?, 'folder', ?)`, uri, description)
			return err
		case err != nil:
			return err
		case kind != "folder":
			return indexError("mkdir", http.StatusConflict, errors.New("a file has this path"))
		default:
			return nil
		}
	})
}

// Write stores content at uri and queues its chunks for embedding. With
// create set, an existing file is a conflict.
func (d *Documents) Write(ctx context.Context, uri, content string, create bool) error {
	err := d.e.update(ctx, func(tx *sql.Tx) error {
		var kind string
		err := tx.QueryRowContext(ctx, `SELECT kind FROM paths WHERE uri = ?`, uri).Scan(&kind)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `INSERT INTO paths(uri, kind) VALUES (?, 'file')`, uri); err != nil {
				return err
			}
		case err != nil:
			return err
		case kind == "folder":
			return indexError("write", http.StatusConflict, errors.New("a folder has this path"))
		case create:
			return indexError("write", http.StatusConflict, errors.New("the file exists"))
		}
		return writeChunks(ctx, tx, uri, content)
	})
	if err == nil {
		d.e.notify()
	}
	return err
}

// writeChunks replaces the chunks of uri. Each chunk starts with the
// headings above it, so it reads on its own.
func writeChunks(ctx context.Context, tx *sql.Tx, uri, content string) error {
	if err := deleteChunks(ctx, tx, `uri = ?`, uri); err != nil {
		return err
	}
	for seq, part := range splitSections(content, searchChunkSize) {
		text := part.Text
		if part.Heading != "" && !strings.HasPrefix(strings.TrimSpace(text), "#") {
			text = part.Heading + "\n" + text
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chunks(uri, seq, text) VALUES (?, ?, ?)`, uri, seq, text); err != nil {
			return err
		}
	}
	return nil
}

func deleteChunks(ctx context.Context, tx *sql.Tx, where string, args ...any) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunk_vec WHERE rid IN (SELECT rid FROM chunks WHERE `+where+`)`, args...); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE `+where, args...)
	return err
}

// under matches a locator and, for a folder, everything below it.
func under(column, uri string) (string, []any) {
	if strings.HasSuffix(uri, "/") {
		return "(" + column + " >= ? AND " + column + " < ?)", []any{uri, uri[:len(uri)-1] + "0"}
	}
	return column + " = ?", []any{uri}
}

// Move renames a file or a folder with everything below it.
func (d *Documents) Move(ctx context.Context, from, to string) error {
	if from == to {
		return nil
	}
	if strings.HasSuffix(from, "/") != strings.HasSuffix(to, "/") {
		return indexError("move", http.StatusBadRequest, errors.New("a file and a folder cannot swap"))
	}
	return d.e.update(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paths WHERE uri = ?)`, from).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return indexError("move", http.StatusNotFound, errors.New("the source does not exist"))
		}
		where, args := under("uri", to)
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paths WHERE `+where+`)`, args...).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return indexError("move", http.StatusConflict, errors.New("the target exists"))
		}
		where, args = under("uri", from)
		rename := append([]any{to, len(from) + 1}, args...)
		if _, err := tx.ExecContext(ctx, `UPDATE paths SET uri = ? || substr(uri, ?) WHERE `+where, rename...); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE chunks SET uri = ? || substr(uri, ?) WHERE `+where, rename...)
		return err
	})
}

// Delete removes a file, or a folder with recursive set. A missing path is
// already deleted.
func (d *Documents) Delete(ctx context.Context, uri string, recursive bool) error {
	return d.e.update(ctx, func(tx *sql.Tx) error {
		where, args := under("uri", uri)
		if strings.HasSuffix(uri, "/") && !recursive {
			var children bool
			err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paths WHERE `+where+` AND uri <> ?)`,
				append(args, uri)...).Scan(&children)
			if err != nil {
				return err
			}
			if children {
				return indexError("delete", http.StatusConflict, errors.New("the folder is not empty"))
			}
		}
		if err := deleteChunks(ctx, tx, where, args...); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM paths WHERE `+where, args...)
		return err
	})
}

// Snapshot confirms that the paths are stored. The index writes in
// transactions, so there is nothing to commit; the ID only marks the
// checkpoint.
func (d *Documents) Snapshot(ctx context.Context, _ string, paths []string) (string, error) {
	if err := d.e.Ping(ctx); err != nil {
		return "", err
	}
	return "local:" + xid.New().String(), nil
}

// Search returns the documents whose chunks are nearest to query, one hit
// per document. scope, when set, is a locator prefix: the search then
// compares the query with every chunk under it, so documents outside the
// scope cannot crowd them out.
func (d *Documents) Search(ctx context.Context, query, scope string, limit int) ([]model.SearchHit, error) {
	if limit <= 0 {
		return nil, nil
	}
	vector, err := d.e.queries.vector(ctx, query, d.e.model.Embed)
	if err != nil {
		return nil, providerError("embed query", err)
	}
	var rows *sql.Rows
	if scope == "" {
		rows, err = d.e.db.QueryContext(ctx, `
			SELECT c.uri, c.text, v.distance
			FROM (SELECT rid, distance FROM chunk_vec WHERE emb MATCH vec_int8(?) AND k = ?) v
			JOIN chunks c ON c.rid = v.rid
			ORDER BY v.distance`, vector, min(limit*4+20, 1000))
	} else {
		rows, err = d.e.db.QueryContext(ctx, `
			SELECT uri, text, min(distance) AS distance FROM (
				SELECT c.uri, c.text, vec_distance_cosine(v.emb, vec_int8(?1)) AS distance
				FROM chunks c JOIN chunk_vec v ON v.rid = c.rid
				WHERE c.uri >= ?2 AND substr(c.uri, 1, length(?2)) = ?2)
			GROUP BY uri ORDER BY distance LIMIT ?3`, vector, scope, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var hits []model.SearchHit
	for rows.Next() {
		var uri, text string
		var distance float64
		if err := rows.Scan(&uri, &text, &distance); err != nil {
			return nil, err
		}
		if seen[uri] || (scope != "" && !strings.HasPrefix(uri, scope)) {
			continue
		}
		seen[uri] = true
		hits = append(hits, model.SearchHit{
			Kind: "document", ID: uri, Title: uri, Snippet: truncateRunes(text, 300),
			Score: 1 - distance, Source: "semantic",
		})
		if len(hits) == limit {
			break
		}
	}
	return hits, rows.Err()
}
