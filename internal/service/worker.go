package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/problem"
	"github.com/iamwavecut/Manifold/internal/store"
	"github.com/iamwavecut/Manifold/internal/upstream"
)

const jobLeaseRenewInterval = time.Minute

type documentSyncPayload struct {
	Create               bool     `json:"create"`
	FolderPaths          []string `json:"folder_paths"`
	BrainDocumentID      string   `json:"brain_document_id"`
	ManualRetryRequested bool     `json:"manual_retry_requested"`
	BrainRetryKey        string   `json:"brain_retry_key"`
}

type renameDocumentCheckpointPayload struct {
	BrainDocumentIDs map[string]string `json:"rename_brain_document_ids"`
}

type retryAfterProvider interface {
	RetryAfter() time.Duration
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
	jobCtx, stopLeaseRenewal := s.startJobLeaseRenewal(ctx, job)
	switch job.Kind {
	case "document.sync":
		err = s.syncDocument(jobCtx, job)
	case "document.delete":
		err = s.deleteDocument(jobCtx, job)
	case "rename.apply":
		err = s.applyRename(jobCtx, job)
	default:
		err = fmt.Errorf("unknown job kind %q", job.Kind)
	}
	leaseErr := stopLeaseRenewal()
	if errors.Is(leaseErr, store.ErrConflict) {
		return fmt.Errorf("job lease was lost: %w", leaseErr)
	}
	if leaseErr != nil {
		err = errors.Join(err, fmt.Errorf("renew job lease: %w", leaseErr))
	}
	if job.Kind == "document.sync" && errors.Is(err, store.ErrDocumentSyncSuperseded) {
		if statusErr := s.store.SetJobStatus(ctx, job.ID, model.JobReady, nil); statusErr != nil {
			return fmt.Errorf("mark superseded document sync job ready: %w", statusErr)
		}
		s.logger.Info("document sync superseded", "job_id", job.ID, "document_id", job.ResourceID)
		return nil
	}
	if err == nil {
		if statusErr := s.store.SetJobStatus(ctx, job.ID, model.JobReady, nil); statusErr != nil {
			return fmt.Errorf("mark job ready: %w", statusErr)
		}
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
	var transitionErr error
	if job.Kind == "document.sync" {
		status, transitionErr = s.failDocumentSync(ctx, job)
		if errors.Is(transitionErr, store.ErrDocumentSyncSuperseded) {
			status = model.JobReady
			semantic = nil
			transitionErr = nil
			s.logger.Info("document sync failed after being superseded", "job_id", job.ID, "document_id", job.ResourceID)
		}
	}
	statusErr := s.store.SetJobStatus(ctx, job.ID, status, semantic)
	if statusErr != nil {
		transitionErr = errors.Join(transitionErr, fmt.Errorf("set job status to %s: %w", status, statusErr))
	}
	if status == model.JobReady && transitionErr == nil {
		return nil
	}
	if transitionErr != nil {
		return errors.Join(err, transitionErr)
	}
	s.logger.Error("job failed", "job_id", job.ID, "kind", job.Kind, "code", semantic.Code)
	return err
}

func (s *Service) startJobLeaseRenewal(ctx context.Context, job model.Job) (context.Context, func() error) {
	jobCtx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	renewalErr := make(chan error, 1)
	go func() {
		defer close(finished)
		ticker := time.NewTicker(jobLeaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				if err := s.store.RenewJobLease(ctx, job.ID, job.Attempts, 2*time.Minute); err != nil {
					select {
					case renewalErr <- err:
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	return jobCtx, func() error {
		cancel()
		<-finished
		select {
		case err := <-renewalErr:
			return err
		default:
			return nil
		}
	}
}

func (s *Service) failDocumentSync(ctx context.Context, job model.Job) (model.JobStatus, error) {
	revisionName, err := s.store.ResolveDocumentSyncRevision(ctx, job)
	if err != nil {
		return model.JobFailed, fmt.Errorf("resolve failed document sync revision: %w", err)
	}
	revisionNumber, err := strconv.Atoi(strings.TrimPrefix(revisionName, "r"))
	if err != nil {
		return model.JobFailed, fmt.Errorf("parse document revision %q: %w", revisionName, err)
	}
	revision, err := s.store.GetRevision(ctx, job.ResourceID, revisionNumber)
	if err != nil {
		return model.JobFailed, fmt.Errorf("load failed document sync revision: %w", err)
	}
	docStatus, jobStatus := model.JobFailed, model.JobFailed
	if revision.SnapshotOID != "" {
		docStatus, jobStatus = model.JobPartiallyReady, model.JobPartiallyReady
	}
	if err := s.store.SetDocumentSyncForRevision(ctx, job.ResourceID, revisionName, docStatus, "", ""); err != nil {
		return jobStatus, fmt.Errorf("set failed document sync state: %w", err)
	}
	return jobStatus, nil
}

func (s *Service) syncDocument(ctx context.Context, job model.Job) error {
	revisionName, err := s.store.ResolveDocumentSyncRevision(ctx, job)
	if err != nil {
		return err
	}
	doc, err := s.store.GetDocument(ctx, job.ResourceID, true)
	if errors.Is(err, store.ErrNotFound) {
		return store.ErrDocumentSyncSuperseded
	}
	if err != nil {
		return err
	}
	if doc.Revision != revisionName {
		return store.ErrDocumentSyncSuperseded
	}
	revisionNumber, err := strconv.Atoi(strings.TrimPrefix(revisionName, "r"))
	if err != nil {
		return fmt.Errorf("parse document revision %q: %w", revisionName, err)
	}
	revision, err := s.store.GetRevision(ctx, doc.ID, revisionNumber)
	if err != nil {
		return err
	}
	var payload documentSyncPayload
	if len(job.Payload) > 0 {
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("decode document sync job payload: %w", err)
		}
	}

	snapshot := revision.SnapshotOID
	if snapshot == "" {
		if err := s.store.SetDocumentSyncForRevision(ctx, doc.ID, revisionName, model.JobIndexing, "", ""); err != nil {
			return err
		}
		for _, path := range payload.FolderPaths {
			uri := "viking://resources/manifold/" + strings.Trim(path, "/") + "/"
			if err := s.ensureFolder(ctx, uri, "Manifold folder "+path); err != nil {
				return err
			}
		}
		if err := s.documents.Write(ctx, revision.OVURI, revision.Content, payload.Create); err != nil {
			if !payload.Create {
				return err
			}
			exists, existsErr := s.documents.Exists(ctx, revision.OVURI)
			if existsErr != nil || !exists {
				return err
			}
			if replaceErr := s.documents.Write(ctx, revision.OVURI, revision.Content, false); replaceErr != nil {
				return replaceErr
			}
		}
		snapshot, err = s.documents.Snapshot(ctx,
			fmt.Sprintf("%s %s %s", job.Kind, doc.ID, revisionName), []string{revision.OVURI})
		if err != nil {
			return err
		}
		if err := s.store.SetDocumentSyncForRevision(ctx, doc.ID, revisionName, model.JobExtracting, "", snapshot); err != nil {
			return err
		}
	} else if err := s.store.SetDocumentSyncForRevision(ctx, doc.ID, revisionName, model.JobExtracting, "", ""); err != nil {
		return err
	}

	graphDocument := upstream.GraphDocument{
		ID: doc.ID, Title: doc.Title, Format: doc.Format, Content: revision.Content,
		OriginURI: "manifold://documents/" + doc.ID + "@" + revisionName,
		Revision:  revisionName, OccurredAt: revision.CreatedAt.Format(time.RFC3339),
	}
	extractionCtx, cancel := context.WithTimeout(ctx, s.brainExtractionTimeout)
	defer cancel()
	if async, ok := s.graph.(upstream.AsyncGraphStore); ok {
		return s.syncAsyncDocument(extractionCtx, async, job, doc, revisionName, snapshot, graphDocument, payload)
	}
	ingest, err := s.graph.IngestDocument(extractionCtx, graphDocument)
	if err != nil {
		return err
	}
	if err := s.materializeGraphDocument(extractionCtx, doc, ingest); err != nil {
		return err
	}
	return s.store.SetDocumentSyncForRevision(ctx, doc.ID, revisionName, model.JobReady, ingest.DocumentID, snapshot)
}

func (s *Service) syncAsyncDocument(
	ctx context.Context,
	graph upstream.AsyncGraphStore,
	job model.Job,
	doc model.Document,
	revision string,
	snapshot string,
	graphDocument upstream.GraphDocument,
	payload documentSyncPayload,
) error {
	brainDocumentID := payload.BrainDocumentID
	if brainDocumentID == "" {
		var err error
		brainDocumentID, err = s.submitBrainDocument(ctx, graph, graphDocument)
		if err != nil {
			return err
		}
		if brainDocumentID == "" {
			return errors.New("Brain accepted document without returning a document ID")
		}
		if err := s.store.MergeJobPayload(ctx, job.ID, map[string]any{"brain_document_id": brainDocumentID}); err != nil {
			return fmt.Errorf("persist Brain document ID: %w", err)
		}
	}

	state, err := s.fetchBrainDocumentState(ctx, graph, brainDocumentID, doc.ID, revision)
	if err != nil {
		return err
	}
	if payload.ManualRetryRequested {
		switch state.Status {
		case upstream.GraphDocumentFailed, upstream.GraphDocumentPartial:
			retryKey := payload.BrainRetryKey
			if retryKey == "" {
				retryKey = fmt.Sprintf("job-%s-attempt-%d", job.ID, job.Attempts)
				if err := s.store.MergeJobPayload(ctx, job.ID, map[string]any{"brain_retry_key": retryKey}); err != nil {
					return fmt.Errorf("persist Brain retry key: %w", err)
				}
			}
			if err := s.ensureDocumentSyncCurrent(ctx, doc.ID, revision); err != nil {
				return err
			}
			if err := s.retryBrainDocument(ctx, graph, brainDocumentID, retryKey); err != nil {
				return err
			}
			if err := s.store.MergeJobPayload(ctx, job.ID, map[string]any{"manual_retry_requested": false}); err != nil {
				return fmt.Errorf("persist completed Brain retry request: %w", err)
			}
			state, err = s.fetchBrainDocumentState(ctx, graph, brainDocumentID, doc.ID, revision)
			if err != nil {
				return err
			}
		case upstream.GraphDocumentPending, upstream.GraphDocumentReady:
			if err := s.store.MergeJobPayload(ctx, job.ID, map[string]any{"manual_retry_requested": false}); err != nil {
				return fmt.Errorf("persist resumed Brain retry request: %w", err)
			}
		}
	}

	state, err = s.waitForBrainDocument(ctx, graph, brainDocumentID, doc.ID, revision, state)
	if err != nil {
		return err
	}
	result := state.Result
	if result.DocumentID == "" {
		result.DocumentID = brainDocumentID
	}
	if result.OriginURI == "" {
		result.OriginURI = graphDocument.OriginURI
	}
	if result.Revision == "" {
		result.Revision = graphDocument.Revision
	}
	switch state.Status {
	case upstream.GraphDocumentReady:
		if err := s.materializeGraphDocument(ctx, doc, result); err != nil {
			return err
		}
		return s.store.SetDocumentSyncForRevision(ctx, doc.ID, revision, model.JobReady, result.DocumentID, snapshot)
	case upstream.GraphDocumentPartial:
		if err := s.materializeGraphDocument(ctx, doc, result); err != nil {
			return err
		}
		if err := s.store.SetDocumentSyncForRevision(ctx, doc.ID, revision, model.JobPartiallyReady, result.DocumentID, snapshot); err != nil {
			return err
		}
		return terminalBrainDocumentError(state)
	case upstream.GraphDocumentFailed:
		if err := s.store.SetDocumentSyncForRevision(ctx, doc.ID, revision, model.JobExtracting, result.DocumentID, snapshot); err != nil {
			return err
		}
		return terminalBrainDocumentError(state)
	default:
		return fmt.Errorf("Brain returned unsupported document state %q", state.Status)
	}
}

func (s *Service) submitBrainDocument(
	ctx context.Context,
	graph upstream.AsyncGraphStore,
	document upstream.GraphDocument,
) (string, error) {
	for {
		if err := s.ensureDocumentSyncCurrent(ctx, document.ID, document.Revision); err != nil {
			return "", err
		}
		documentID, err := graph.SubmitDocument(ctx, document)
		if err == nil {
			return documentID, nil
		}
		if ctx.Err() != nil {
			return "", brainDeadlineError(ctx, "submit document", err)
		}
		if !retryableUpstreamError(err) {
			return "", err
		}
		if waitErr := waitForContext(ctx, brainRetryDelay(err, s.brainPollInterval)); waitErr != nil {
			return "", brainDeadlineError(ctx, "submit document", err)
		}
	}
}

func (s *Service) fetchBrainDocumentState(
	ctx context.Context,
	graph upstream.AsyncGraphStore,
	documentID string,
	localDocumentID string,
	revision string,
) (upstream.GraphDocumentState, error) {
	for {
		if err := s.ensureDocumentSyncCurrent(ctx, localDocumentID, revision); err != nil {
			return upstream.GraphDocumentState{}, err
		}
		state, err := graph.DocumentResult(ctx, documentID)
		if err == nil {
			if state.Status != upstream.GraphDocumentPending && state.Status != upstream.GraphDocumentReady &&
				state.Status != upstream.GraphDocumentPartial && state.Status != upstream.GraphDocumentFailed {
				return upstream.GraphDocumentState{}, fmt.Errorf("Brain returned unsupported document state %q", state.Status)
			}
			return state, nil
		}
		if ctx.Err() != nil {
			return upstream.GraphDocumentState{}, brainDeadlineError(ctx, "get document result", err)
		}
		if !retryableUpstreamError(err) {
			return upstream.GraphDocumentState{}, err
		}
		if waitErr := waitForContext(ctx, brainRetryDelay(err, s.brainPollInterval)); waitErr != nil {
			return upstream.GraphDocumentState{}, brainDeadlineError(ctx, "get document result", err)
		}
	}
}

func (s *Service) waitForBrainDocument(
	ctx context.Context,
	graph upstream.AsyncGraphStore,
	documentID string,
	localDocumentID string,
	revision string,
	state upstream.GraphDocumentState,
) (upstream.GraphDocumentState, error) {
	for state.Status == upstream.GraphDocumentPending {
		if err := s.ensureDocumentSyncCurrent(ctx, localDocumentID, revision); err != nil {
			return upstream.GraphDocumentState{}, err
		}
		if err := waitForContext(ctx, s.brainPollInterval); err != nil {
			return upstream.GraphDocumentState{}, brainDeadlineError(ctx, "wait for document extraction", nil)
		}
		var err error
		state, err = s.fetchBrainDocumentState(ctx, graph, documentID, localDocumentID, revision)
		if err != nil {
			return upstream.GraphDocumentState{}, err
		}
	}
	return state, nil
}

func (s *Service) retryBrainDocument(
	ctx context.Context,
	graph upstream.AsyncGraphStore,
	documentID, retryKey string,
) error {
	for {
		err := graph.RetryDocument(ctx, documentID, retryKey)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return brainDeadlineError(ctx, "retry document extraction", err)
		}
		if !retryableUpstreamError(err) {
			return err
		}
		if waitErr := waitForContext(ctx, brainRetryDelay(err, s.brainPollInterval)); waitErr != nil {
			return brainDeadlineError(ctx, "retry document extraction", err)
		}
	}
}

func brainDeadlineError(ctx context.Context, operation string, cause error) error {
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ctx.Err()
	}
	if cause != nil {
		return errors.Join(cause, ctx.Err())
	}
	return &upstream.DependencyError{
		Dependency: "brain", Operation: operation, Retryable: true, Cause: ctx.Err(),
	}
}

func (s *Service) ensureDocumentSyncCurrent(ctx context.Context, documentID, revision string) error {
	doc, err := s.store.GetDocument(ctx, documentID, false)
	if errors.Is(err, store.ErrNotFound) {
		return store.ErrDocumentSyncSuperseded
	}
	if err != nil {
		return err
	}
	if doc.Revision != revision {
		return store.ErrDocumentSyncSuperseded
	}
	return nil
}

func retryableUpstreamError(err error) bool {
	var dependency *upstream.DependencyError
	return errors.As(err, &dependency) &&
		(dependency.Retryable || dependency.Status == 429 || dependency.Status >= 500)
}

func brainRetryDelay(err error, fallback time.Duration) time.Duration {
	var dependency *upstream.DependencyError
	if errors.As(err, &dependency) && dependency.RetryAfter > 0 {
		return dependency.RetryAfter
	}
	var retryAfter retryAfterProvider
	if errors.As(err, &retryAfter) {
		if delay := retryAfter.RetryAfter(); delay > 0 {
			return delay
		}
	}
	if fallback > 0 {
		return fallback
	}
	return defaultBrainPollInterval
}

func waitForContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func decodeRenameDocumentCheckpoints(payload []byte) (renameDocumentCheckpointPayload, error) {
	var checkpoints renameDocumentCheckpointPayload
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &checkpoints); err != nil {
			return renameDocumentCheckpointPayload{}, fmt.Errorf("decode rename document checkpoints: %w", err)
		}
	}
	if checkpoints.BrainDocumentIDs == nil {
		checkpoints.BrainDocumentIDs = map[string]string{}
	}
	return checkpoints, nil
}

func renameDocumentCheckpointKey(documentID, revision string) string {
	return documentID + "@" + revision
}

func (s *Service) persistRenameBrainDocumentID(
	ctx context.Context,
	jobID string,
	checkpoints *renameDocumentCheckpointPayload,
	documentID, revision, brainDocumentID string,
) error {
	if checkpoints.BrainDocumentIDs == nil {
		checkpoints.BrainDocumentIDs = map[string]string{}
	}
	checkpoints.BrainDocumentIDs[renameDocumentCheckpointKey(documentID, revision)] = brainDocumentID
	if err := s.store.MergeJobPayload(ctx, jobID, map[string]any{
		"rename_brain_document_ids": checkpoints.BrainDocumentIDs,
	}); err != nil {
		return fmt.Errorf("persist rename Brain document ID: %w", err)
	}
	return nil
}

func terminalBrainDocumentError(state upstream.GraphDocumentState) error {
	var failed []string
	for _, run := range state.Runs {
		if run.Status == "failed" {
			if run.IndexerID == "" {
				failed = append(failed, "unknown indexer")
			} else {
				failed = append(failed, run.IndexerID)
			}
		}
	}
	if len(failed) == 0 {
		return fmt.Errorf("Brain document extraction finished in %s state", state.Status)
	}
	return fmt.Errorf("Brain document extraction finished in %s state; failed indexers: %s",
		state.Status, strings.Join(failed, ", "))
}
