package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr            string
	DBPath          string
	PublicURL       string
	BootstrapAPIKey string
	LogLevel        string
	MaxBodyBytes    int64
	OpenVikingURL   string
	OpenVikingKey   string
	BrainURL        string
	BrainKey        string
	HTTPTimeout     time.Duration
	WorkerInterval  time.Duration
}

func Load() (Config, error) {
	maxBody, err := envInt64("MANIFOLD_MAX_BODY_BYTES", 5<<20)
	if err != nil {
		return Config{}, err
	}
	httpTimeout, err := envDuration("MANIFOLD_HTTP_TIMEOUT", 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	workerInterval, err := envDuration("MANIFOLD_WORKER_INTERVAL", time.Second)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Addr:            env("MANIFOLD_ADDR", ":8080"),
		DBPath:          env("MANIFOLD_DB_PATH", "manifold.db"),
		PublicURL:       strings.TrimRight(env("MANIFOLD_PUBLIC_URL", "http://localhost:8080"), "/"),
		BootstrapAPIKey: os.Getenv("MANIFOLD_BOOTSTRAP_API_KEY"),
		LogLevel:        env("MANIFOLD_LOG_LEVEL", "info"),
		MaxBodyBytes:    maxBody,
		OpenVikingURL:   strings.TrimRight(env("OPENVIKING_URL", ""), "/"),
		OpenVikingKey:   os.Getenv("OPENVIKING_API_KEY"),
		BrainURL:        strings.TrimRight(env("BRAIN_URL", ""), "/"),
		BrainKey:        os.Getenv("BRAIN_API_KEY"),
		HTTPTimeout:     httpTimeout,
		WorkerInterval:  workerInterval,
	}
	if cfg.MaxBodyBytes < 1024 {
		return Config{}, fmt.Errorf("MANIFOLD_MAX_BODY_BYTES must be at least 1024")
	}
	return cfg, nil
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
