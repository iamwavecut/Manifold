package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaultsForTheEmbeddedEngine(t *testing.T) {
	for _, name := range []string{
		"MANIFOLD_DB_PATH", "MANIFOLD_INDEX_DB_PATH", "OPENAI_BASE_URL", "OPENAI_CHAT_EXTRA_BODY",
		"OPENAI_EMBEDDING_MODEL", "MANIFOLD_EMBEDDING_DIMENSIONS", "MANIFOLD_EXTRACTION_CONCURRENCY",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("MANIFOLD_DB_PATH", "/var/lib/manifold/manifold.db")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IndexPath != "/var/lib/manifold/index.db" {
		t.Fatalf("IndexPath = %q, want index.db beside the database", cfg.IndexPath)
	}
	if cfg.ProviderURL != "https://api.openai.com/v1" || cfg.EmbeddingModel != "text-embedding-3-small" ||
		cfg.EmbeddingDimensions != 512 || cfg.ExtractionConcurrency != 4 || cfg.ChatExtraBody != nil {
		t.Fatalf("provider defaults = %+v", cfg)
	}
	if cfg.ExtractionTimeout != 30*time.Minute || cfg.ProviderTimeout != 3*time.Minute {
		t.Fatalf("timeouts = %s, %s", cfg.ExtractionTimeout, cfg.ProviderTimeout)
	}

	t.Setenv("MANIFOLD_DB_PATH", ":memory:")
	if cfg, err := Load(); err != nil || cfg.IndexPath != ":memory:" {
		t.Fatalf("in-memory IndexPath = %q, %v", cfg.IndexPath, err)
	}
}

func TestLoadChatExtraBody(t *testing.T) {
	t.Setenv("OPENAI_CHAT_EXTRA_BODY", `{"reasoning":{"enabled":false}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if reasoning, ok := cfg.ChatExtraBody["reasoning"].(map[string]any); !ok || reasoning["enabled"] != false {
		t.Fatalf("ChatExtraBody = %#v", cfg.ChatExtraBody)
	}
	for _, value := range []string{`[1]`, `{"broken"`} {
		t.Setenv("OPENAI_CHAT_EXTRA_BODY", value)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "OPENAI_CHAT_EXTRA_BODY") {
			t.Fatalf("extra body %s accepted: %v", value, err)
		}
	}
}

func TestLoadRejectsOutOfRangeSettings(t *testing.T) {
	for name, values := range map[string][]string{
		"MANIFOLD_EMBEDDING_DIMENSIONS":   {"0", "4097", "many"},
		"MANIFOLD_EXTRACTION_CONCURRENCY": {"0", "17"},
		"MANIFOLD_EXTRACTION_TIMEOUT":     {"0s", "-1s", "forever"},
		"MANIFOLD_PROVIDER_TIMEOUT":       {"0s"},
	} {
		for _, value := range values {
			t.Run(name+"="+value, func(t *testing.T) {
				t.Setenv(name, value)
				if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("%s=%s accepted: %v", name, value, err)
				}
			})
		}
	}
}
