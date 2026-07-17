package api

import (
	"strconv"
	"strings"
)

type body[T any] struct {
	Body T
}

type accepted[T any] struct {
	Location string `header:"Location"`
	Body     T
}

type withETag[T any] struct {
	ETag string `header:"ETag"`
	Body T
}

type page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func paginate[T any](items []T, cursor string, limit int) page[T] {
	offset := 0
	if cursor != "" {
		parsed, err := strconv.ParseInt(strings.TrimPrefix(cursor, "c"), 36, 32)
		if err == nil && parsed > 0 {
			offset = int(parsed)
		}
	}
	if offset > len(items) {
		offset = len(items)
	}
	if limit <= 0 {
		limit = 50
	}
	end := min(offset+limit, len(items))
	result := page[T]{Items: items[offset:end]}
	if end < len(items) {
		result.NextCursor = "c" + strconv.FormatInt(int64(end), 36)
	}
	return result
}

type MutationHeaders struct {
	IdempotencyKey string `header:"Idempotency-Key" minLength:"8" maxLength:"128" doc:"A caller-generated key that makes this mutation safe to retry."`
}
