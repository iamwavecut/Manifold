package engine

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iamwavecut/Manifold/internal/llm"
	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/upstream"
)

const testDims = 64

// fakeModel answers extraction from fixtures keyed by a phrase of the
// document text and embeds text as a hashed bag of words, so texts that
// share words are near each other.
type fakeModel struct {
	mu          sync.Mutex
	extractions map[string]Extraction
	chatCalls   atomic.Int32
	embedCalls  atomic.Int32
	chatErr     error
	embedErr    error
	lastUser    string
}

func (m *fakeModel) JSON(_ context.Context, name string, _ map[string]any, _, user string, out any) error {
	m.chatCalls.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastUser = user
	if m.chatErr != nil {
		return m.chatErr
	}
	result := out.(*Extraction)
	for phrase, extraction := range m.extractions {
		if strings.Contains(user, phrase) {
			*result = extraction
			return nil
		}
	}
	*result = Extraction{}
	return nil
}

func (m *fakeModel) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	m.embedCalls.Add(1)
	m.mu.Lock()
	err := m.embedErr
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	vectors := make([][]float32, len(inputs))
	for i, input := range inputs {
		vector := make([]float32, testDims)
		for _, token := range tokens(input) {
			h := fnv.New32a()
			h.Write([]byte(token))
			vector[h.Sum32()%testDims]++
		}
		var norm float64
		for _, value := range vector {
			norm += float64(value * value)
		}
		norm = math.Sqrt(norm)
		if norm == 0 {
			vector[0], norm = 1, 1
		}
		for j := range vector {
			vector[j] = float32(float64(vector[j]) / norm)
		}
		vectors[i] = vector
	}
	return vectors, nil
}

func openTestEngine(t *testing.T, model *fakeModel) *Engine {
	t.Helper()
	return openTestEngineAt(t, filepath.Join(t.TempDir(), "index.db"), model, "embed-a", testDims)
}

func openTestEngineAt(t *testing.T, path string, model *fakeModel, embeddingModel string, dims int) *Engine {
	t.Helper()
	engine, err := Open(context.Background(), Options{
		Path: path, EmbeddingModel: embeddingModel, Dimensions: dims, Model: model,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine
}

func graphDocument(id, revision, content string, occurred time.Time) upstream.GraphDocument {
	return upstream.GraphDocument{
		ID: id, Title: id, Format: "markdown", Content: content,
		OriginURI: "manifold://documents/" + id + "@" + revision,
		Revision:  revision, OccurredAt: occurred.Format(time.RFC3339),
	}
}

func factBy(result upstream.GraphIngestResult, predicate, object string) (upstream.GraphFact, bool) {
	for _, fact := range result.Facts {
		if fact.Predicate == predicate && fact.Object == object {
			return fact, true
		}
	}
	return upstream.GraphFact{}, false
}

var day = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func manifoldExtraction(port string) Extraction {
	return Extraction{
		Entities: []ExtractedEntity{
			{Name: "Manifold", Type: "project", Aliases: []string{"Manifold service"}},
			{Name: "SQLite", Type: "asset"},
		},
		Facts: []ExtractedFact{
			{Entity: 0, Predicate: "service_port", Value: port, Confidence: 0.9, Evidence: "Manifold listens on port " + port},
			{Entity: 0, Predicate: "runs_on", Value: "geta.moe host", Confidence: 0.8, Evidence: "runs on the geta.moe host"},
		},
		Relations: []ExtractedRelation{{From: 0, To: 1, Predicate: "depends_on", Confidence: 0.9}},
	}
}

func TestIngestDocumentReturnsCompleteResultAndReusesIDsAcrossRevisions(t *testing.T) {
	model := &fakeModel{extractions: map[string]Extraction{"port 8080": manifoldExtraction("8080")}}
	engine := openTestEngine(t, model)
	graph := engine.Graph()
	content := "Manifold listens on port 8080. It runs on the geta.moe host and stores data in SQLite."

	first, err := graph.IngestDocument(t.Context(), graphDocument("ops-notes", "r1", content, day))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entities) != 2 || len(first.Facts) != 2 || len(first.Relations) != 1 {
		t.Fatalf("result = %+v", first)
	}
	if first.OriginURI != "manifold://documents/ops-notes@r1" || first.Revision != "r1" || first.DocumentID == "" {
		t.Fatalf("source = %q %q %q", first.DocumentID, first.OriginURI, first.Revision)
	}
	if len(first.FactIDs) != 2 || len(first.EntityIDs) != 2 || len(first.EdgeIDs) != 1 {
		t.Fatalf("declared IDs = %v %v %v", first.EntityIDs, first.FactIDs, first.EdgeIDs)
	}
	port, ok := factBy(first, "service_port", "8080")
	if !ok || port.Status != "active" || port.SourceDocumentID != "ops-notes" || port.Confidence != 0.9 {
		t.Fatalf("port fact = %+v", port)
	}
	if !port.ValidFrom.Equal(day) {
		t.Fatalf("valid_from = %v, want the revision time", port.ValidFrom)
	}

	second, err := graph.IngestDocument(t.Context(), graphDocument("ops-notes", "r2", content+" More text.", day.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	again, _ := factBy(second, "service_port", "8080")
	if again.BrainID != port.BrainID {
		t.Fatalf("restated fact got a new ID: %s, was %s", again.BrainID, port.BrainID)
	}
	if second.Relations[0].BrainID != first.Relations[0].BrainID {
		t.Fatal("restated relation got a new ID")
	}
	if second.Entities[0].BrainID != first.Entities[0].BrainID {
		t.Fatal("restated entity got a new ID")
	}
}

func TestIngestDocumentRepeatsAStoredRevisionWithoutTheModel(t *testing.T) {
	model := &fakeModel{extractions: map[string]Extraction{"port 8080": manifoldExtraction("8080")}}
	graph := openTestEngine(t, model).Graph()
	document := graphDocument("ops-notes", "r1", "Manifold listens on port 8080 and runs on the geta.moe host.", day)
	first, err := graph.IngestDocument(t.Context(), document)
	if err != nil {
		t.Fatal(err)
	}
	calls := model.chatCalls.Load()
	second, err := graph.IngestDocument(t.Context(), document)
	if err != nil {
		t.Fatal(err)
	}
	if model.chatCalls.Load() != calls {
		t.Fatal("a repeated revision called the model again")
	}
	if len(second.Facts) != len(first.Facts) || second.Facts[0] != first.Facts[0] {
		t.Fatalf("repeated result differs: %+v vs %+v", second.Facts, first.Facts)
	}
}

func TestExtractionDropsInvalidAndUngroundedItems(t *testing.T) {
	model := &fakeModel{extractions: map[string]Extraction{"Borealis": {
		Entities: []ExtractedEntity{{Name: "Borealis", Type: "spaceship"}, {Name: "  "}},
		Facts: []ExtractedFact{
			{Entity: 0, Predicate: "Service Port!", Value: "9090", Evidence: "Borealis uses port 9090"},
			{Entity: 0, Predicate: "owner", Value: "Zed Kowalski", Evidence: "Zed Kowalski owns everything"},
			{Entity: 7, Predicate: "status", Value: "live", Evidence: "Borealis"},
			{Entity: 0, Predicate: "", Value: "x", Evidence: "Borealis"},
		},
		Relations: []ExtractedRelation{{From: 0, To: 0, Predicate: "self"}, {From: 0, To: 9, Predicate: "uses"}},
	}}}
	graph := openTestEngine(t, model).Graph()
	result, err := graph.IngestDocument(t.Context(), graphDocument("borealis", "r1", "Borealis uses port 9090.", day))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entities) != 1 || result.Entities[0].Type != "other" {
		t.Fatalf("entities = %+v", result.Entities)
	}
	if len(result.Facts) != 1 || result.Facts[0].Predicate != "service_port" || result.Facts[0].Confidence != 0.5 {
		t.Fatalf("facts = %+v", result.Facts)
	}
	if len(result.Relations) != 0 {
		t.Fatalf("relations = %+v", result.Relations)
	}
}

func TestEntitiesResolveByNameAndAlias(t *testing.T) {
	model := &fakeModel{extractions: map[string]Extraction{
		"first": {
			Entities: []ExtractedEntity{{Name: "Manifold", Type: "project", Aliases: []string{"the memory service"}}},
			Facts:    []ExtractedFact{{Entity: 0, Predicate: "language", Value: "Go", Evidence: "Manifold is written in Go"}},
		},
		"second": {
			Entities: []ExtractedEntity{{Name: "manifold", Type: "other"}},
			Facts:    []ExtractedFact{{Entity: 0, Predicate: "database", Value: "SQLite", Evidence: "uses SQLite"}},
		},
		"third": {
			Entities: []ExtractedEntity{{Name: "The Memory Service", Type: "asset"}},
			Facts:    []ExtractedFact{{Entity: 0, Predicate: "license", Value: "MIT", Evidence: "MIT"}},
		},
	}}
	engine := openTestEngine(t, model)
	graph := engine.Graph()
	ids := map[string]bool{}
	for _, item := range []struct{ id, text string }{
		{"a", "first: Manifold is written in Go"},
		{"b", "second: manifold uses SQLite"},
		{"c", "third: the memory service is MIT licensed"},
	} {
		result, err := graph.IngestDocument(t.Context(), graphDocument(item.id, "r1", item.text, day))
		if err != nil {
			t.Fatal(err)
		}
		ids[result.Entities[0].BrainID] = true
		if result.Entities[0].Name != "Manifold" || result.Entities[0].Type != "project" {
			t.Fatalf("entity = %+v", result.Entities[0])
		}
	}
	if len(ids) != 1 {
		t.Fatalf("one entity became %d", len(ids))
	}
	model.mu.Lock()
	user := model.lastUser
	model.mu.Unlock()
	if !strings.Contains(user, "Manifold (project)") {
		t.Fatalf("known entities were not offered to the model:\n%s", user)
	}
}

func singleFact(entity, predicate, value string) Extraction {
	return Extraction{
		Entities: []ExtractedEntity{{Name: entity, Type: "project"}},
		Facts:    []ExtractedFact{{Entity: 0, Predicate: predicate, Value: value, Confidence: 1, Evidence: value}},
	}
}

func TestSingleValuedPredicateSupersedesAndRetractionRevives(t *testing.T) {
	model := &fakeModel{extractions: map[string]Extraction{
		"alpha": singleFact("Atlas", "status", "alpha"),
		"beta":  singleFact("Atlas", "status", "beta"),
	}}
	engine := openTestEngine(t, model)
	graph := engine.Graph()
	old, err := graph.IngestDocument(t.Context(), graphDocument("old", "r1", "Atlas status: alpha", day))
	if err != nil {
		t.Fatal(err)
	}
	newer, err := graph.IngestDocument(t.Context(), graphDocument("new", "r1", "Atlas status: beta", day.Add(24*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if newer.Facts[0].Status != "active" {
		t.Fatalf("newer value status = %s", newer.Facts[0].Status)
	}
	if status := engine.factStatus(t, old.Facts[0].BrainID); status != "superseded" {
		t.Fatalf("older value status = %s, want superseded", status)
	}
	revived, err := graph.RetractFact(t.Context(), newer.Facts[0].BrainID, "manifold_source_withdrawn")
	if err != nil {
		t.Fatal(err)
	}
	if len(revived) != 1 || revived[0] != old.Facts[0].BrainID {
		t.Fatalf("revived = %v, want the superseded predecessor", revived)
	}
	if status := engine.factStatus(t, old.Facts[0].BrainID); status != "active" {
		t.Fatalf("predecessor status after revival = %s", status)
	}
	again, err := graph.RetractFact(t.Context(), newer.Facts[0].BrainID, "manifold_source_withdrawn")
	if err != nil || len(again) != 0 {
		t.Fatalf("repeated retraction = %v, %v", again, err)
	}
}

func TestOlderSingleValuedStatementDoesNotSupersedeNewerOne(t *testing.T) {
	model := &fakeModel{extractions: map[string]Extraction{
		"alpha": singleFact("Atlas", "status", "alpha"),
		"beta":  singleFact("Atlas", "status", "beta"),
	}}
	engine := openTestEngine(t, model)
	graph := engine.Graph()
	newer, err := graph.IngestDocument(t.Context(), graphDocument("new", "r1", "Atlas status: beta", day.Add(48*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	older, err := graph.IngestDocument(t.Context(), graphDocument("old", "r1", "Atlas status: alpha", day))
	if err != nil {
		t.Fatal(err)
	}
	if older.Facts[0].Status != "superseded" || older.Facts[0].ValidUntil == nil {
		t.Fatalf("older statement = %+v, want superseded with an end", older.Facts[0])
	}
	if status := engine.factStatus(t, newer.Facts[0].BrainID); status != "active" {
		t.Fatalf("newer statement status = %s", status)
	}
}

func TestMultiValuedPredicatesCoexist(t *testing.T) {
	model := &fakeModel{extractions: map[string]Extraction{
		"one": singleFact("Atlas", "uses", "Redis"),
		"two": singleFact("Atlas", "uses", "Postgres"),
	}}
	engine := openTestEngine(t, model)
	graph := engine.Graph()
	first, _ := graph.IngestDocument(t.Context(), graphDocument("one", "r1", "one: Atlas uses Redis", day))
	second, _ := graph.IngestDocument(t.Context(), graphDocument("two", "r1", "two: Atlas uses Postgres", day.Add(time.Hour)))
	for _, id := range []string{first.Facts[0].BrainID, second.Facts[0].BrainID} {
		if status := engine.factStatus(t, id); status != "active" {
			t.Fatalf("fact %s status = %s", id, status)
		}
	}
}

func TestRetractedFactBecomesActiveWhenStatedAgain(t *testing.T) {
	model := &fakeModel{extractions: map[string]Extraction{"Redis": singleFact("Atlas", "uses", "Redis")}}
	engine := openTestEngine(t, model)
	graph := engine.Graph()
	first, _ := graph.IngestDocument(t.Context(), graphDocument("one", "r1", "Atlas uses Redis", day))
	if _, err := graph.RetractFact(t.Context(), first.Facts[0].BrainID, "manifold_source_withdrawn"); err != nil {
		t.Fatal(err)
	}
	second, err := graph.IngestDocument(t.Context(), graphDocument("one", "r2", "Atlas uses Redis, still.", day.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if second.Facts[0].BrainID != first.Facts[0].BrainID || second.Facts[0].Status != "active" {
		t.Fatalf("restated fact = %+v, want the same active fact", second.Facts[0])
	}
}

func TestRetractUnknownAndRelationIDs(t *testing.T) {
	model := &fakeModel{extractions: map[string]Extraction{"port 8080": manifoldExtraction("8080")}}
	engine := openTestEngine(t, model)
	graph := engine.Graph()
	_, err := graph.RetractFact(t.Context(), "missing", "manifold_source_withdrawn")
	var dependency *upstream.DependencyError
	if !errors.As(err, &dependency) || dependency.Status != http.StatusNotFound {
		t.Fatalf("unknown ID error = %v, want a 404 dependency error", err)
	}
	result, err := graph.IngestDocument(t.Context(), graphDocument("ops", "r1", "Manifold listens on port 8080 and runs on the geta.moe host.", day))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.RetractFact(t.Context(), result.Relations[0].BrainID, "manifold_source_withdrawn"); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := engine.db.QueryRow(`SELECT status FROM relations WHERE id = ?`, result.Relations[0].BrainID).Scan(&status); err != nil || status != "retracted" {
		t.Fatalf("relation status = %q, %v", status, err)
	}
}

func TestGraphSearchUsesWordsAndVectorsAndHidesRetractedFacts(t *testing.T) {
	model := &fakeModel{extractions: map[string]Extraction{
		"port 8080": manifoldExtraction("8080"),
		"alpha":     singleFact("Atlas", "status", "alpha"),
		"beta":      singleFact("Atlas", "status", "beta"),
	}}
	engine := openTestEngine(t, model)
	graph := engine.Graph()
	ops, _ := graph.IngestDocument(t.Context(), graphDocument("ops", "r1", "Manifold listens on port 8080 and runs on the geta.moe host.", day))
	graph.IngestDocument(t.Context(), graphDocument("old", "r1", "Atlas status: alpha", day))
	graph.IngestDocument(t.Context(), graphDocument("new", "r1", "Atlas status: beta", day.Add(time.Hour)))
	if _, err := engine.EmbedPending(t.Context()); err != nil {
		t.Fatal(err)
	}

	hits, err := graph.Search(t.Context(), "which port does Manifold use", 5, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Snippet != "service_port 8080" || hits[0].Title != "Manifold" ||
		hits[0].Kind != "fact" || hits[0].Source != "graph" {
		t.Fatalf("hits = %+v", hits)
	}
	if hits := mustSearch(t, graph, "Atlas status", false); containsSnippet(hits, "status alpha") || !containsSnippet(hits, "status beta") {
		t.Fatalf("current search = %+v, want only the current status", hits)
	}
	if hits := mustSearch(t, graph, "Atlas status", true); !containsSnippet(hits, "status alpha") {
		t.Fatalf("history search = %+v, want the superseded status too", hits)
	}
	port, _ := factBy(ops, "service_port", "8080")
	if _, err := graph.RetractFact(t.Context(), port.BrainID, "manifold_source_withdrawn"); err != nil {
		t.Fatal(err)
	}
	if hits := mustSearch(t, graph, "Manifold port 8080", true); containsSnippet(hits, "service_port 8080") {
		t.Fatalf("retracted fact still found: %+v", hits)
	}

	model.mu.Lock()
	model.embedErr = &llm.Error{Operation: "embedding", Status: 503, Class: llm.ClassOutage}
	model.mu.Unlock()
	if hits := mustSearch(t, graph, "geta.moe host", false); !containsSnippet(hits, "runs_on geta.moe host") {
		t.Fatalf("lexical fallback hits = %+v", hits)
	}
}

func mustSearch(t *testing.T, graph *Graph, query string, history bool) []model.SearchHit {
	t.Helper()
	hits, err := graph.Search(t.Context(), query, 10, history)
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func containsSnippet(hits []model.SearchHit, snippet string) bool {
	for _, hit := range hits {
		if hit.Snippet == snippet {
			return true
		}
	}
	return false
}

func TestProviderRefusalReachesTheCallerWithItsClass(t *testing.T) {
	model := &fakeModel{chatErr: &llm.Error{Operation: "chat completion", Status: 402, Class: llm.ClassBilling}}
	graph := openTestEngine(t, model).Graph()
	_, err := graph.IngestDocument(t.Context(), graphDocument("doc", "r1", "Some text.", day))
	var dependency *upstream.DependencyError
	if !errors.As(err, &dependency) || dependency.Refusal != llm.ClassBilling || dependency.Status != 402 {
		t.Fatalf("error = %#v, want a billing refusal", err)
	}
}

func TestManualFactsAndRelations(t *testing.T) {
	engine := openTestEngine(t, &fakeModel{})
	graph := engine.Graph()
	entity := model0Entity("atlas", "Atlas", "project")
	id, outcome, err := graph.IngestFact(t.Context(), model.Fact{EntityID: "atlas", Predicate: "owner", Object: "Ops team", Confidence: 1, ValidFrom: day}, entity)
	if err != nil || id == "" || outcome != "created" {
		t.Fatalf("IngestFact = %q %q %v", id, outcome, err)
	}
	again, outcome, err := graph.IngestFact(t.Context(), model.Fact{EntityID: "atlas", Predicate: "owner", Object: "ops team", Confidence: 1, ValidFrom: day}, entity)
	if err != nil || again != id || outcome != "corroborated" {
		t.Fatalf("repeated IngestFact = %q %q %v, want %q corroborated", again, outcome, err, id)
	}
	relation, err := graph.IngestRelation(t.Context(), model.Relation{Predicate: "depends_on", Confidence: 1},
		entity, model0Entity("redis", "Redis", "asset"))
	if err != nil || relation == "" {
		t.Fatalf("IngestRelation = %q %v", relation, err)
	}
}

func model0Entity(id, name, kind string) model.Entity {
	return model.Entity{ID: id, Name: name, Kind: kind}
}

func TestEmbeddingSettingChangeRebuildsVectors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	model := &fakeModel{extractions: map[string]Extraction{"Redis": singleFact("Atlas", "uses", "Redis")}}
	engine := openTestEngineAt(t, path, model, "embed-a", testDims)
	if _, err := engine.Graph().IngestDocument(t.Context(), graphDocument("one", "r1", "Atlas uses Redis", day)); err != nil {
		t.Fatal(err)
	}
	if n, err := engine.EmbedPending(t.Context()); err != nil || n == 0 {
		t.Fatalf("first embedding pass = %d, %v", n, err)
	}
	if n, _ := engine.EmbedPending(t.Context()); n != 0 {
		t.Fatalf("second pass embedded %d rows again", n)
	}
	engine.Close()
	reopened := openTestEngineAt(t, path, model, "embed-b", testDims)
	if n, err := reopened.EmbedPending(t.Context()); err != nil || n == 0 {
		t.Fatalf("pass after the model change = %d, %v; want every row again", n, err)
	}
}

func TestDocumentsIndexWriteSearchMoveAndDelete(t *testing.T) {
	model := &fakeModel{}
	engine := openTestEngine(t, model)
	documents := engine.Documents()
	ctx := t.Context()
	folder := "viking://resources/manifold/ops/"
	uri := folder + "deploy-notes.md"
	if err := documents.Mkdir(ctx, folder, "Ops"); err != nil {
		t.Fatal(err)
	}
	if err := documents.Write(ctx, uri, "# Deploy\n\nThe broker restarts the compose project after a profile change.", true); err != nil {
		t.Fatal(err)
	}
	err := documents.Write(ctx, uri, "other", true)
	var dependency *upstream.DependencyError
	if !errors.As(err, &dependency) || dependency.Status != http.StatusConflict {
		t.Fatalf("create over an existing file = %v, want 409", err)
	}
	if _, err := engine.EmbedPending(ctx); err != nil {
		t.Fatal(err)
	}
	hits, err := documents.Search(ctx, "broker restarts compose", "", 5)
	if err != nil || len(hits) != 1 || hits[0].ID != uri || hits[0].Source != "semantic" || hits[0].Kind != "document" {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
	if hits, _ := documents.Search(ctx, "broker", "viking://resources/manifold/other/", 5); len(hits) != 0 {
		t.Fatalf("scope did not filter: %+v", hits)
	}
	if _, err := documents.Snapshot(ctx, "sync", []string{uri}); err != nil {
		t.Fatal(err)
	}

	moved := "viking://resources/manifold/runbooks/"
	if err := documents.Move(ctx, folder, moved); err != nil {
		t.Fatal(err)
	}
	if exists, _ := documents.Exists(ctx, uri); exists {
		t.Fatal("old path still exists after move")
	}
	if exists, _ := documents.Exists(ctx, moved+"deploy-notes.md"); !exists {
		t.Fatal("moved file is missing")
	}
	if hits, _ := documents.Search(ctx, "broker restarts", "", 5); len(hits) != 1 || hits[0].ID != moved+"deploy-notes.md" {
		t.Fatalf("hits after move = %+v", hits)
	}
	if err := documents.Delete(ctx, moved, true); err != nil {
		t.Fatal(err)
	}
	if exists, _ := documents.Exists(ctx, moved+"deploy-notes.md"); exists {
		t.Fatal("file survived recursive delete")
	}
	if hits, _ := documents.Search(ctx, "broker restarts", "", 5); len(hits) != 0 {
		t.Fatalf("deleted file still found: %+v", hits)
	}
	if err := documents.Delete(ctx, moved, true); err != nil {
		t.Fatalf("repeated delete = %v", err)
	}
}

func TestScopedDocumentSearchLooksBeyondTheNearestDocumentsOutsideTheScope(t *testing.T) {
	engine := openTestEngine(t, &fakeModel{})
	documents := engine.Documents()
	ctx := t.Context()
	for i := range 60 {
		uri := fmt.Sprintf("viking://resources/manifold/noise/note-%d.md", i)
		if err := documents.Write(ctx, uri, "broker restarts compose project", true); err != nil {
			t.Fatal(err)
		}
	}
	inScope := "viking://resources/manifold/ops/deploy-notes.md"
	if err := documents.Write(ctx, inScope, "broker notes for the operations team", true); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.EmbedPending(ctx); err != nil {
		t.Fatal(err)
	}
	hits, err := documents.Search(ctx, "broker restarts compose project", "viking://resources/manifold/ops/", 1)
	if err != nil || len(hits) != 1 || hits[0].ID != inScope {
		t.Fatalf("scoped hits = %+v, %v; want %s", hits, err, inScope)
	}
}

func TestDocumentSearchReportsEmbeddingFailure(t *testing.T) {
	model := &fakeModel{embedErr: &llm.Error{Operation: "embedding", Status: 503, Class: llm.ClassOutage}}
	documents := openTestEngine(t, model).Documents()
	if _, err := documents.Search(t.Context(), "anything", "", 5); err == nil {
		t.Fatal("search without a query vector reported success")
	}
}

type seedSource struct {
	entities  []SeedEntity
	facts     []SeedFact
	relations []SeedRelation
	folders   []string
	documents []SeedDocument
	ingests   []SeedIngest
}

func (s seedSource) SeedIngests(_ context.Context, fn func(SeedIngest) error) error {
	for _, item := range s.ingests {
		if err := fn(item); err != nil {
			return err
		}
	}
	return nil
}

func (s seedSource) SeedEntities(_ context.Context, fn func(SeedEntity) error) error {
	for _, item := range s.entities {
		if err := fn(item); err != nil {
			return err
		}
	}
	return nil
}

func (s seedSource) SeedFacts(_ context.Context, fn func(SeedFact) error) error {
	for _, item := range s.facts {
		if err := fn(item); err != nil {
			return err
		}
	}
	return nil
}

func (s seedSource) SeedRelations(_ context.Context, fn func(SeedRelation) error) error {
	for _, item := range s.relations {
		if err := fn(item); err != nil {
			return err
		}
	}
	return nil
}

func (s seedSource) SeedFolders(_ context.Context, fn func(string) error) error {
	for _, item := range s.folders {
		if err := fn(item); err != nil {
			return err
		}
	}
	return nil
}

func (s seedSource) SeedDocuments(_ context.Context, fn func(SeedDocument) error) error {
	for _, item := range s.documents {
		if err := fn(item); err != nil {
			return err
		}
	}
	return nil
}

func TestSeedKeepsUpstreamIDsAndHistory(t *testing.T) {
	model := &fakeModel{extractions: map[string]Extraction{"Redis": singleFact("Atlas", "uses", "Redis")}}
	engine := openTestEngine(t, model)
	source := seedSource{
		entities: []SeedEntity{{Key: "knowledge_entity:atlas", UpstreamID: "knowledge_entity:atlas", Name: "Atlas", Kind: "project"}},
		facts: []SeedFact{
			{UpstreamID: "knowledge_fact:1", EntityKey: "knowledge_entity:atlas", Predicate: "uses", Object: "Redis",
				Status: "active", Confidence: 0.7, ValidFrom: day, SourceDocumentID: "one"},
			{UpstreamID: "knowledge_fact:2", EntityKey: "knowledge_entity:atlas", Predicate: "uses", Object: "Memcached",
				Status: "retracted", Confidence: 0.7, ValidFrom: day},
		},
		relations: []SeedRelation{{UpstreamID: "knowledge_edge:1", FromKey: "knowledge_entity:atlas",
			ToKey: "knowledge_entity:atlas", Predicate: "self", Status: "active", Confidence: 1}},
		folders:   []string{"viking://resources/manifold/ops/"},
		documents: []SeedDocument{{URI: "viking://resources/manifold/ops/one.md", Content: "Atlas uses Redis for queues."}},
		ingests: []SeedIngest{{DocumentID: "one", Revision: 1, Result: upstream.GraphIngestResult{
			DocumentID: "one", OriginURI: "manifold://documents/one@r1", Revision: "r1",
			FactIDs: []string{"knowledge_fact:1"},
			Facts: []upstream.GraphFact{{BrainID: "knowledge_fact:1", EntityBrainID: "knowledge_entity:atlas",
				Predicate: "uses", Object: "Redis", Status: "active", Confidence: 0.7, ValidFrom: day, SourceDocumentID: "one"}},
		}}},
	}
	if seeded, _ := engine.Seeded(t.Context()); seeded {
		t.Fatal("new index reports seeded")
	}
	if err := engine.Seed(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if seeded, _ := engine.Seeded(t.Context()); !seeded {
		t.Fatal("seeded index reports unseeded")
	}
	if _, err := engine.EmbedPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	hits := mustSearch(t, engine.Graph(), "Atlas Redis", false)
	if len(hits) == 0 || hits[0].ID != "knowledge_fact:1" {
		t.Fatalf("seeded fact search = %+v", hits)
	}
	if containsSnippet(hits, "uses Memcached") {
		t.Fatal("seeded retracted fact is searchable")
	}
	stored, err := engine.Graph().IngestDocument(t.Context(), graphDocument("one", "r1", "Atlas uses Redis for queues.", day))
	if err != nil {
		t.Fatal(err)
	}
	if model.chatCalls.Load() != 0 || len(stored.Facts) != 1 || stored.Facts[0].Confidence != 0.7 {
		t.Fatalf("seeded revision = %+v after %d model calls, want the projected result", stored, model.chatCalls.Load())
	}
	result, err := engine.Graph().IngestDocument(t.Context(), graphDocument("one", "r2", "Atlas uses Redis", day.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Facts[0].BrainID != "knowledge_fact:1" || result.Entities[0].BrainID != "knowledge_entity:atlas" {
		t.Fatalf("new extraction did not reuse seeded IDs: %+v", result)
	}
	if exists, _ := engine.Documents().Exists(t.Context(), "viking://resources/manifold/ops/one.md"); !exists {
		t.Fatal("seeded document path is missing")
	}
	if hits, err := engine.Documents().Search(t.Context(), "Redis queues", "", 5); err != nil || len(hits) != 1 {
		t.Fatalf("seeded document search = %+v, %v", hits, err)
	}
}

// Manifold's database keeps statuses but not which fact superseded which.
// The seed rebuilds that chain for single-valued predicates, so retracting
// a seeded current value revives the value before it.
func TestSeedRebuildsTheSupersessionChainOfSingleValuedPredicates(t *testing.T) {
	engine := openTestEngine(t, &fakeModel{})
	atlas := "knowledge_entity:atlas"
	fact := func(id, object, status string, at time.Time) SeedFact {
		return SeedFact{UpstreamID: id, EntityKey: atlas, Predicate: "status", Object: object, Status: status,
			Confidence: 0.9, ValidFrom: at}
	}
	source := seedSource{
		entities: []SeedEntity{{Key: atlas, UpstreamID: atlas, Name: "Atlas", Kind: "project"}},
		facts: []SeedFact{
			fact("knowledge_fact:deployed", "deployed", "active", day.Add(48*time.Hour)),
			fact("knowledge_fact:testing", "testing", "superseded", day),
			fact("knowledge_fact:staging", "staging", "superseded", day.Add(24*time.Hour)),
			{UpstreamID: "knowledge_fact:old-owner", EntityKey: atlas, Predicate: "code_memory__owns", Object: "queues",
				Status: "superseded", Confidence: 0.9, ValidFrom: day},
		},
	}
	if err := engine.Seed(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	graph := engine.Graph()
	revived, err := graph.RetractFact(t.Context(), "knowledge_fact:deployed", "test")
	if err != nil || !slices.Equal(revived, []string{"knowledge_fact:staging"}) {
		t.Fatalf("retracting the seeded current status revived %v, %v; want the staging fact", revived, err)
	}
	revived, err = graph.RetractFact(t.Context(), "knowledge_fact:staging", "test")
	if err != nil || !slices.Equal(revived, []string{"knowledge_fact:testing"}) {
		t.Fatalf("retracting the revived status revived %v, %v; want the testing fact", revived, err)
	}
	if status := engine.factStatus(t, "knowledge_fact:testing"); status != "active" {
		t.Fatalf("oldest status = %s, want active", status)
	}
	if status := engine.factStatus(t, "knowledge_fact:old-owner"); status != "superseded" {
		t.Fatalf("superseded fact of a multi-valued predicate = %s, want superseded", status)
	}
}

func (e *Engine) factStatus(t *testing.T, id string) string {
	t.Helper()
	var status string
	if err := e.db.QueryRow(`SELECT status FROM facts WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// After a provider failure the embedder backs off, but a later successful
// provider call proves the provider answers again: new rows must not wait
// out the backoff.
func TestEmbedderResumesOnceTheProviderAnswers(t *testing.T) {
	model := &fakeModel{embedErr: errors.New("provider unreachable")}
	e := openTestEngine(t, model)
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() { e.RunEmbedder(ctx); close(stopped) }()
	defer func() { cancel(); <-stopped }()

	pending := func() int {
		var n int
		if err := e.db.QueryRowContext(ctx, `SELECT count(*) FROM chunks WHERE embedded = 0`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	waitUntil := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting until %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := e.Documents().Write(ctx, "viking://resources/manifold/notes.md", "Ada maintains the parser.", true); err != nil {
		t.Fatal(err)
	}
	waitUntil("the embedder tried and failed", func() bool { return model.embedCalls.Load() >= 1 })
	model.mu.Lock()
	model.embedErr = nil
	model.mu.Unlock()
	if _, err := e.Graph().IngestDocument(ctx, graphDocument("notes", "r1", "Nothing to extract.", time.Now())); err != nil {
		t.Fatal(err)
	}
	waitUntil("the pending chunk got its vector", func() bool { return pending() == 0 })
}

// Concurrent searches for one text share a provider call. When the caller
// that made the call gives up, a caller that still waits must get a vector,
// not the other caller's cancellation.
func TestQueryCacheOutlivesACancelledFirstCaller(t *testing.T) {
	cache := newQueryCache(4)
	started := make(chan struct{})
	var calls atomic.Int32
	embed := func(ctx context.Context, inputs []string) ([][]float32, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return [][]float32{{1, 0}}, nil
	}
	first, cancelFirst := context.WithCancel(t.Context())
	firstDone := make(chan error, 1)
	go func() {
		_, err := cache.vector(first, "deploy", embed)
		firstDone <- err
	}()
	<-started
	second := make(chan error, 1)
	go func() {
		vector, err := cache.vector(t.Context(), "deploy", embed)
		if err == nil && len(vector) != 2 {
			err = fmt.Errorf("vector has %d values, want 2", len(vector))
		}
		second <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancelFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller = %v, want its own cancellation", err)
	}
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("waiting caller = %v, want a vector", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiting caller did not finish")
	}
}

// firstEmbedSucceeds answers the first embedding call and fails every later
// one.
type firstEmbedSucceeds struct {
	*fakeModel
	calls atomic.Int32
}

func (m *firstEmbedSucceeds) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if m.calls.Add(1) > 1 {
		return nil, errors.New("provider unreachable")
	}
	return m.fakeModel.Embed(ctx, inputs)
}

// The embedder's own successful batch is no news about the provider: when a
// later batch of the same pass fails, the embedder backs off instead of
// trying again at once.
func TestEmbedderBacksOffAfterAPartlyFailedPass(t *testing.T) {
	model := &firstEmbedSucceeds{fakeModel: &fakeModel{}}
	e, err := Open(t.Context(), Options{
		Path: filepath.Join(t.TempDir(), "index.db"), EmbeddingModel: "embed-a", Dimensions: testDims, Model: model,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	for i := range embedBatchSize + 6 {
		uri := fmt.Sprintf("viking://resources/manifold/note-%d.md", i)
		if err := e.Documents().Write(t.Context(), uri, fmt.Sprintf("note %d", i), true); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() { e.RunEmbedder(ctx); close(stopped) }()
	defer func() { cancel(); <-stopped }()

	deadline := time.Now().Add(3 * time.Second)
	for model.calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the embedder did not reach the failing batch")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if calls := model.calls.Load(); calls != 2 {
		t.Fatalf("embedding calls = %d within 300 ms of a failure, want 2", calls)
	}
}

// Manifold keeps no index ID for an entity it created through the API: the
// index finds such an entity by name. The seed does the same, so the
// entity's facts survive and no duplicate appears.
func TestSeedFindsUnmappedEntitiesByName(t *testing.T) {
	engine := openTestEngine(t, &fakeModel{})
	err := engine.Seed(t.Context(), seedSource{
		entities: []SeedEntity{
			{Key: "knowledge_entity:atlas", UpstreamID: "knowledge_entity:atlas", Name: "Atlas", Kind: "project"},
			{Key: "manifold:atlas", Name: "Atlas", Kind: "project"},
			{Key: "manifold:redis-team", Name: "Redis Team", Kind: "organization"},
		},
		facts: []SeedFact{
			{UpstreamID: "fact-1", EntityKey: "manifold:atlas", Predicate: "owner", Object: "Operations",
				Status: "active", Confidence: 1, ValidFrom: day},
			{UpstreamID: "fact-2", EntityKey: "manifold:redis-team", Predicate: "on_call", Object: "weekdays",
				Status: "active", Confidence: 1, ValidFrom: day},
		},
		relations: []SeedRelation{{UpstreamID: "relation-1", FromKey: "manifold:redis-team", ToKey: "manifold:atlas",
			Predicate: "supports", Status: "active", Confidence: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var atlases int
	var owner string
	if err := engine.db.QueryRow(`SELECT count(*) FROM entities WHERE norm = 'atlas'`).Scan(&atlases); err != nil {
		t.Fatal(err)
	}
	if err := engine.db.QueryRow(`SELECT e.id FROM facts f JOIN entities e ON e.rid = f.entity_rid WHERE f.id = 'fact-1'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if atlases != 1 || owner != "knowledge_entity:atlas" {
		t.Fatalf("atlas entities = %d, fact-1 entity = %q", atlases, owner)
	}
	hits, err := engine.Graph().Search(t.Context(), "Redis Team on call", 5, false)
	if err != nil || len(hits) == 0 || hits[0].ID != "fact-2" {
		t.Fatalf("hits = %#v, err = %v", hits, err)
	}
	var relations int
	if err := engine.db.QueryRow(`SELECT count(*) FROM relations WHERE id = 'relation-1'`).Scan(&relations); err != nil || relations != 1 {
		t.Fatalf("relations = %d, err = %v", relations, err)
	}
}

// A large write transaction grows the WAL file; SQLite keeps that size
// unless journal_size_limit asks it to shrink the file at the next reset.
func TestWALShrinksAfterALargeWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	e := openTestEngineAt(t, path, &fakeModel{}, "embed-a", testDims)
	ctx := t.Context()
	var large strings.Builder
	for large.Len() < 24<<20 {
		large.WriteString("Ada maintains the parser and the lexer of the compiler toolchain. ")
	}
	if err := e.Documents().Write(ctx, "viking://resources/manifold/large.md", large.String(), true); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		uri := fmt.Sprintf("viking://resources/manifold/small-%d.md", i)
		if err := e.Documents().Write(ctx, uri, "A small note.", true); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 8<<20 {
		t.Fatalf("WAL file stays at %d bytes after the large write", info.Size())
	}
}
