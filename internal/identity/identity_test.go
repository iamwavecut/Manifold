package identity

import "testing"

func TestSlugTransliteratesAndNormalizesHumanNames(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"Память Агента":              "pamyat-agenta",
		"  API / Design_v2.md  ":     "api-design-v2-md",
		"ёж — не machine/random key": "yozh-ne-machine-random-key",
		"":                           "item",
	}
	for input, want := range tests {
		if got := Slug(input); got != want {
			t.Errorf("Slug(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestXIDIsCompactAndRecognizable(t *testing.T) {
	t.Parallel()

	first := NewXID()
	second := NewXID()
	if !IsXID(first) || !IsXID(second) {
		t.Fatalf("generated IDs are not valid XIDs: %q %q", first, second)
	}
	if first == second {
		t.Fatalf("two generated XIDs unexpectedly matched: %q", first)
	}
	if IsXID("550e8400-e29b-41d4-a716-446655440000") {
		t.Fatal("UUID must not be accepted as a Manifold XID")
	}
}

func TestSecretIsNotAnXID(t *testing.T) {
	t.Parallel()

	secret, err := Secret(32)
	if err != nil {
		t.Fatal(err)
	}
	if IsXID(secret) {
		t.Fatalf("cryptographic secret must not use the XID format: %q", secret)
	}
	if len(secret) < 40 {
		t.Fatalf("secret is unexpectedly short: %d", len(secret))
	}
}

func TestChunkIDUsesReadableDocumentRevisionAddress(t *testing.T) {
	t.Parallel()

	if got := ChunkID("agent-memory", "r3", 7); got != "agent-memory@r3#chunk-7" {
		t.Fatalf("ChunkID() = %q", got)
	}
}
