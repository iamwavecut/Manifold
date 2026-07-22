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
	"github.com/iamwavecut/Manifold/internal/service"
	"github.com/iamwavecut/Manifold/internal/store"
	"github.com/iamwavecut/Manifold/internal/upstream"
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
			return fmt.Errorf("unknown command %q; supported commands: openapi, version", os.Args[1])
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

	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	documents := upstream.NewOpenViking(cfg.OpenVikingURL, cfg.OpenVikingKey, httpClient)
	graph := upstream.NewBrain(cfg.BrainURL, cfg.BrainKey, httpClient)
	svc := service.New(db, documents, graph, cfg.PublicURL, logger, cfg.WorkerInterval, buildinfo.New(version, commit))
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

	var workers sync.WaitGroup
	workers.Go(func() {
		svc.RunWorker(ctx)
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

	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	svc := service.New(
		db,
		upstream.NewOpenViking(cfg.OpenVikingURL, cfg.OpenVikingKey, httpClient),
		upstream.NewBrain(cfg.BrainURL, cfg.BrainKey, httpClient),
		cfg.PublicURL,
		logger,
		cfg.WorkerInterval,
		buildinfo.New(version, commit),
	)
	api := manifoldapi.New(svc, auth.NewManager(db), cfg, logger, nil)
	data, err := api.Spec.YAML()
	if err != nil {
		return fmt.Errorf("render OpenAPI: %w", err)
	}
	_, err = os.Stdout.Write(data)
	return err
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
