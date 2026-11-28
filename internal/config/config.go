package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr            string
	DBPath          string
	IndexPath       string
	PublicURL       string
	BootstrapAPIKey string
	LogLevel        string
	MaxBodyBytes    int64

	// The OpenAI-compatible model provider for extraction and embeddings.
	ProviderURL           string
	ProviderKey           string
	ChatModel             string
	ChatExtraBody         map[string]any
	EmbeddingModel        string
	EmbeddingDimensions   int
	ProviderTimeout       time.Duration
	ExtractionTimeout     time.Duration
	ExtractionConcurrency int

	WorkerInterval        time.Duration
	WorkerConcurrency     int
	ProviderRetryInterval time.Duration
}

func Load() (Config, error) {
	maxBody, err := envInt64("MANIFOLD_MAX_BODY_BYTES", 5<<20)
	if err != nil {
		return Config{}, err
	}
	providerTimeout, err := positiveDuration("MANIFOLD_PROVIDER_TIMEOUT", 3*time.Minute)
	if err != nil {
		return Config{}, err
	}
	extractionTimeout, err := positiveDuration("MANIFOLD_EXTRACTION_TIMEOUT", 30*time.Minute)
	if err != nil {
		return Config{}, err
	}
	dimensions, err := boundedInt("MANIFOLD_EMBEDDING_DIMENSIONS", 512, 1, 4096)
	if err != nil {
		return Config{}, err
	}
	extractionConcurrency, err := boundedInt("MANIFOLD_EXTRACTION_CONCURRENCY", 4, 1, 16)
	if err != nil {
		return Config{}, err
	}
	var extraBody map[string]any
	if raw := strings.TrimSpace(os.Getenv("OPENAI_CHAT_EXTRA_BODY")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &extraBody); err != nil || extraBody == nil {
			return Config{}, fmt.Errorf("OPENAI_CHAT_EXTRA_BODY must be a JSON object")
		}
	}
	workerInterval, err := envDuration("MANIFOLD_WORKER_INTERVAL", time.Second)
	if err != nil {
		return Config{}, err
	}
	workerConcurrency, err := boundedInt("MANIFOLD_WORKER_CONCURRENCY", 2, 1, 16)
	if err != nil {
		return Config{}, err
	}
	providerRetry, err := envDuration("MANIFOLD_PROVIDER_RETRY_INTERVAL", 15*time.Minute)
	if err != nil {
		return Config{}, err
	}
	if providerRetry < 0 {
		return Config{}, fmt.Errorf("MANIFOLD_PROVIDER_RETRY_INTERVAL must be zero (off) or greater")
	}
	dbPath := env("MANIFOLD_DB_PATH", "manifold.db")
	cfg := Config{
		Addr:                  env("MANIFOLD_ADDR", ":8080"),
		DBPath:                dbPath,
		IndexPath:             env("MANIFOLD_INDEX_DB_PATH", defaultIndexPath(dbPath)),
		PublicURL:             strings.TrimRight(env("MANIFOLD_PUBLIC_URL", "http://localhost:8080"), "/"),
		BootstrapAPIKey:       os.Getenv("MANIFOLD_BOOTSTRAP_API_KEY"),
		LogLevel:              env("MANIFOLD_LOG_LEVEL", "info"),
		MaxBodyBytes:          maxBody,
		ProviderURL:           strings.TrimRight(env("OPENAI_BASE_URL", "https://api.openai.com/v1"), "/"),
		ProviderKey:           strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		ChatModel:             env("OPENAI_CHAT_MODEL", "gpt-4.1-mini"),
		ChatExtraBody:         extraBody,
		EmbeddingModel:        env("OPENAI_EMBEDDING_MODEL", "text-embedding-3-small"),
		EmbeddingDimensions:   dimensions,
		ProviderTimeout:       providerTimeout,
		ExtractionTimeout:     extractionTimeout,
		ExtractionConcurrency: extractionConcurrency,
		WorkerInterval:        workerInterval,
		WorkerConcurrency:     workerConcurrency,
		ProviderRetryInterval: providerRetry,
	}
	if cfg.MaxBodyBytes < 1024 {
		return Config{}, fmt.Errorf("MANIFOLD_MAX_BODY_BYTES must be at least 1024")
	}
	return cfg, nil
}

// defaultIndexPath keeps the derived index beside the canonical database.
func defaultIndexPath(dbPath string) string {
	path, _, _ := strings.Cut(strings.TrimPrefix(dbPath, "file:"), "?")
	if path == "" || path == ":memory:" {
		return ":memory:"
	}
	return filepath.Join(filepath.Dir(path), "index.db")
}

func positiveDuration(name string, fallback time.Duration) (time.Duration, error) {
	value, err := envDuration(name, fallback)
	if err != nil {
		return 0, err
	}
	if value <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", name)
	}
	return value, nil
}

func boundedInt(name string, fallback, low, high int64) (int, error) {
	value, err := envInt64(name, fallback)
	if err != nil {
		return 0, err
	}
	if value < low || value > high {
		return 0, fmt.Errorf("%s must be between %d and %d", name, low, high)
	}
	return int(value), nil
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt64(name string, fallback int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return parsed, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 30s: %w", name, err)
	}
	return parsed, nil
}
