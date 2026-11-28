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
	Create      bool     `json:"create"`
	FolderPaths []string `json:"folder_paths"`
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
	var refused *providerRefusedError
	if job.Kind == "document.sync" && errors.As(err, &refused) {
		semantic = s.providerProblem(errorContext, job, refused.class)
		if mergeErr := s.store.MergeJobPayload(ctx, job.ID, map[string]any{"provider_refusal": refused.class}); mergeErr != nil {
			err = errors.Join(err, fmt.Errorf("record provider refusal: %w", mergeErr))
		}
	}
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
		if err := s.writeDocument(ctx, revision.OVURI, revision.Content, payload.Create); err != nil {
			if !payload.Create {
				return err
			}
			exists, existsErr := s.documents.Exists(ctx, revision.OVURI)
			if existsErr != nil || !exists {
				return err
			}
			if replaceErr := s.writeDocument(ctx, revision.OVURI, revision.Content, false); replaceErr != nil {
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
	extractionCtx, cancel := context.WithTimeout(ctx, s.extractionTimeout)
	defer cancel()
	hold := s.holdGraphPhase()
	defer hold.release()
	ingest, err := s.graph.IngestDocument(extractionCtx, graphDocument)
	if class := upstream.ProviderRefusal(err); class != "" {
		return &providerRefusedError{class: class, cause: err}
	}
	if err != nil {
		return err
	}
	if err := s.materializeGraphDocument(extractionCtx, doc, ingest, hold); err != nil {
		return err
	}
	return s.store.SetDocumentSyncForRevision(ctx, doc.ID, revisionName, model.JobReady, ingest.DocumentID, snapshot)
}

func retryableUpstreamError(err error) bool {
	var dependency *upstream.DependencyError
	return errors.As(err, &dependency) &&
		(dependency.Retryable || dependency.Status == 429 || dependency.Status >= 500)
}

func upstreamRetryDelay(err error, fallback time.Duration) time.Duration {
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
	return time.Second
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
