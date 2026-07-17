package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestProblemRenderingUsesCodeAndRemediation(t *testing.T) {
	var p problem
	p.Code = "slug_taken"
	p.Title = "Slug already in use"
	p.RequestID = "d5vqh9idc6b5u5sctq00"
	p.SuggestedSlug = "agent-memory-2"
	p.Remediation.Summary = "Choose an unused slug."
	p.Remediation.Steps = []string{"Review suggested_slug."}
	var output bytes.Buffer
	renderProblem(&output, p)
	for _, expected := range []string{
		"ERROR [slug_taken]", "How to fix: Choose an unused slug.",
		"Suggested slug: agent-memory-2", "Request ID: d5vqh9idc6b5u5sctq00",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("output lacks %q: %s", expected, output.String())
		}
	}
	if exitCode(p.Code) != 14 {
		t.Fatalf("slug_taken exit code = %d", exitCode(p.Code))
	}
	if exitCode("idempotency_key_reused") != 15 {
		t.Fatalf("idempotency_key_reused exit code = %d", exitCode("idempotency_key_reused"))
	}
}

func TestIdempotencyKeyIsNotAnXID(t *testing.T) {
	key := newIdempotencyKey()
	if !strings.HasPrefix(key, "skill-") || len(key) < 25 {
		t.Fatalf("unexpected idempotency key %q", key)
	}
}
