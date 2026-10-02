package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOpenVikingWriteHasIndependentDeadline(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(40 * time.Millisecond):
		}
		_, _ = w.Write([]byte(`{"status":"ok","result":{"semantic_status":"complete","vector_status":"complete"}}`))
	}))
	defer server.Close()
	client := NewOpenViking(server.URL, "test-key",
		&http.Client{Timeout: 5 * time.Millisecond},
		&http.Client{Timeout: time.Second})
	if err := client.Write(t.Context(), "viking://resources/manifold/note.md", "verified", true); err != nil {
		t.Fatalf("durable write inherited interactive deadline: %v", err)
	}
	var dependency *DependencyError
	if err := client.Health(t.Context()); !errors.As(err, &dependency) || !dependency.Retryable {
		t.Fatalf("interactive health must retain its short deadline: %v", err)
	}
}

func TestOpenVikingWriteHonorsCallerCancellation(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	client := NewOpenViking(server.URL, "test-key", server.Client(), &http.Client{Timeout: time.Second})
	done := make(chan error, 1)
	go func() { done <- client.Write(ctx, "viking://resources/manifold/note.md", "verified", true) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write = %v", err)
	}
}

func TestOpenVikingWriteRejectsIncompleteIndexing(t *testing.T) {
	for _, state := range []string{"failed", "queued", "deferred", "unknown", ""} {
		for _, field := range []string{"semantic_status", "vector_status"} {
			t.Run(field+"/"+state, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					result := map[string]string{"semantic_status": "complete", "vector_status": "complete"}
					result[field] = state
					_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "result": result})
				}))
				defer server.Close()
				err := NewOpenViking(server.URL, "test-key", server.Client()).Write(t.Context(), "test-resource", "verified", true)
				var dependency *DependencyError
				if !errors.As(err, &dependency) {
					t.Fatalf("HTTP 200 with %s=%s was accepted as indexed: %v", field, state, err)
				}
			})
		}
	}
}

func TestOpenVikingSnapshotRequiresDurableCheckpoint(t *testing.T) {
	for _, body := range []string{
		`{"status":"ok","result":{}}`,
		`{"status":"error","result":{"commit_oid":"uncommitted"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			checkpoint, err := NewOpenViking(server.URL, "test-key", server.Client()).Snapshot(t.Context(), "revision", []string{"test-resource"})
			if err == nil || checkpoint != "" {
				t.Fatalf("invalid snapshot accepted: checkpoint=%q error=%v", checkpoint, err)
			}
		})
	}
}
