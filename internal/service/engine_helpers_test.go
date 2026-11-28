package service

import (
	"context"
	"hash/fnv"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/iamwavecut/Manifold/internal/engine"
	"github.com/iamwavecut/Manifold/internal/store"
	"github.com/iamwavecut/Manifold/internal/upstream"
)

// testModel answers extraction from fixtures keyed by a phrase of the text
// and embeds text as a hashed bag of words.
type testModel struct {
	mu          sync.Mutex
	extractions map[string]engine.Extraction
	chatErr     error
	failChats   int
	chatCalls   atomic.Int32
}

func (m *testModel) JSON(_ context.Context, _ string, _ map[string]any, _, user string, out any) error {
	calls := m.chatCalls.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.chatErr != nil && int(calls) <= m.failChats {
		return m.chatErr
	}
	result := out.(*engine.Extraction)
	*result = engine.Extraction{}
	for phrase, extraction := range m.extractions {
		if strings.Contains(user, phrase) {
			*result = extraction
		}
	}
	return nil
}

func (m *testModel) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	vectors := make([][]float32, len(inputs))
	for i, input := range inputs {
		vector := make([]float32, 32)
		for _, word := range strings.FieldsFunc(strings.ToLower(input), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}) {
			h := fnv.New32a()
			h.Write([]byte(word))
			vector[h.Sum32()%32]++
		}
		var norm float64
		for _, value := range vector {
			norm += float64(value * value)
		}
		if norm == 0 {
			vector[0], norm = 1, 1
		}
		for j := range vector {
			vector[j] = float32(float64(vector[j]) / math.Sqrt(norm))
		}
		vectors[i] = vector
	}
	return vectors, nil
}

func newTestEngine(t *testing.T, model *testModel) *engine.Engine {
	t.Helper()
	index, err := engine.Open(t.Context(), engine.Options{
		Path: filepath.Join(t.TempDir(), "index.db"), EmbeddingModel: "test", Dimensions: 32, Model: model,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { index.Close() })
	return index
}

// recordingIndex records document index calls and can fail snapshots.
type recordingIndex struct {
	upstream.DocumentStore
	mu               sync.Mutex
	moves            []string
	writeModes       []string
	snapshots        int
	snapshotFailures int
}

func (r *recordingIndex) Write(ctx context.Context, uri, content string, create bool) error {
	r.mu.Lock()
	mode := "replace"
	if create {
		mode = "create"
	}
	r.writeModes = append(r.writeModes, mode)
	r.mu.Unlock()
	return r.DocumentStore.Write(ctx, uri, content, create)
}

func (r *recordingIndex) Move(ctx context.Context, from, to string) error {
	r.mu.Lock()
	r.moves = append(r.moves, from+" -> "+to)
	r.mu.Unlock()
	return r.DocumentStore.Move(ctx, from, to)
}

func (r *recordingIndex) Snapshot(ctx context.Context, message string, paths []string) (string, error) {
	r.mu.Lock()
	r.snapshots++
	fail := r.snapshotFailures > 0
	if fail {
		r.snapshotFailures--
	}
	r.mu.Unlock()
	if fail {
		return "", &upstream.DependencyError{Dependency: "index", Operation: "snapshot", Status: 503, Retryable: true}
	}
	return r.DocumentStore.Snapshot(ctx, message, paths)
}

func (r *recordingIndex) recorded() (moves, writes []string, snapshots int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.moves...), append([]string(nil), r.writeModes...), r.snapshots
}

func newEngineService(t *testing.T, db *store.Store, index *engine.Engine) (*Service, *recordingIndex) {
	t.Helper()
	documents := &recordingIndex{DocumentStore: index.Documents()}
	svc := New(db, documents, index.Graph(), "https://memory.example.test",
		slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	return svc, documents
}
