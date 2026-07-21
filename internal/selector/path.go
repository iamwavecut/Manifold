package selector

import (
	"fmt"
	pathpkg "path"
	"strings"
)

// NormalizePath accepts the public slash-separated path form and removes
// leading and trailing separators. Empty segments are never meaningful.
func NormalizePath(value string) string {
	return strings.Trim(strings.TrimSpace(value), "/")
}

// ValidateGlob accepts exact semantic path segments, * and ? within one
// segment, and ** as a complete recursive segment.
func ValidateGlob(pattern string) error {
	pattern = NormalizePath(pattern)
	if pattern == "" {
		return nil
	}
	for _, segment := range strings.Split(pattern, "/") {
		if segment == "**" {
			continue
		}
		if segment == "" || strings.ContainsAny(segment, "[]\\") {
			return fmt.Errorf("unsupported glob segment %q", segment)
		}
		for _, r := range segment {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '*' || r == '?' {
				continue
			}
			return fmt.Errorf("unsupported character %q in glob segment %q", r, segment)
		}
		if _, err := pathpkg.Match(segment, "item"); err != nil {
			return fmt.Errorf("invalid glob segment %q: %w", segment, err)
		}
	}
	return nil
}

func Match(pattern, value string) bool {
	pattern = NormalizePath(pattern)
	value = NormalizePath(value)
	if pattern == "" {
		pattern = "**"
	}
	return matchSegments(strings.Split(pattern, "/"), splitPath(value))
}

func splitPath(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, "/")
}

func matchSegments(pattern, value []string) bool {
	if len(pattern) == 0 {
		return len(value) == 0
	}
	if pattern[0] == "**" {
		return matchSegments(pattern[1:], value) ||
			(len(value) > 0 && matchSegments(pattern, value[1:]))
	}
	if len(value) == 0 {
		return false
	}
	matched, err := pathpkg.Match(pattern[0], value[0])
	return err == nil && matched && matchSegments(pattern[1:], value[1:])
}
