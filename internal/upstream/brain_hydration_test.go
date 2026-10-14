package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hydrationFixture serves a committed graph of n entities that each own one
// fact, linked in a ring by n relations. Every response takes delay so tests
// can observe how many reads are in flight at once.
type hydrationFixture struct {
	t        *testing.T
	delay    time.Duration
	failPath string

	mu       sync.Mutex
	requests map[string]int
	inFlight atomic.Int32
	peak     atomic.Int32
}

func (f *hydrationFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	current := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		peak := f.peak.Load()
		if current <= peak || f.peak.CompareAndSwap(peak, current) {
			break
		}
	}
	f.mu.Lock()
	f.requests[r.URL.Path]++
	f.mu.Unlock()
	time.Sleep(f.delay)
	if r.URL.Path == f.failPath {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	var index int
	switch {
	case scan(r.URL.Path, "/v1/entities/knowledge_entity:e%d/connections", &index):
		next := (index + 1) % hydrationEntities
		_ = json.NewEncoder(w).Encode(map[string]any{"edges": []any{
			map[string]any{
				"edgeId": fmt.Sprintf("knowledge_edge:r%d", index),
				"from":   fmt.Sprintf("knowledge_entity:e%d", index),
				"to":     fmt.Sprintf("knowledge_entity:e%d", next),
				"kind":   "links_to", "weight": 0.9,
			},
			map[string]any{
				"edgeId": fmt.Sprintf("knowledge_edge:r%d", (index+hydrationEntities-1)%hydrationEntities),
				"from":   fmt.Sprintf("knowledge_entity:e%d", (index+hydrationEntities-1)%hydrationEntities),
				"to":     fmt.Sprintf("knowledge_entity:e%d", index),
				"kind":   "links_to", "weight": 0.9,
			},
		}})
	case scan(r.URL.Path, "/v1/entities/knowledge_entity:e%d", &index):
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entityId": fmt.Sprintf("knowledge_entity:e%d", index), "type": "project",
			"canonicalName": fmt.Sprintf("Entity %d", index),
			"facts":         []any{map[string]any{"factId": fmt.Sprintf("knowledge_fact:f%d", index), "status": "active"}},
		})
	case scan(r.URL.Path, "/v1/facts/knowledge_fact:f%d", &index):
		_ = json.NewEncoder(w).Encode(map[string]any{
			"factId": fmt.Sprintf("knowledge_fact:f%d", index), "aspect": "status", "statement": "ready",
			"confidence": 0.9, "validFrom": "2026-10-14T10:00:00.000Z",
		})
	default:
		http.NotFound(w, r)
	}
}

func scan(path, format string, index *int) bool {
	n, err := fmt.Sscanf(path, format, index)
	if err != nil || n != 1 {
		return false
	}
	return path == fmt.Sprintf(format, *index)
}

const hydrationEntities = 24

// hydrationCandidates repeats the first entity, fact and relation so that
// Brain's "merged" duplicates share one commit reference with their leader.
func hydrationCandidates() []brainCandidate {
	var candidates []brainCandidate
	for i := range hydrationEntities {
		candidates = append(candidates,
			brainCandidate{Kind: "entity", Status: "committed", CommitRef: fmt.Sprintf("knowledge_entity:e%d", i),
				Payload: map[string]any{"entityIndex": i}},
			brainCandidate{Kind: "fact", Status: "committed", CommitRef: fmt.Sprintf("knowledge_fact:f%d", i),
				Payload: map[string]any{"entityIndex": i}},
			brainCandidate{Kind: "relation", Status: "committed", CommitRef: fmt.Sprintf("knowledge_edge:r%d", i),
				Payload: map[string]any{"fromEntityIndex": i, "toEntityIndex": (i + 1) % hydrationEntities}},
		)
	}
	candidates = append(candidates,
		brainCandidate{Kind: "entity", Status: "merged", CommitRef: "knowledge_entity:e0", Payload: map[string]any{"entityIndex": 0}},
		brainCandidate{Kind: "fact", Status: "duplicate", CommitRef: "knowledge_fact:f0", Payload: map[string]any{"entityIndex": 0}},
	)
	return candidates
}

func TestBrainMaterializeCandidatesReadsIndependentResourcesConcurrentlyOnce(t *testing.T) {
	t.Parallel()

	fixture := &hydrationFixture{t: t, delay: 20 * time.Millisecond, requests: map[string]int{}}
	server := httptest.NewServer(fixture)
	defer server.Close()

	result, err := NewBrain(server.URL, "brain-secret", server.Client(), server.Client()).materializeCandidates(
		t.Context(), "source_document:brain-doc-1", "manifold://documents/ring@r1", "r1", hydrationCandidates(),
	)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Entities) != hydrationEntities || len(result.Facts) != hydrationEntities ||
		len(result.Relations) != hydrationEntities {
		t.Fatalf("graph = %d entities, %d facts, %d relations, want %d of each",
			len(result.Entities), len(result.Facts), len(result.Relations), hydrationEntities)
	}
	for i := 1; i < len(result.Entities); i++ {
		if result.Entities[i-1].BrainID >= result.Entities[i].BrainID {
			t.Fatalf("entities are not sorted by Brain ID: %q before %q",
				result.Entities[i-1].BrainID, result.Entities[i].BrainID)
		}
	}
	for path, count := range fixture.requests {
		if count != 1 {
			t.Errorf("%s was requested %d times, want once", path, count)
		}
	}
	// 24 entities + 24 facts + 24 connection reads, one request each.
	if got, want := len(fixture.requests), 3*hydrationEntities; got != want {
		t.Errorf("distinct resources read = %d, want %d", got, want)
	}
	if peak := fixture.peak.Load(); peak < 2 || peak > brainHydrationConcurrency {
		t.Errorf("peak concurrent reads = %d, want between 2 and %d", peak, brainHydrationConcurrency)
	}
}

func TestBrainMaterializeCandidatesReturnsTheFirstFailureWithoutWaitingForTheRest(t *testing.T) {
	t.Parallel()

	fixture := &hydrationFixture{
		t: t, delay: 20 * time.Millisecond, requests: map[string]int{},
		failPath: "/v1/entities/knowledge_entity:e3",
	}
	server := httptest.NewServer(fixture)
	defer server.Close()

	_, err := NewBrain(server.URL, "brain-secret", server.Client(), server.Client()).materializeCandidates(
		t.Context(), "source_document:brain-doc-1", "manifold://documents/ring@r1", "r1", hydrationCandidates(),
	)
	var dependency *DependencyError
	if !errors.As(err, &dependency) || dependency.Status != http.StatusInternalServerError {
		t.Fatalf("error = %v, want the Brain 500 as a dependency error", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error %v reports the cancellation of a sibling read instead of the failure", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.requests["/v1/facts/knowledge_fact:f0"] != 0 {
		t.Errorf("fact reads started after an entity read failed")
	}
}
