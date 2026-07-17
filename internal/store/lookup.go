package store

import (
	"context"
	"fmt"
	"sort"
)

func (s *Store) SimilarSlugs(ctx context.Context, resourceType, requested string, limit int) ([]string, error) {
	table, err := slugTable(resourceType)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT id FROM %s`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type candidate struct {
		id       string
		distance int
	}
	var candidates []candidate
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate{id: id, distance: editDistance(requested, id)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].distance == candidates[j].distance {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].distance < candidates[j].distance
	})
	if limit <= 0 {
		limit = 3
	}
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	result := make([]string, len(candidates))
	for index := range candidates {
		result[index] = candidates[index].id
	}
	return result, nil
}

func (s *Store) RenameDestination(ctx context.Context, resourceType, oldSlug string) (string, bool, error) {
	available, err := s.SlugAvailable(ctx, resourceType, oldSlug)
	if err != nil {
		return "", false, err
	}
	if !available {
		return "", false, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT new_slug
		FROM rename_audit
		WHERE resource_type = ? AND old_slug = ?
		ORDER BY new_slug
		LIMIT 2`, resourceType, oldSlug)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var destinations []string
	for rows.Next() {
		var destination string
		if err := rows.Scan(&destination); err != nil {
			return "", false, err
		}
		destinations = append(destinations, destination)
	}
	return first(destinations), len(destinations) == 1, rows.Err()
}

func editDistance(left, right string) int {
	a, b := []rune(left), []rune(right)
	previous := make([]int, len(b)+1)
	for index := range previous {
		previous[index] = index
	}
	for i, leftRune := range a {
		current := make([]int, len(b)+1)
		current[0] = i + 1
		for j, rightRune := range b {
			cost := 1
			if leftRune == rightRune {
				cost = 0
			}
			current[j+1] = min(
				current[j]+1,
				previous[j+1]+1,
				previous[j]+cost,
			)
		}
		previous = current
	}
	return previous[len(b)]
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
