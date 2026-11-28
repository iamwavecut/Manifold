package engine

import (
	"context"
	"database/sql"
	"time"
)

const embedBatchSize = 64

type pendingVector struct {
	table string // "chunk" or "fact"
	rid   int64
	text  string
}

// EmbedPending embeds every chunk and live fact without a vector and
// returns how many it embedded.
func (e *Engine) EmbedPending(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := e.embedBatch(ctx)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
}

func (e *Engine) embedBatch(ctx context.Context) (int, error) {
	pending, err := e.pendingVectors(ctx)
	if err != nil || len(pending) == 0 {
		return 0, err
	}
	texts := make([]string, len(pending))
	for i, item := range pending {
		texts[i] = item.text
	}
	vectors, err := e.model.Embed(ctx, texts)
	if err != nil {
		return 0, providerError("embed index", err)
	}
	err = e.update(ctx, func(tx *sql.Tx) error {
		for i, item := range pending {
			// The row can have changed since it was read: a rewritten
			// document drops its chunks, a retracted fact needs no vector.
			mark := `UPDATE chunks SET embedded = 1 WHERE rid = ? AND embedded = 0`
			if item.table == "fact" {
				mark = `UPDATE facts SET embedded = 1 WHERE rid = ? AND embedded = 0 AND status <> 'retracted'`
			}
			result, err := tx.ExecContext(ctx, mark, item.rid)
			if err != nil {
				return err
			}
			if n, _ := result.RowsAffected(); n == 0 {
				continue
			}
			table := item.table + "_vec"
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE rid = ?`, item.rid); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+`(rid, emb) VALUES (?, vec_int8(?))`,
				item.rid, quantize(vectors[i])); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(pending), nil
}

func (e *Engine) pendingVectors(ctx context.Context) ([]pendingVector, error) {
	var pending []pendingVector
	rows, err := e.db.QueryContext(ctx, `SELECT rid, text FROM chunks WHERE embedded = 0 ORDER BY rid LIMIT ?`, embedBatchSize)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		item := pendingVector{table: "chunk"}
		if err := rows.Scan(&item.rid, &item.text); err != nil {
			rows.Close()
			return nil, err
		}
		pending = append(pending, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(pending) == embedBatchSize {
		return pending, nil
	}
	rows, err = e.db.QueryContext(ctx, `
		SELECT f.rid, e.name, f.predicate, f.object FROM facts f JOIN entities e ON e.rid = f.entity_rid
		WHERE f.embedded = 0 AND f.status <> 'retracted' ORDER BY f.rid LIMIT ?`, embedBatchSize-len(pending))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, predicate, object string
		item := pendingVector{table: "fact"}
		if err := rows.Scan(&item.rid, &name, &predicate, &object); err != nil {
			return nil, err
		}
		item.text = name + " " + predicateWords(predicate) + " " + object
		pending = append(pending, item)
	}
	return pending, rows.Err()
}

// RunEmbedder embeds new rows until ctx ends. Writes wake it at once. A
// provider failure backs it off up to half an hour, and the next call the
// provider answers, such as an extraction, ends the backoff.
func (e *Engine) RunEmbedder(ctx context.Context) {
	const minBackoff, maxBackoff = time.Minute, 30 * time.Minute
	backoff := minBackoff
	for {
		n, err := e.EmbedPending(ctx)
		if ctx.Err() != nil {
			return
		}
		// Answers during the pass, the embedder's own included, say nothing
		// about the provider after it; only a later answer ends the wait.
		select {
		case <-e.answered:
		default:
		}
		if n > 0 {
			e.logger.Info("index embedded", "rows", n)
		}
		wait, wake := 10*time.Minute, e.wake
		if err != nil {
			e.logger.Warn("index embedding failed; retrying later", "error", err, "retry_in", backoff.String())
			// Writes do not end the backoff: they would hammer a failing provider.
			wait, backoff, wake = backoff, min(backoff*2, maxBackoff), nil
		} else {
			backoff = minBackoff
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
		case <-e.answered:
		case <-timer.C:
		}
		timer.Stop()
	}
}
