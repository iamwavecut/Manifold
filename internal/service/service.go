package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iamwavecut/Manifold/internal/buildinfo"
	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/problem"
	"github.com/iamwavecut/Manifold/internal/selector"
	"github.com/iamwavecut/Manifold/internal/store"
	"github.com/iamwavecut/Manifold/internal/upstream"
)

type Service struct {
	store      *store.Store
	documents  upstream.DocumentStore
	graph      upstream.GraphStore
	publicURL  string
	logger     *slog.Logger
	workerTick time.Duration
	build      buildinfo.Info
}

type Status struct {
	Service          string            `json:"service"`
	Version          string            `json:"version"`
	Commit           string            `json:"commit"`
	APIMajor         int               `json:"api_major"`
	ProtocolRevision int               `json:"protocol_revision"`
	Features         []string          `json:"features"`
	State            string            `json:"state"`
	Components       map[string]string `json:"components"`
	Counts           map[string]int    `json:"counts"`
}

type SearchRequest struct {
	Query          string
	Mode           model.SearchMode
	Scope          string
	ScopeGlob      string
	SourceTypes    []string
	Limit          int
	IncludeHistory bool
}

type SearchResponse struct {
	Items                []model.SearchHit `json:"items"`
	DegradedDependencies []string          `json:"degraded_dependencies,omitempty"`
}

type folderMove struct {
	key   string
	from  string
	stage string
	to    string
	phase int
}

type resourceMove struct {
	key   string
	from  string
	stage string
	to    string
	phase int
}

func New(s *store.Store, documents upstream.DocumentStore, graph upstream.GraphStore, publicURL string, logger *slog.Logger, workerTick time.Duration, builds ...buildinfo.Info) *Service {
	build := buildinfo.New("dev", "unknown")
	if len(builds) > 0 {
		build = builds[0]
	}
	return &Service{
		store: s, documents: documents, graph: graph, publicURL: publicURL,
		logger: logger, workerTick: workerTick, build: build,
	}
}

func (s *Service) Store() *store.Store {
	return s.store
}

func (s *Service) BuildInfo() buildinfo.Info {
	return s.build
}

func (s *Service) Status(ctx context.Context) (Status, error) {
	status := Status{
		Service: s.build.Service, Version: s.build.Version, Commit: s.build.Commit,
		APIMajor: s.build.APIMajor, ProtocolRevision: s.build.ProtocolRevision,
		Features: s.build.Features, State: "ready",
		Components: map[string]string{"sqlite": "ready", "openviking": "unknown", "brain": "unknown"},
	}
	if err := s.store.Ping(ctx); err != nil {
		status.Components["sqlite"] = "failed"
		status.State = "failed"
	}
	if err := s.documents.Health(ctx); err != nil {
		status.Components["openviking"] = "degraded"
		status.State = "degraded"
	} else {
		status.Components["openviking"] = "ready"
	}
	if err := s.graph.Health(ctx); err != nil {
		status.Components["brain"] = "degraded"
		if status.State == "ready" {
			status.State = "degraded"
		}
	} else {
		status.Components["brain"] = "ready"
	}
	counts, err := s.store.Metrics(ctx)
	if err != nil {
		return Status{}, err
	}
	status.Counts = counts
	return status, nil
}

func (s *Service) CreateFolder(ctx context.Context, folder model.Folder) (model.Folder, error) {
	uri, err := s.folderURIFor(ctx, folder.ParentID, folder.ID)
	if err != nil {
		return model.Folder{}, err
	}
	if err := s.documents.Mkdir(ctx, uri, folder.Summary); err != nil {
		return model.Folder{}, err
	}
	return s.store.CreateFolder(ctx, folder)
}

func (s *Service) DeleteFolder(ctx context.Context, id string) error {
	empty, err := s.store.FolderEmpty(ctx, id)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("%w: folder is not empty", store.ErrConflict)
	}
	path, err := s.store.FolderPath(ctx, id)
	if err != nil {
		return err
	}
	uri := "viking://resources/manifold" + path + "/"
	for attempt := range 6 {
		err = s.documents.Delete(ctx, uri, true)
		if err == nil {
			break
		}
		var dependency *upstream.DependencyError
		if !errors.As(err, &dependency) || dependency.Dependency != "openviking" {
			return err
		}
		if dependency.Status == http.StatusNotFound {
			break
		}
		if dependency.Status != http.StatusConflict {
			return err
		}
		dependency.Retryable = true
		if attempt == 5 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return s.store.DeleteFolder(ctx, id)
}

func (s *Service) CreateDocument(ctx context.Context, doc model.Document) (model.Document, model.Job, error) {
	uri, err := s.documentURI(ctx, doc.FolderID, doc.ID, doc.Format)
	if err != nil {
		return model.Document{}, model.Job{}, err
	}
	doc.OVURI = uri
	payload, _ := json.Marshal(map[string]any{"create": true})
	job := model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: doc.ID,
		Payload: payload,
	}
	return s.store.CreateDocument(ctx, doc, job)
}

func (s *Service) CreateDocumentAtPath(
	ctx context.Context,
	folderPath string,
	doc model.Document,
) (model.Document, model.Job, []model.Folder, error) {
	folderPath = strings.Trim(folderPath, "/")
	segments := strings.Split(folderPath, "/")
	for _, segment := range segments {
		if !identity.IsSlug(segment) {
			return model.Document{}, model.Job{}, nil, fmt.Errorf("invalid folder path segment %q", segment)
		}
	}
	doc.OVURI = "viking://resources/manifold/" + folderPath + "/" + doc.ID + extension(doc.Format)
	folderPaths := make([]string, 0, len(segments))
	for index := range segments {
		folderPaths = append(folderPaths, strings.Join(segments[:index+1], "/"))
	}
	payload, _ := json.Marshal(map[string]any{"create": true, "folder_paths": folderPaths})
	job := model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: doc.ID,
		Payload: payload,
	}
	return s.store.CreateDocumentAtPath(ctx, segments, doc, job)
}

func (s *Service) UpdateDocument(ctx context.Context, id, ifMatch, title, content string, metadata map[string]any, tags []string) (model.Document, model.Job, error) {
	payload, _ := json.Marshal(map[string]any{"create": false})
	job := model.Job{
		ID: identity.NewXID(), Kind: "document.sync", ResourceType: "document", ResourceID: id,
		Payload: payload,
	}
	return s.store.UpdateDocument(ctx, id, ifMatch, title, content, metadata, tags, job)
}

func (s *Service) DeleteDocument(ctx context.Context, id, ifMatch string) (model.Job, error) {
	doc, err := s.store.GetDocument(ctx, id, true)
	if err != nil {
		return model.Job{}, err
	}
	payload, _ := json.Marshal(map[string]any{"ov_uri": doc.OVURI, "brain_id": doc.BrainID})
	job := model.Job{
		ID: identity.NewXID(), Kind: "document.delete", ResourceType: "document", ResourceID: id,
		Payload: payload,
	}
	return s.store.MarkDocumentDeleted(ctx, id, ifMatch, job)
}

func (s *Service) CreateRenamePlan(ctx context.Context, operations []model.RenameOperation) (model.RenamePlan, error) {
	return s.store.CreateRenamePlan(ctx, identity.NewXID(), operations)
}

func (s *Service) ApplyRenamePlan(ctx context.Context, planID string) (model.Job, error) {
	plan, err := s.store.GetRenamePlan(ctx, planID)
	if err != nil {
		return model.Job{}, err
	}
	if plan.Status != "previewed" {
		return model.Job{}, store.ErrConflict
	}
	if len(plan.Preview.Blockers) > 0 || len(plan.Preview.Conflicts) > 0 {
		return model.Job{}, fmt.Errorf("%w: unrewritable references", store.ErrConflict)
	}
	job := model.Job{
		ID: identity.NewXID(), Kind: "rename.apply", ResourceType: "rename_plan", ResourceID: planID,
		Payload: json.RawMessage(`{}`),
	}
	return s.store.QueueRename(ctx, plan, job)
}

func (s *Service) CreateFact(ctx context.Context, fact model.Fact) (model.Fact, error) {
	entity, err := s.store.GetEntity(ctx, fact.EntityID)
	if err != nil {
		return model.Fact{}, err
	}
	created, err := s.store.CreateFact(ctx, fact)
	if err != nil {
		return model.Fact{}, err
	}
	upstreamID, _, err := s.graph.IngestFact(ctx, created, entity)
	if err != nil {
		return created, nil
	}
	_ = s.store.UpdateFactUpstream(ctx, created.ID, upstreamID, "")
	created.UpstreamID = upstreamID
	return created, nil
}

func (s *Service) CreateRelation(ctx context.Context, relation model.Relation) (model.Relation, error) {
	from, err := s.store.GetEntity(ctx, relation.FromID)
	if err != nil {
		return model.Relation{}, err
	}
	to, err := s.store.GetEntity(ctx, relation.ToID)
	if err != nil {
		return model.Relation{}, err
	}
	created, err := s.store.CreateRelation(ctx, relation)
	if err != nil {
		return model.Relation{}, err
	}
	upstreamID, err := s.graph.IngestRelation(ctx, created, from, to)
	if err == nil {
		_ = s.store.UpdateRelationUpstream(ctx, created.ID, upstreamID)
		created.UpstreamID = upstreamID
	}
	return created, nil
}

func (s *Service) RetractFact(ctx context.Context, id, reason string) error {
	fact, err := s.store.GetFact(ctx, id)
	if err != nil {
		return err
	}
	if fact.UpstreamID != "" {
		if err := s.graph.RetractFact(ctx, fact.UpstreamID, reason); err != nil {
			return err
		}
	}
	return s.store.RetractFact(ctx, id)
}

func (s *Service) Search(ctx context.Context, request SearchRequest) (SearchResponse, error) {
	if request.Limit <= 0 {
		request.Limit = 10
	}
	if err := selector.ValidateGlob(request.ScopeGlob); err != nil {
		return SearchResponse{}, err
	}
	type sourceResult struct {
		name string
		hits []model.SearchHit
		err  error
	}
	results := make(chan sourceResult, 2)
	var wg sync.WaitGroup
	lists := make([][]model.SearchHit, 0, 3)
	if request.Mode == model.SearchHybrid || request.Mode == model.SearchLexical {
		local, err := s.store.SearchDocuments(ctx, request.Query, request.Limit*2)
		if err != nil {
			return SearchResponse{}, err
		}
		canonical, err := s.canonicalizeSearchHits(ctx, local, request)
		if err != nil {
			return SearchResponse{}, err
		}
		lists = append(lists, canonical)
	}
	if request.Mode == model.SearchHybrid || request.Mode == model.SearchSemantic {
		wg.Go(func() {
			hits, err := s.documents.Search(ctx, request.Query, request.Scope, request.Limit*2)
			results <- sourceResult{name: "openviking", hits: hits, err: err}
		})
	}
	if request.Mode == model.SearchHybrid || request.Mode == model.SearchSemantic || request.Mode == model.SearchGraph {
		wg.Go(func() {
			hits, err := s.graph.Search(ctx, request.Query, request.Limit*2, request.IncludeHistory)
			results <- sourceResult{name: "brain", hits: hits, err: err}
		})
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	response := SearchResponse{}
	for result := range results {
		if result.err != nil {
			response.DegradedDependencies = append(response.DegradedDependencies, result.name)
			continue
		}
		canonical, err := s.canonicalizeSearchHits(ctx, result.hits, request)
		if err != nil {
			return SearchResponse{}, err
		}
		lists = append(lists, canonical)
	}
	response.Items = reciprocalRankFusion(lists, request.Limit)
	return response, nil
}

func (s *Service) canonicalizeSearchHits(
	ctx context.Context,
	hits []model.SearchHit,
	request SearchRequest,
) ([]model.SearchHit, error) {
	allowedKinds := map[string]bool{}
	for _, kind := range request.SourceTypes {
		allowedKinds[strings.TrimSpace(kind)] = true
	}
	canonical := make([]model.SearchHit, 0, len(hits))
	for _, hit := range hits {
		var mapped model.SearchHit
		var ok bool
		var err error
		switch hit.Kind {
		case "document":
			mapped, ok, err = s.canonicalizeDocumentHit(ctx, hit)
		case "fact":
			mapped, ok, err = s.canonicalizeFactHit(ctx, hit)
		}
		if err != nil {
			return nil, err
		}
		if !ok || (len(allowedKinds) > 0 && !allowedKinds[mapped.Kind]) {
			continue
		}
		if request.ScopeGlob != "" && (mapped.Path == "" || !selector.Match(request.ScopeGlob, mapped.Path)) {
			continue
		}
		canonical = append(canonical, mapped)
	}
	return canonical, nil
}

func (s *Service) canonicalizeDocumentHit(ctx context.Context, hit model.SearchHit) (model.SearchHit, bool, error) {
	var doc model.Document
	var err error
	if hit.Source == "openviking" {
		doc, err = s.store.GetDocumentByOVURI(ctx, hit.ID)
	} else {
		doc, err = s.store.GetDocument(ctx, hit.ID, false)
	}
	if errors.Is(err, store.ErrNotFound) {
		return model.SearchHit{}, false, nil
	}
	if err != nil {
		return model.SearchHit{}, false, err
	}
	path, err := s.store.DocumentPath(ctx, doc.ID)
	if err != nil {
		return model.SearchHit{}, false, err
	}
	hit.Kind = "document"
	hit.ID = doc.ID
	hit.Title = doc.Title
	hit.Revision = doc.Revision
	hit.Path = path
	hit.CanonicalRef = "manifold://documents/" + doc.ID + "@" + doc.Revision
	hit.UpstreamSourceRef = ""
	return hit, true, nil
}

func (s *Service) canonicalizeFactHit(ctx context.Context, hit model.SearchHit) (model.SearchHit, bool, error) {
	fact, err := s.store.GetFactByUpstreamID(ctx, hit.ID)
	if err == nil {
		hit.ID = fact.ID
		hit.CanonicalRef = "manifold://facts/" + fact.ID
		hit.UpstreamSourceRef = ""
		if fact.SourceDocumentID != "" {
			hit.Path, err = s.store.DocumentPath(ctx, fact.SourceDocumentID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return model.SearchHit{}, false, err
			}
		}
		return hit, true, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return model.SearchHit{}, false, err
	}
	if hit.UpstreamSourceRef == "" {
		return model.SearchHit{}, false, nil
	}
	doc, err := s.store.GetDocumentByUpstreamRef(ctx, hit.UpstreamSourceRef)
	if errors.Is(err, store.ErrNotFound) {
		return model.SearchHit{}, false, nil
	}
	if err != nil {
		return model.SearchHit{}, false, err
	}
	path, err := s.store.DocumentPath(ctx, doc.ID)
	if err != nil {
		return model.SearchHit{}, false, err
	}
	hit.Kind = "document"
	hit.ID = doc.ID
	hit.Title = doc.Title
	hit.Revision = doc.Revision
	hit.Path = path
	hit.CanonicalRef = "manifold://documents/" + doc.ID + "@" + doc.Revision
	hit.UpstreamSourceRef = ""
	return hit, true, nil
}

func (s *Service) Context(ctx context.Context, request SearchRequest, budget int) (model.ContextPack, []string, error) {
	search, err := s.Search(ctx, request)
	if err != nil {
		return model.ContextPack{}, nil, err
	}
	if budget <= 0 {
		budget = 4000
	}
	var text strings.Builder
	items := make([]model.SearchHit, 0, len(search.Items))
	used := 0
	truncated := false
	for _, hit := range search.Items {
		block := fmt.Sprintf("[%s:%s] %s\n%s\n\n", hit.Kind, hit.ID, hit.Title, hit.Snippet)
		tokens := estimateTokens(block)
		if used+tokens > budget {
			truncated = true
			break
		}
		text.WriteString(block)
		used += tokens
		items = append(items, hit)
	}
	return model.ContextPack{
		Query: request.Query, Items: items, Text: text.String(),
		EstimatedTokens: used, TokenBudget: budget, Truncated: truncated,
	}, search.DegradedDependencies, nil
}

func (s *Service) RunWorker(ctx context.Context) {
	ticker := time.NewTicker(s.workerTick)
	defer ticker.Stop()
	for {
		if err := s.processOne(ctx); err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, context.Canceled) {
			s.logger.Error("job processing failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) processOne(ctx context.Context) error {
	job, err := s.store.NextJob(ctx)
	if err != nil {
		return err
	}
	s.logger.Info("job started", "job_id", job.ID, "kind", job.Kind, "resource", job.ResourceID)
	switch job.Kind {
	case "document.sync":
		err = s.syncDocument(ctx, job)
	case "document.delete":
		err = s.deleteDocument(ctx, job)
	case "rename.apply":
		err = s.applyRename(ctx, job)
	default:
		err = fmt.Errorf("unknown job kind %q", job.Kind)
	}
	if err == nil {
		_ = s.store.SetJobStatus(ctx, job.ID, model.JobReady, nil)
		s.logger.Info("job completed", "job_id", job.ID, "kind", job.Kind)
		return nil
	}
	errorContext := problem.WithRequestID(ctx, identity.NewXID())
	semantic := s.dependencyProblem(errorContext, job, err)
	if job.Kind == "rename.apply" {
		if plan, getErr := s.store.GetRenamePlan(ctx, job.ResourceID); getErr == nil && plan.Error != nil {
			semantic = plan.Error
		}
	}
	status := model.JobFailed
	if job.Kind == "document.sync" {
		doc, getErr := s.store.GetDocument(ctx, job.ResourceID, false)
		if getErr == nil && doc.Status == model.JobExtracting {
			status = model.JobPartiallyReady
			_ = s.store.SetDocumentSync(ctx, doc.ID, string(model.JobPartiallyReady), "", "")
		}
	}
	_ = s.store.SetJobStatus(ctx, job.ID, status, semantic)
	s.logger.Error("job failed", "job_id", job.ID, "kind", job.Kind, "code", semantic.Code)
	return err
}

func (s *Service) syncDocument(ctx context.Context, job model.Job) error {
	doc, err := s.store.GetDocument(ctx, job.ResourceID, true)
	if err != nil {
		return err
	}
	var payload struct {
		Create      bool     `json:"create"`
		FolderPaths []string `json:"folder_paths"`
	}
	_ = json.Unmarshal(job.Payload, &payload)
	for _, path := range payload.FolderPaths {
		uri := "viking://resources/manifold/" + strings.Trim(path, "/") + "/"
		if err := s.ensureFolder(ctx, uri, "Manifold folder "+path); err != nil {
			return err
		}
	}

	var snapshot string
	if doc.Status == model.JobExtracting || doc.Status == model.JobPartiallyReady {
		var revisionNumber int
		if _, err := fmt.Sscanf(doc.Revision, "r%d", &revisionNumber); err != nil {
			return fmt.Errorf("parse document revision %q: %w", doc.Revision, err)
		}
		revision, err := s.store.GetRevision(ctx, doc.ID, revisionNumber)
		if err != nil {
			return err
		}
		if revision.SnapshotOID == "" {
			return fmt.Errorf("document %s has no OpenViking checkpoint for %s", doc.ID, doc.Revision)
		}
		snapshot = revision.SnapshotOID
	} else {
		if err := s.documents.Write(ctx, doc.OVURI, doc.Content, payload.Create); err != nil {
			if !payload.Create {
				return err
			}
			exists, existsErr := s.documents.Exists(ctx, doc.OVURI)
			if existsErr != nil || !exists {
				return err
			}
			if replaceErr := s.documents.Write(ctx, doc.OVURI, doc.Content, false); replaceErr != nil {
				return replaceErr
			}
		}
		snapshot, err = s.documents.Snapshot(ctx,
			fmt.Sprintf("%s %s %s", job.Kind, doc.ID, doc.Revision), []string{doc.OVURI})
		if err != nil {
			return err
		}
		if err := s.store.SetDocumentSync(ctx, doc.ID, string(model.JobExtracting), "", snapshot); err != nil {
			return err
		}
	}
	ingest, err := s.graph.IngestDocument(ctx, upstream.GraphDocument{
		ID: doc.ID, Title: doc.Title, Format: doc.Format, Content: doc.Content,
		OriginURI: doc.OVURI, Revision: doc.Revision, OccurredAt: doc.UpdatedAt.Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	return s.store.SetDocumentSync(ctx, doc.ID, string(model.JobReady), ingest.DocumentID, snapshot)
}

func (s *Service) deleteDocument(ctx context.Context, job model.Job) error {
	var payload struct {
		OVURI   string `json:"ov_uri"`
		BrainID string `json:"brain_id"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return err
	}
	if err := s.documents.Delete(ctx, payload.OVURI, false); err != nil {
		return err
	}
	_, err := s.documents.Snapshot(ctx, "delete document "+job.ResourceID, []string{payload.OVURI})
	return err
}

func (s *Service) applyRename(ctx context.Context, job model.Job) error {
	plan, err := s.store.GetRenamePlan(ctx, job.ResourceID)
	if err != nil {
		return err
	}
	var before []model.Document
	var beforeFolders []model.Folder
	switch plan.Status {
	case "accepted":
		before, err = s.store.ListAllDocuments(ctx)
		if err == nil {
			beforeFolders, err = s.store.ListFolders(ctx)
		}
	case "applying":
		before, err = s.store.RenameBackupDocuments(ctx, job.ResourceID)
		if err == nil {
			beforeFolders, err = foldersBeforeRename(ctx, s.store, plan)
		}
	default:
		return fmt.Errorf("rename plan %s is in unexpected state %s", job.ResourceID, plan.Status)
	}
	if err != nil {
		return err
	}
	documentMapping := map[string]string{}
	for _, operation := range plan.Preview.Operations {
		if operation.ResourceType == "document" {
			documentMapping[operation.From] = operation.To
		}
	}
	if plan.Status == "accepted" {
		if _, err := s.store.ApplyRename(ctx, job.ResourceID); err != nil {
			errorContext := problem.WithRequestID(ctx, identity.NewXID())
			semantic := problem.New(errorContext, s.publicURL, 409, "stale_rename_plan",
				"Rename preview is stale", "Knowledge state changed before the queued rename could start.",
				problem.Remediation{
					Summary: "Create and review a fresh rename plan.",
					Steps:   []string{"Create a new preview.", "Review every change and blocker.", "Apply the new plan."},
				})
			_ = s.store.CancelQueuedRename(ctx, job.ResourceID, semantic)
			return err
		}
	}
	after, err := s.store.ListAllDocuments(ctx)
	if err != nil {
		return err
	}
	afterFolders, err := s.store.ListFolders(ctx)
	if err != nil {
		return err
	}
	folderMoves := buildFolderMoves(plan, beforeFolders, afterFolders)
	previousURIs := make(map[string]string, len(after))
	expectedURIs := make(map[string]string, len(after))
	for _, doc := range after {
		oldID := doc.ID
		for from, to := range documentMapping {
			if to == doc.ID {
				oldID = from
				break
			}
		}
		previous, previousErr := s.documentURI(ctx, doc.FolderID, oldID, doc.Format)
		if previousErr != nil {
			return previousErr
		}
		expected, expectedErr := s.documentURI(ctx, doc.FolderID, doc.ID, doc.Format)
		if expectedErr != nil {
			return expectedErr
		}
		previousURIs[doc.ID] = previous
		expectedURIs[doc.ID] = expected
	}
	documentMoves := buildDocumentMoves(job.ResourceID, after, previousURIs, expectedURIs)
	syncErr := s.syncRenamedFolders(ctx, job.ResourceID, folderMoves)
	if syncErr == nil {
		syncErr = s.syncRenamedResources(ctx, job.ResourceID, documentMoves)
	}
	if syncErr == nil {
		syncErr = s.syncRenamedDocuments(ctx, job, after)
	}
	if syncErr != nil {
		errorContext := problem.WithRequestID(ctx, identity.NewXID())
		semantic := s.dependencyProblem(errorContext, job, syncErr)
		if rollbackErr := s.store.RollbackRename(ctx, job.ResourceID, semantic); rollbackErr != nil {
			return fmt.Errorf("rename compensation failed: %w (original error: %v)", rollbackErr, syncErr)
		}
		s.compensateRenameUpstreams(
			ctx, before, documentMoves, folderMoves, job.ResourceID,
		)
		return syncErr
	}
	_ = s.documents.Delete(ctx, "viking://resources/manifold/.manifold-renames/"+job.ResourceID+"/", true)
	return s.store.FinalizeRename(ctx, job.ResourceID)
}

func (s *Service) syncRenamedDocuments(
	ctx context.Context,
	job model.Job,
	after []model.Document,
) error {
	for _, doc := range after {
		expected, err := s.documentURI(ctx, doc.FolderID, doc.ID, doc.Format)
		if err != nil {
			return err
		}
		if err := s.store.SetDocumentOVURI(ctx, doc.ID, expected); err != nil {
			return err
		}
		doc.OVURI = expected
		if err := s.documents.Write(ctx, expected, doc.Content, false); err != nil {
			return err
		}
		snapshot, err := s.documents.Snapshot(ctx, "rename plan "+job.ResourceID, []string{expected})
		if err != nil {
			return err
		}
		ingest, graphErr := s.graph.IngestDocument(ctx, upstream.GraphDocument{
			ID: doc.ID, Title: doc.Title, Format: doc.Format, Content: doc.Content,
			OriginURI: expected, Revision: doc.Revision, OccurredAt: doc.UpdatedAt.Format(time.RFC3339),
		})
		if graphErr != nil {
			return graphErr
		}
		if err := s.store.SetDocumentSync(ctx, doc.ID, string(model.JobReady), ingest.DocumentID, snapshot); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) syncRenamedFolders(ctx context.Context, planID string, moves []folderMove) error {
	if len(moves) == 0 {
		return nil
	}
	stageRoot := "viking://resources/manifold/.manifold-renames/"
	planRoot := stageRoot + planID + "/"
	if err := s.ensureFolder(ctx, stageRoot, "Manifold internal rename staging"); err != nil {
		return err
	}
	if err := s.ensureFolder(ctx, planRoot, "Rename plan "+planID); err != nil {
		return err
	}
	progress, err := s.store.RenameProgress(ctx, planID)
	if err != nil {
		return err
	}
	for index := range moves {
		moves[index].phase = progress[moves[index].key]
	}
	sort.SliceStable(moves, func(i, j int) bool {
		return strings.Count(moves[i].from, "/") > strings.Count(moves[j].from, "/")
	})
	for index := range moves {
		if moves[index].phase >= 2 {
			continue
		}
		if moves[index].phase == 0 {
			if err := s.store.SetRenameProgress(ctx, planID, moves[index].key, 1); err != nil {
				return err
			}
			moves[index].phase = 1
		}
		if err := s.moveFolderToStage(ctx, &moves[index]); err != nil {
			return err
		}
		if err := s.store.SetRenameProgress(ctx, planID, moves[index].key, 2); err != nil {
			return err
		}
		moves[index].phase = 2
	}
	sort.SliceStable(moves, func(i, j int) bool {
		return strings.Count(moves[i].to, "/") < strings.Count(moves[j].to, "/")
	})
	for index := range moves {
		if moves[index].phase >= 4 {
			continue
		}
		if moves[index].phase == 2 {
			if err := s.store.SetRenameProgress(ctx, planID, moves[index].key, 3); err != nil {
				return err
			}
			moves[index].phase = 3
		}
		if err := s.moveResource(ctx, moves[index].stage, moves[index].to); err != nil {
			return err
		}
		if err := s.store.SetRenameProgress(ctx, planID, moves[index].key, 4); err != nil {
			return err
		}
		moves[index].phase = 4
	}
	return nil
}

func (s *Service) syncRenamedResources(ctx context.Context, planID string, moves []resourceMove) error {
	if len(moves) == 0 {
		return nil
	}
	stageRoot := "viking://resources/manifold/.manifold-renames/" + planID + "/documents/"
	if err := s.ensureFolder(ctx, stageRoot, "Renamed documents"); err != nil {
		return err
	}
	progress, err := s.store.RenameProgress(ctx, planID)
	if err != nil {
		return err
	}
	for index := range moves {
		moves[index].phase = progress[moves[index].key]
	}
	for index := range moves {
		if moves[index].phase >= 2 {
			continue
		}
		if moves[index].phase == 0 {
			if err := s.store.SetRenameProgress(ctx, planID, moves[index].key, 1); err != nil {
				return err
			}
			moves[index].phase = 1
		}
		if err := s.moveResource(ctx, moves[index].from, moves[index].stage); err != nil {
			return err
		}
		if err := s.store.SetRenameProgress(ctx, planID, moves[index].key, 2); err != nil {
			return err
		}
		moves[index].phase = 2
	}
	for index := range moves {
		if moves[index].phase >= 4 {
			continue
		}
		if moves[index].phase == 2 {
			if err := s.store.SetRenameProgress(ctx, planID, moves[index].key, 3); err != nil {
				return err
			}
			moves[index].phase = 3
		}
		if err := s.moveResource(ctx, moves[index].stage, moves[index].to); err != nil {
			return err
		}
		if err := s.store.SetRenameProgress(ctx, planID, moves[index].key, 4); err != nil {
			return err
		}
		moves[index].phase = 4
	}
	return nil
}

func (s *Service) compensateRenameUpstreams(
	ctx context.Context,
	before []model.Document,
	documentMoves []resourceMove,
	folderMoves []folderMove,
	planID string,
) {
	s.rollbackRenamedResources(ctx, documentMoves, planID)
	s.rollbackRenamedFolders(ctx, folderMoves, planID)
	for _, original := range before {
		if err := s.documents.Write(ctx, original.OVURI, original.Content, false); err != nil {
			s.logger.Error("rename upstream content compensation failed", "plan_id", planID, "document_id", original.ID)
			continue
		}
		_, _ = s.documents.Snapshot(ctx, "compensate rename plan "+planID, []string{original.OVURI})
		if _, err := s.graph.IngestDocument(ctx, upstream.GraphDocument{
			ID: original.ID, Title: original.Title, Format: original.Format, Content: original.Content,
			OriginURI: original.OVURI, Revision: original.Revision, OccurredAt: original.UpdatedAt.Format(time.RFC3339),
		}); err != nil {
			s.logger.Error("rename graph compensation failed", "plan_id", planID, "document_id", original.ID)
		}
	}
}

func (s *Service) rollbackRenamedFolders(ctx context.Context, moves []folderMove, planID string) {
	sort.SliceStable(moves, func(i, j int) bool {
		return strings.Count(moves[i].to, "/") > strings.Count(moves[j].to, "/")
	})
	for index := range moves {
		if moves[index].phase < 3 {
			continue
		}
		if err := s.moveResource(ctx, moves[index].to, moves[index].stage); err != nil {
			s.logger.Error("rename folder stage compensation failed", "plan_id", planID)
			continue
		}
		moves[index].phase = 2
	}
	sort.SliceStable(moves, func(i, j int) bool {
		return strings.Count(moves[i].from, "/") < strings.Count(moves[j].from, "/")
	})
	for index := range moves {
		if moves[index].phase == 0 || moves[index].phase > 2 {
			continue
		}
		if err := s.moveResource(ctx, moves[index].stage, moves[index].from); err != nil {
			s.logger.Error("rename folder compensation failed", "plan_id", planID)
		}
	}
	_ = s.documents.Delete(ctx, "viking://resources/manifold/.manifold-renames/"+planID+"/", true)
}

func (s *Service) rollbackRenamedResources(ctx context.Context, moves []resourceMove, planID string) {
	for index := len(moves) - 1; index >= 0; index-- {
		if moves[index].phase < 3 {
			continue
		}
		if err := s.moveResource(ctx, moves[index].to, moves[index].stage); err != nil {
			s.logger.Error("rename document stage compensation failed", "plan_id", planID)
			continue
		}
		moves[index].phase = 2
	}
	for index := len(moves) - 1; index >= 0; index-- {
		if moves[index].phase == 0 || moves[index].phase > 2 {
			continue
		}
		if err := s.moveResource(ctx, moves[index].stage, moves[index].from); err != nil {
			s.logger.Error("rename document compensation failed", "plan_id", planID)
		}
	}
}

func (s *Service) ensureFolder(ctx context.Context, uri, description string) error {
	if err := s.documents.Mkdir(ctx, uri, description); err != nil {
		exists, statErr := s.documents.Exists(ctx, uri)
		if statErr == nil && exists {
			return nil
		}
		return err
	}
	return nil
}

func (s *Service) moveResource(ctx context.Context, from, to string) error {
	if err := s.documents.Move(ctx, from, to); err != nil {
		fromExists, fromErr := s.documents.Exists(ctx, from)
		toExists, toErr := s.documents.Exists(ctx, to)
		if fromErr == nil && toErr == nil && !fromExists && toExists {
			return nil
		}
		return err
	}
	return nil
}

func (s *Service) moveFolderToStage(ctx context.Context, move *folderMove) error {
	if err := s.documents.Move(ctx, move.from, move.stage); err != nil {
		fromExists, fromErr := s.documents.Exists(ctx, move.from)
		stageExists, stageErr := s.documents.Exists(ctx, move.stage)
		toExists, toErr := s.documents.Exists(ctx, move.to)
		if fromErr == nil && stageErr == nil && toErr == nil && !fromExists {
			switch {
			case stageExists:
				move.phase = 1
				return nil
			case toExists:
				move.phase = 2
				return nil
			}
		}
		return err
	}
	move.phase = 1
	return nil
}

func buildFolderMoves(plan model.RenamePlan, before, after []model.Folder) []folderMove {
	beforeByID := make(map[string]model.Folder, len(before))
	afterByID := make(map[string]model.Folder, len(after))
	for _, folder := range before {
		beforeByID[folder.ID] = folder
	}
	for _, folder := range after {
		afterByID[folder.ID] = folder
	}
	var moves []folderMove
	for index, operation := range plan.Preview.Operations {
		if operation.ResourceType != "folder" {
			continue
		}
		from, fromOK := beforeByID[operation.From]
		to, toOK := afterByID[operation.To]
		if !fromOK || !toOK {
			continue
		}
		moves = append(moves, folderMove{
			key:   fmt.Sprintf("folder:%d", index),
			from:  folderURI(from.Path),
			stage: fmt.Sprintf("viking://resources/manifold/.manifold-renames/%s/%d/", plan.ID, index),
			to:    folderURI(to.Path),
		})
	}
	return moves
}

func buildDocumentMoves(
	planID string,
	documents []model.Document,
	previousURIs map[string]string,
	expectedURIs map[string]string,
) []resourceMove {
	var moves []resourceMove
	for _, document := range documents {
		from := previousURIs[document.ID]
		to := expectedURIs[document.ID]
		if from == "" || to == "" || from == to {
			continue
		}
		index := len(moves)
		moves = append(moves, resourceMove{
			key:  "document:" + document.ID,
			from: from,
			stage: fmt.Sprintf(
				"viking://resources/manifold/.manifold-renames/%s/documents/%d%s",
				planID, index, extension(document.Format),
			),
			to: to,
		})
	}
	return moves
}

func foldersBeforeRename(ctx context.Context, s *store.Store, plan model.RenamePlan) ([]model.Folder, error) {
	current, err := s.ListFolders(ctx)
	if err != nil {
		return nil, err
	}
	reverse := map[string]string{}
	for _, operation := range plan.Preview.Operations {
		if operation.ResourceType == "folder" {
			reverse[operation.To] = operation.From
		}
	}
	for index := range current {
		if oldID, ok := reverse[current[index].ID]; ok {
			current[index].ID = oldID
		}
		if oldParentID, ok := reverse[current[index].ParentID]; ok {
			current[index].ParentID = oldParentID
		}
		for newID, oldID := range reverse {
			current[index].Path = replacePathSegment(current[index].Path, newID, oldID)
		}
	}
	return current, nil
}

func replacePathSegment(path, from, to string) string {
	parts := strings.Split(path, "/")
	for index := range parts {
		if parts[index] == from {
			parts[index] = to
		}
	}
	return strings.Join(parts, "/")
}

func folderURI(path string) string {
	return "viking://resources/manifold" + path + "/"
}

func (s *Service) folderURIFor(ctx context.Context, parentID, id string) (string, error) {
	if parentID == "" {
		return "viking://resources/manifold/" + id + "/", nil
	}
	path, err := s.store.FolderPath(ctx, parentID)
	if err != nil {
		return "", err
	}
	return "viking://resources/manifold" + path + "/" + id + "/", nil
}

func (s *Service) documentURI(ctx context.Context, folderID, id, format string) (string, error) {
	base := "viking://resources/manifold"
	if folderID != "" {
		path, err := s.store.FolderPath(ctx, folderID)
		if err != nil {
			return "", err
		}
		base += path
	}
	return base + "/" + id + extension(format), nil
}

func extension(format string) string {
	switch strings.ToLower(format) {
	case "markdown", "md":
		return ".md"
	case "json":
		return ".json"
	case "yaml", "yml":
		return ".yaml"
	case "toml":
		return ".toml"
	case "javascript", "js":
		return ".js"
	case "typescript", "ts":
		return ".ts"
	case "python", "py":
		return ".py"
	default:
		return ".txt"
	}
}

func reciprocalRankFusion(lists [][]model.SearchHit, limit int) []model.SearchHit {
	type fused struct {
		hit   model.SearchHit
		score float64
	}
	byKey := map[string]*fused{}
	for _, hits := range lists {
		for rank, hit := range hits {
			key := hit.Kind + ":" + hit.ID
			entry := byKey[key]
			if entry == nil {
				entry = &fused{hit: hit}
				byKey[key] = entry
			}
			entry.score += 1 / float64(60+rank+1)
		}
	}
	values := make([]fused, 0, len(byKey))
	for _, entry := range byKey {
		entry.hit.Score = entry.score
		values = append(values, *entry)
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].score == values[j].score {
			return values[i].hit.ID < values[j].hit.ID
		}
		return values[i].score > values[j].score
	})
	if len(values) > limit {
		values = values[:limit]
	}
	result := make([]model.SearchHit, len(values))
	for index := range values {
		result[index] = values[index].hit
	}
	return result
}

func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return max(1, (len([]rune(text))+3)/4)
}

func (s *Service) dependencyProblem(ctx context.Context, job model.Job, err error) *problem.Error {
	var dependency *upstream.DependencyError
	if errors.As(err, &dependency) {
		p := problem.New(ctx, s.publicURL, 503, "dependency_unavailable", "Dependency unavailable",
			fmt.Sprintf("%s could not complete %s.", dependency.Dependency, dependency.Operation),
			problem.Remediation{
				Summary: "Wait for the dependency to recover, then retry the job.",
				Steps: []string{
					"Check GET /api/v1/status for component health.",
					"Fix the model provider or dependency configuration if it remains degraded.",
					fmt.Sprintf("POST /api/v1/jobs/%s/retry after recovery.", job.ID),
				},
			})
		p.Retryable = dependency.Retryable
		p.RetryAfter = "30s"
		p.Job = "/api/v1/jobs/" + job.ID
		return p
	}
	return problem.New(ctx, s.publicURL, 500, "job_failed", "Job failed",
		"The background operation failed without changing the canonical source.",
		problem.Remediation{
			Summary: "Inspect the job and request logs, correct the cause, then retry.",
			Steps: []string{
				fmt.Sprintf("GET /api/v1/jobs/%s", job.ID),
				"Use request_id to find the matching structured log entry.",
				fmt.Sprintf("POST /api/v1/jobs/%s/retry after correcting the cause.", job.ID),
			},
		})
}
