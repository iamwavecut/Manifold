package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	manifoldapi "github.com/iamwavecut/Manifold/internal/api"
	"github.com/iamwavecut/Manifold/internal/auth"
	"github.com/iamwavecut/Manifold/internal/buildinfo"
	"github.com/iamwavecut/Manifold/internal/config"
	"github.com/iamwavecut/Manifold/internal/engine"
	"github.com/iamwavecut/Manifold/internal/llm"
	"github.com/iamwavecut/Manifold/internal/service"
	"github.com/iamwavecut/Manifold/internal/store"
	"github.com/iamwavecut/Manifold/web"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	if err := run(); err != nil {
		slog.Error("manifold stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// The probe runs every few seconds; it must not open the database or
	// depend on the rest of the configuration.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		addr := os.Getenv("MANIFOLD_ADDR")
		if addr == "" {
			addr = ":8080"
		}
		return healthcheck(context.Background(), addr)
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	logger := newLogger(cfg.LogLevel)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "openapi":
			return writeOpenAPI(cfg, logger)
		case "version":
			fmt.Printf("%s (%s) protocol=%d\n", version, commit, buildinfo.ProtocolRevision)
			return nil
		default:
			return fmt.Errorf("unknown command %q; supported commands: openapi, version, healthcheck", os.Args[1])
		}
	}
	return serve(cfg, logger)
}

func serve(cfg config.Config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	authManager := auth.NewManager(db)
	if err := authManager.Bootstrap(ctx, cfg.BootstrapAPIKey); err != nil {
		return fmt.Errorf("bootstrap API key: %w", err)
	}

	client := newModelClient(cfg)
	index, err := openIndex(ctx, cfg, cfg.IndexPath, client, logger)
	if err != nil {
		return err
	}
	defer index.Close()
	if err := seedIndex(ctx, index, db, logger); err != nil {
		return err
	}
	if !client.Configured() {
		logger.Warn("model provider is not configured; extraction and semantic search stay unavailable")
	}
	svc := newService(cfg, db, index, client, logger)
	handler := manifoldapi.New(svc, authManager, cfg, logger, web.Handler()).Handler

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	// Workers claim different jobs; the store runs one resource's jobs in
	// order and a rename alone.
	var workers sync.WaitGroup
	for range cfg.WorkerConcurrency {
		workers.Go(func() {
			svc.RunWorker(ctx)
		})
	}
	workers.Go(func() {
		svc.RunProviderRecovery(ctx, cfg.ProviderRetryInterval)
	})
	workers.Go(func() {
		index.RunEmbedder(ctx)
	})
	serverError := make(chan error, 1)
	go func() {
		logger.Info("manifold listening", "address", cfg.Addr, "public_url", cfg.PublicURL, "version", version)
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serverError <- err
	}()

	select {
	case err := <-serverError:
		if err != nil {
			return fmt.Errorf("serve HTTP: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown HTTP server: %w", err)
	}
	workers.Wait()
	return <-serverError
}

func writeOpenAPI(cfg config.Config, logger *slog.Logger) error {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		return err
	}
	defer db.Close()

	client := newModelClient(cfg)
	index, err := openIndex(ctx, cfg, ":memory:", client, logger)
	if err != nil {
		return err
	}
	defer index.Close()
	api := manifoldapi.New(newService(cfg, db, index, client, logger), auth.NewManager(db), cfg, logger, nil)
	data, err := api.Spec.YAML()
	if err != nil {
		return fmt.Errorf("render OpenAPI: %w", err)
	}
	_, err = os.Stdout.Write(data)
	return err
}

func newModelClient(cfg config.Config) *llm.Client {
	return llm.New(llm.Config{
		BaseURL:        cfg.ProviderURL,
		APIKey:         cfg.ProviderKey,
		ChatModel:      cfg.ChatModel,
		EmbeddingModel: cfg.EmbeddingModel,
		Dimensions:     cfg.EmbeddingDimensions,
		ExtraBody:      cfg.ChatExtraBody,
		HTTPClient:     &http.Client{Timeout: cfg.ProviderTimeout},
	})
}

func openIndex(ctx context.Context, cfg config.Config, path string, client *llm.Client, logger *slog.Logger) (*engine.Engine, error) {
	index, err := engine.Open(ctx, engine.Options{
		Path:                  path,
		EmbeddingModel:        cfg.EmbeddingModel,
		Dimensions:            cfg.EmbeddingDimensions,
		Model:                 client,
		Logger:                logger,
		ExtractionConcurrency: cfg.ExtractionConcurrency,
	})
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	return index, nil
}

// seedIndex fills a new index from the canonical database once. A restart
// after a failed seed tries again, because the seed marks the index only
// when its transaction commits.
func seedIndex(ctx context.Context, index *engine.Engine, db *store.Store, logger *slog.Logger) error {
	seeded, err := index.Seeded(ctx)
	if err != nil {
		return fmt.Errorf("read index state: %w", err)
	}
	if seeded {
		return nil
	}
	started := time.Now()
	if err := index.Seed(ctx, db); err != nil {
		return fmt.Errorf("seed index: %w", err)
	}
	logger.Info("index seeded from the canonical database", "duration", time.Since(started).Round(time.Millisecond).String())
	return nil
}

func newService(cfg config.Config, db *store.Store, index *engine.Engine, client *llm.Client, logger *slog.Logger) *service.Service {
	svc := service.New(db, index.Documents(), index.Graph(), cfg.PublicURL, logger, cfg.WorkerInterval, buildinfo.New(version, commit))
	svc.SetExtractionTimeout(cfg.ExtractionTimeout)
	svc.SetProviderHealth(client.Health)
	return svc
}

func newLogger(levelName string) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(levelName) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
