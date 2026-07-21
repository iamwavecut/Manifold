package store

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/model"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}

func createTestDocument(t *testing.T, s *Store, id, format, content string) model.Document {
	t.Helper()
	doc, _, err := s.CreateDocument(t.Context(), model.Document{
		ID: id, Title: id, Format: format, Content: content,
		OVURI: "viking://resources/manifold/" + id + ".md",
	}, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func queueTestRename(t *testing.T, s *Store, plan model.RenamePlan) {
	t.Helper()
	if _, err := s.QueueRename(t.Context(), plan, model.Job{
		ID: identity.NewXID(), Kind: "rename.apply", ResourceType: "rename_plan", ResourceID: plan.ID,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAvailableSlugUsesDeterministicSuffixes(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateFolder(t.Context(), model.Folder{ID: "memory", Name: "Memory"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFolder(t.Context(), model.Folder{ID: "memory-2", Name: "Memory 2"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.AvailableSlug(t.Context(), "folder", "Memory")
	if err != nil {
		t.Fatal(err)
	}
	if got != "memory-3" {
		t.Fatalf("available slug = %q, want memory-3", got)
	}
}

func TestCreateDocumentAtPathCreatesAndReusesHierarchyAtomically(t *testing.T) {
	s := openTestStore(t)
	create := func(id string, segments []string) ([]model.Folder, error) {
		_, _, folders, err := s.CreateDocumentAtPath(t.Context(), segments, model.Document{
			ID: id, Title: id, Format: "markdown", Content: id,
			OVURI: "viking://resources/manifold/" + strings.Join(segments, "/") + "/" + id + ".md",
		}, model.Job{
			ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: id,
		})
		return folders, err
	}

	created, err := create("memory-policy", []string{"shared", "agent-practice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 || created[0].Path != "/shared" || created[1].Path != "/shared/agent-practice" {
		t.Fatalf("created folders = %#v", created)
	}
	reused, err := create("memory-procedure", []string{"shared", "agent-practice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reused) != 0 {
		t.Fatalf("reused path created duplicate folders: %#v", reused)
	}
	path, err := s.DocumentPath(t.Context(), "memory-procedure")
	if err != nil || path != "shared/agent-practice/memory-procedure" {
		t.Fatalf("document path = %q, err = %v", path, err)
	}

	_, err = create("isolated-task", []string{"tasks", "agent-practice"})
	var conflict *FolderPathConflictError
	if !errors.As(err, &conflict) || conflict.ExistingPath != "shared/agent-practice" ||
		conflict.RequestedPath != "tasks/agent-practice" {
		t.Fatalf("path conflict = %#v, err = %v", conflict, err)
	}
	if _, err := s.GetFolder(t.Context(), "tasks"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed path left a partial tasks folder: %v", err)
	}
	if _, err := s.GetDocument(t.Context(), "isolated-task", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed path left a partial document: %v", err)
	}
}

func TestMigrationScrubsExistingAPIKeyIdempotencyResponses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifold.db")
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIdempotency(
		t.Context(), "bootstrap-admin:create-key", http.MethodPost, "/api/v1/api-keys",
		"request-hash", http.StatusCreated, []byte(`{"secret":"must-not-remain"}`),
	); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	_, body, _, err := s.GetIdempotency(
		t.Context(), "bootstrap-admin:create-key", http.MethodPost, "/api/v1/api-keys",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Fatalf("migration retained a one-time API key response: %q", body)
	}
}

func TestRenameSwapCascadesAndPreservesHistoricalContent(t *testing.T) {
	s := openTestStore(t)
	createTestDocument(t, s, "alpha", "markdown", "alpha links to [[beta]]; alpha-beta stays whole.")
	createTestDocument(t, s, "beta", "markdown", "beta links to manifold://documents/alpha.")

	plan, err := s.CreateRenamePlan(t.Context(), identity.NewXID(), []model.RenameOperation{
		{ResourceType: "document", From: "alpha", To: "beta"},
		{ResourceType: "document", From: "beta", To: "alpha"},
	})
	if err != nil {
		t.Fatal(err)
	}
	queueTestRename(t, s, plan)
	if _, err := s.ApplyRename(t.Context(), plan.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizeRename(t.Context(), plan.ID); err != nil {
		t.Fatal(err)
	}

	alpha, err := s.GetDocument(t.Context(), "alpha", true)
	if err != nil {
		t.Fatal(err)
	}
	if alpha.Revision != "r2" {
		t.Fatalf("rewritten active document revision = %s, want r2", alpha.Revision)
	}
	if alpha.Content != "alpha links to manifold://documents/beta." {
		t.Fatalf("cycle replacement corrupted content: %q", alpha.Content)
	}
	beta, err := s.GetDocument(t.Context(), "beta", true)
	if err != nil {
		t.Fatal(err)
	}
	if beta.Content != "beta links to [[alpha]]; alpha-beta stays whole." {
		t.Fatalf("token-bounded replacement mismatch: %q", beta.Content)
	}
	revisions, err := s.ListRevisions(t.Context(), "beta")
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 2 || revisions[1].Content != "alpha links to [[beta]]; alpha-beta stays whole." {
		t.Fatalf("historical snapshot was rewritten: %#v", revisions)
	}
}

func TestRenameThreeWayCycleIsAtomic(t *testing.T) {
	s := openTestStore(t)
	for _, id := range []string{"alpha", "beta", "gamma"} {
		createTestDocument(t, s, id, "markdown", id)
	}
	plan, err := s.CreateRenamePlan(t.Context(), identity.NewXID(), []model.RenameOperation{
		{ResourceType: "document", From: "alpha", To: "beta"},
		{ResourceType: "document", From: "beta", To: "gamma"},
		{ResourceType: "document", From: "gamma", To: "alpha"},
	})
	if err != nil {
		t.Fatal(err)
	}
	queueTestRename(t, s, plan)
	if _, err := s.ApplyRename(t.Context(), plan.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizeRename(t.Context(), plan.ID); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		"alpha": "alpha",
		"beta":  "beta",
		"gamma": "gamma",
	}
	for id, content := range expected {
		document, err := s.GetDocument(t.Context(), id, true)
		if err != nil {
			t.Fatal(err)
		}
		if document.Content != content {
			t.Fatalf("%s content = %q, want %q", id, document.Content, content)
		}
	}
}

func TestRenamePreviewBecomesStaleAfterStateChange(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateFolder(t.Context(), model.Folder{ID: "old", Name: "Old"}); err != nil {
		t.Fatal(err)
	}
	plan, err := s.CreateRenamePlan(t.Context(), identity.NewXID(), []model.RenameOperation{
		{ResourceType: "folder", From: "old", To: "new"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFolder(t.Context(), model.Folder{ID: "other", Name: "Other"}); err != nil {
		t.Fatal(err)
	}
	queueTestRename(t, s, plan)
	if _, err := s.ApplyRename(t.Context(), plan.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale plan error = %v, want ErrConflict", err)
	}
	if err := s.CancelQueuedRename(t.Context(), plan.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetFolder(t.Context(), "old"); err != nil {
		t.Fatalf("stale plan changed active state: %v", err)
	}
}

func TestRenameCollisionIsReportedInPreview(t *testing.T) {
	s := openTestStore(t)
	for _, id := range []string{"old", "occupied"} {
		if _, err := s.CreateFolder(t.Context(), model.Folder{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := s.CreateRenamePlan(t.Context(), identity.NewXID(), []model.RenameOperation{
		{ResourceType: "folder", From: "old", To: "occupied"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Preview.Conflicts) != 1 {
		t.Fatalf("preview conflicts = %#v, want occupied target", plan.Preview.Conflicts)
	}
	for _, id := range []string{"old", "occupied"} {
		if _, err := s.GetFolder(t.Context(), id); err != nil {
			t.Fatalf("rollback lost folder %q: %v", id, err)
		}
	}
}

func TestRenameRollbackRestoresOldIDsContentAndReservations(t *testing.T) {
	s := openTestStore(t)
	createTestDocument(t, s, "old", "markdown", "old links to [[old]].")
	plan, err := s.CreateRenamePlan(t.Context(), identity.NewXID(), []model.RenameOperation{
		{ResourceType: "document", From: "old", To: "new"},
	})
	if err != nil {
		t.Fatal(err)
	}
	queueTestRename(t, s, plan)
	if available, err := s.SlugAvailable(t.Context(), "document", "new"); err != nil || available {
		t.Fatalf("reserved target available=%v err=%v", available, err)
	}
	if _, err := s.ApplyRename(t.Context(), plan.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RollbackRename(t.Context(), plan.ID, nil); err != nil {
		t.Fatal(err)
	}
	doc, err := s.GetDocument(t.Context(), "old", true)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Content != "old links to [[old]]." || doc.Revision != "r1" {
		t.Fatalf("rollback did not restore exact active document: %#v", doc)
	}
	if _, err := s.GetDocument(t.Context(), "new", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("new slug remained active after rollback: %v", err)
	}
	if available, err := s.SlugAvailable(t.Context(), "document", "new"); err != nil || !available {
		t.Fatalf("rollback did not release target available=%v err=%v", available, err)
	}
}

func TestRenamePreviewReportsBinaryBlocker(t *testing.T) {
	s := openTestStore(t)
	createTestDocument(t, s, "old", "pdf", "binary payload contains old")
	plan, err := s.CreateRenamePlan(t.Context(), identity.NewXID(), []model.RenameOperation{
		{ResourceType: "document", From: "old", To: "new"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Preview.Blockers) != 1 {
		t.Fatalf("blockers = %#v, want one precise blocker", plan.Preview.Blockers)
	}
	blocker := plan.Preview.Blockers[0]
	if blocker.Resource != "documents/old" || blocker.Location != "old@r1" {
		t.Fatalf("blocker does not identify resource and location: %#v", blocker)
	}
}

func TestRenamePreviewAndApplyIncludeTitleMetadataAndTags(t *testing.T) {
	s := openTestStore(t)
	doc, _, err := s.CreateDocument(t.Context(), model.Document{
		ID: "old", Title: "Notes for old", Format: "markdown", Content: "No matching content.",
		OVURI: "viking://resources/manifold/old.md",
		Metadata: map[string]any{
			"canonical": "manifold://documents/old",
			"owner":     "old",
		},
		Tags: []string{"old", "stable"},
	}, model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: "old",
	})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Revision != "r1" {
		t.Fatalf("initial revision = %s", doc.Revision)
	}
	plan, err := s.CreateRenamePlan(t.Context(), identity.NewXID(), []model.RenameOperation{
		{ResourceType: "document", From: "old", To: "new"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Preview.Replacements) != 1 || plan.Preview.Replacements[0].Occurrences != 4 {
		t.Fatalf("preview replacements = %#v, want title + two metadata + tag", plan.Preview.Replacements)
	}
	queueTestRename(t, s, plan)
	if _, err := s.ApplyRename(t.Context(), plan.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizeRename(t.Context(), plan.ID); err != nil {
		t.Fatal(err)
	}
	renamed, err := s.GetDocument(t.Context(), "new", true)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Title != "Notes for new" ||
		renamed.Metadata["canonical"] != "manifold://documents/new" ||
		renamed.Metadata["owner"] != "new" ||
		len(renamed.Tags) != 2 || renamed.Tags[0] != "new" ||
		renamed.Revision != "r2" {
		t.Fatalf("renamed document = %#v", renamed)
	}
}

func TestListsCompleteWithSingleSQLiteConnection(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateFolder(t.Context(), model.Folder{ID: "folder", Name: "Folder"}); err != nil {
		t.Fatal(err)
	}
	createTestDocument(t, s, "doc", "markdown", "content")

	done := make(chan error, 1)
	go func() {
		if _, err := s.ListFolders(t.Context()); err != nil {
			done <- err
			return
		}
		_, err := s.ListJobs(t.Context(), 10)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("list operation deadlocked while a result cursor held the only SQLite connection")
	}
}

func TestConflictingFactsArePreservedForExplicitResolution(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateEntity(t.Context(), model.Entity{ID: "service", Name: "Service", Kind: "system"}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"active", "retired"} {
		if _, err := s.CreateFact(t.Context(), model.Fact{
			ID: identity.NewXID(), EntityID: "service", Predicate: "lifecycle", Object: value,
			Origin: "explicit", Status: "active", Confidence: 1, ValidFrom: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	conflicts, err := s.ListConflicts(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].Status != "unresolved" {
		t.Fatalf("conflicts = %#v, want one unresolved conflict", conflicts)
	}
	resolved, err := s.ResolveConflict(t.Context(), conflicts[0].ID, "The service is active.")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != "resolved" || resolved.Resolution == "" {
		t.Fatalf("resolved conflict = %#v", resolved)
	}
}
