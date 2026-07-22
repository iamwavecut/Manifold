package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadBrainIngestTimeout(t *testing.T) {
	t.Setenv("MANIFOLD_MAX_BODY_BYTES", "")
	t.Setenv("MANIFOLD_HTTP_TIMEOUT", "")
	t.Setenv("MANIFOLD_WORKER_INTERVAL", "")

	t.Run("default", func(t *testing.T) {
		t.Setenv("MANIFOLD_BRAIN_INGEST_TIMEOUT", "")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.BrainIngestTimeout != 2*time.Minute {
			t.Fatalf("BrainIngestTimeout = %s, want 2m", cfg.BrainIngestTimeout)
		}
	})

	t.Run("configured", func(t *testing.T) {
		t.Setenv("MANIFOLD_BRAIN_INGEST_TIMEOUT", "3m30s")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.BrainIngestTimeout != 3*time.Minute+30*time.Second {
			t.Fatalf("BrainIngestTimeout = %s, want 3m30s", cfg.BrainIngestTimeout)
		}
	})

	t.Run("non-positive", func(t *testing.T) {
		t.Setenv("MANIFOLD_BRAIN_INGEST_TIMEOUT", "0s")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "MANIFOLD_BRAIN_INGEST_TIMEOUT") {
			t.Fatalf("Load error = %v", err)
		}
	})
}
