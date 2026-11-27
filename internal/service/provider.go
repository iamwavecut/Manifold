package service

import (
	"context"
	"errors"
	"time"

	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/problem"
	"github.com/iamwavecut/Manifold/internal/store"
)

// The model provider can refuse extraction for reasons no retry of the same
// request changes: the account has no credit, or its key stopped working.
// Manifold names that cause, shows it in status, and retries the blocked
// documents itself once the provider answers again.

const (
	providerBilling     = "billing"
	providerCreditsCode = "provider_credits_exhausted"
	providerRetryAfter  = "15m"
)

type providerRefusedError struct {
	class string
	cause error
}

func (e *providerRefusedError) Error() string {
	return "model provider refused (" + e.class + "): " + e.cause.Error()
}

func (e *providerRefusedError) Unwrap() error { return e.cause }

func providerBlockedCode(class string) string {
	if class == providerBilling {
		return providerCreditsCode
	}
	return "dependency_unavailable"
}

func (s *Service) providerProblem(ctx context.Context, job model.Job, class string) *problem.Error {
	title := "Dependency unavailable"
	detail := "The model provider refused document extraction."
	first := "Check the model provider account and key the server uses."
	if class == providerBilling {
		title = "Model provider credit is exhausted"
		detail = "The model provider refused document extraction because its account or key has no credit."
		first = "Add credit to the model provider account the server uses."
	}
	p := problem.New(ctx, s.publicURL, 503, providerBlockedCode(class), title, detail,
		problem.Remediation{
			Summary: "Restore the model provider. Manifold then retries blocked documents itself.",
			Steps: []string{
				first,
				"Watch pipeline.blocked_documents in GET /api/v1/status.",
				"POST /api/v1/jobs/" + job.ID + "/retry to retry this document at once.",
			},
		})
	p.Retryable = true
	p.RetryAfter = providerRetryAfter
	p.Job = "/api/v1/jobs/" + job.ID
	return p
}

// RecoverProviderBlocked retries document syncs the model provider refused.
// While nothing has succeeded since the last refusal it retries only the
// oldest one, as a probe; after a success it retries all of them.
func (s *Service) RecoverProviderBlocked(ctx context.Context) (int, error) {
	blocked, err := s.store.ProviderBlockedSyncs(ctx)
	if err != nil || len(blocked) == 0 {
		return 0, err
	}
	succeeded, err := s.store.LatestSyncSuccess(ctx)
	if err != nil {
		return 0, err
	}
	targets := blocked[:1]
	if succeeded.After(blocked[len(blocked)-1].BlockedAt) {
		targets = blocked
	}
	retried := 0
	for _, target := range targets {
		err := s.store.RetryJob(ctx, target.JobID)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			return retried, err
		}
		retried++
	}
	return retried, nil
}

// RunProviderRecovery calls RecoverProviderBlocked until ctx ends. A zero or
// negative interval turns automatic recovery off.
func (s *Service) RunProviderRecovery(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		retried, err := s.RecoverProviderBlocked(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Error("provider recovery failed", "error", err)
		}
		if retried > 0 {
			s.logger.Info("retrying syncs the model provider refused", "jobs", retried)
		}
	}
}
