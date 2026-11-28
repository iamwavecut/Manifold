package engine

import (
	"container/list"
	"context"
	"errors"
	"math"
	"sync"
)

// quantize maps a vector to int8 by its largest magnitude. Cosine distance
// ignores the scale, and the rounding keeps the ranking of a float32 search
// on real embeddings while it stores a quarter of the bytes.
func quantize(vector []float32) []byte {
	var peak float64
	for _, value := range vector {
		peak = math.Max(peak, math.Abs(float64(value)))
	}
	out := make([]byte, len(vector))
	if peak == 0 {
		return out
	}
	for i, value := range vector {
		out[i] = byte(int8(math.Round(float64(value) / peak * 127)))
	}
	return out
}

// queryCache keeps recent query embeddings and shares one provider call
// between concurrent searches for the same text.
type queryCache struct {
	mu       sync.Mutex
	capacity int
	order    *list.List
	entries  map[string]*list.Element
	inflight map[string]*pendingQuery
}

type cachedQuery struct {
	text   string
	vector []byte
}

type pendingQuery struct {
	done   chan struct{}
	vector []byte
	err    error
}

func newQueryCache(capacity int) *queryCache {
	return &queryCache{
		capacity: capacity, order: list.New(),
		entries: map[string]*list.Element{}, inflight: map[string]*pendingQuery{},
	}
}

// vector returns the quantized embedding of text. Failures are not cached.
// A caller that waits for another caller's request makes its own when that
// caller gave up.
func (c *queryCache) vector(ctx context.Context, text string, embed func(context.Context, []string) ([][]float32, error)) ([]byte, error) {
	for {
		c.mu.Lock()
		if element, ok := c.entries[text]; ok {
			c.order.MoveToFront(element)
			vector := element.Value.(*cachedQuery).vector
			c.mu.Unlock()
			return vector, nil
		}
		pending, ok := c.inflight[text]
		if !ok {
			return c.fetch(ctx, text, embed)
		}
		c.mu.Unlock()
		select {
		case <-pending.done:
			abandoned := errors.Is(pending.err, context.Canceled) || errors.Is(pending.err, context.DeadlineExceeded)
			if !abandoned || ctx.Err() != nil {
				return pending.vector, pending.err
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// fetch embeds text for every caller that asks meanwhile. The caller holds
// c.mu; fetch releases it.
func (c *queryCache) fetch(ctx context.Context, text string, embed func(context.Context, []string) ([][]float32, error)) ([]byte, error) {
	pending := &pendingQuery{done: make(chan struct{})}
	c.inflight[text] = pending
	c.mu.Unlock()

	vectors, err := embed(ctx, []string{text})
	if err == nil {
		pending.vector = quantize(vectors[0])
	}
	pending.err = err

	c.mu.Lock()
	delete(c.inflight, text)
	if err == nil {
		c.entries[text] = c.order.PushFront(&cachedQuery{text: text, vector: pending.vector})
		for c.order.Len() > c.capacity {
			oldest := c.order.Back()
			c.order.Remove(oldest)
			delete(c.entries, oldest.Value.(*cachedQuery).text)
		}
	}
	c.mu.Unlock()
	close(pending.done)
	return pending.vector, err
}
