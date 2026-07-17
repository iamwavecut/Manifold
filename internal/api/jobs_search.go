package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iamwavecut/Manifold/internal/auth"
	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/problem"
	"github.com/iamwavecut/Manifold/internal/service"
	"github.com/iamwavecut/Manifold/internal/store"
)

func (a *API) registerJobs() {
	huma.Register(a.huma, huma.Operation{
		OperationID: "list-jobs", Method: http.MethodGet, Path: "/api/v1/jobs",
		Summary: "List background jobs", Tags: []string{"Jobs"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		Cursor string `query:"cursor" pattern:"^c[0-9a-z]+$"`
		Limit  int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
	}) (*body[page[model.Job]], error) {
		if _, err := a.require(ctx, auth.ReadDocuments); err != nil {
			return nil, err
		}
		jobs, err := a.service.Store().ListJobs(ctx, 10_000)
		if err != nil {
			return nil, err
		}
		return &body[page[model.Job]]{Body: paginate(jobs, input.Cursor, input.Limit)}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "get-job", Method: http.MethodGet, Path: "/api/v1/jobs/{id}",
		Summary: "Read job status and remediation", Tags: []string{"Jobs"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		ID string `path:"id" pattern:"^[0-9a-v]{20}$"`
	}) (*body[model.Job], error) {
		if _, err := a.require(ctx, auth.ReadDocuments); err != nil {
			return nil, err
		}
		job, err := a.service.Store().GetJob(ctx, input.ID)
		if err != nil {
			return nil, a.storeError(ctx, "jobs", input.ID, err)
		}
		return &body[model.Job]{Body: job}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "retry-job", Method: http.MethodPost, Path: "/api/v1/jobs/{id}/retry",
		Summary: "Retry a failed or partially ready job", Tags: []string{"Jobs"},
		DefaultStatus: http.StatusAccepted, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		MutationHeaders
		ID string `path:"id" pattern:"^[0-9a-v]{20}$"`
	}) (*accepted[model.Job], error) {
		if _, err := a.require(ctx, auth.WriteDocuments); err != nil {
			return nil, err
		}
		job, err := a.service.Store().GetJob(ctx, input.ID)
		if err != nil {
			return nil, a.storeError(ctx, "jobs", input.ID, err)
		}
		if job.Status != model.JobFailed && job.Status != model.JobPartiallyReady {
			p := problem.New(ctx, a.config.PublicURL, http.StatusConflict, "invalid_state_transition",
				"Job cannot be retried", "Only failed or partially_ready jobs can transition back to accepted.",
				problem.Remediation{
					Summary: "Wait for the active job or retry a terminal failed job.",
					Steps:   []string{"Inspect current_state and allowed_states.", "Retry only after the job reaches failed or partially_ready."},
				})
			p.CurrentState = string(job.Status)
			p.AllowedStates = []string{string(model.JobFailed), string(model.JobPartiallyReady)}
			p.Job = "/api/v1/jobs/" + job.ID
			return nil, p
		}
		if err := a.service.Store().RetryJob(ctx, input.ID); err != nil {
			return nil, a.storeError(ctx, "jobs", input.ID, err)
		}
		job, _ = a.service.Store().GetJob(ctx, input.ID)
		return &accepted[model.Job]{Location: "/api/v1/jobs/" + job.ID, Body: job}, nil
	})
}

type searchInput struct {
	Body struct {
		Query          string           `json:"query" minLength:"1" maxLength:"8000"`
		Mode           model.SearchMode `json:"mode,omitempty" enum:"hybrid,semantic,lexical,graph" default:"hybrid"`
		Scope          string           `json:"scope,omitempty" maxLength:"512"`
		Limit          int              `json:"limit,omitempty" minimum:"1" maximum:"100" default:"10"`
		IncludeHistory bool             `json:"include_history,omitempty"`
	}
}

func (a *API) registerSearch() {
	huma.Register(a.huma, huma.Operation{
		OperationID: "search-knowledge", Method: http.MethodPost, Path: "/api/v1/search",
		Summary: "Search documents and graph knowledge", Tags: []string{"Retrieval"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *searchInput) (*body[service.SearchResponse], error) {
		if _, err := a.require(ctx, auth.Search); err != nil {
			return nil, err
		}
		mode := input.Body.Mode
		if mode == "" {
			mode = model.SearchHybrid
		}
		result, err := a.service.Search(ctx, service.SearchRequest{
			Query: input.Body.Query, Mode: mode, Scope: input.Body.Scope,
			Limit: input.Body.Limit, IncludeHistory: input.Body.IncludeHistory,
		})
		if err != nil {
			return nil, err
		}
		return &body[service.SearchResponse]{Body: result}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "build-context", Method: http.MethodPost, Path: "/api/v1/context",
		Summary: "Build a token-budgeted evidence pack", Tags: []string{"Retrieval"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		Body struct {
			Query          string   `json:"query" minLength:"1" maxLength:"8000"`
			TokenBudget    int      `json:"token_budget" minimum:"50" maximum:"50000" default:"4000"`
			Scope          string   `json:"scope,omitempty" maxLength:"512"`
			GraphDepth     int      `json:"graph_depth,omitempty" minimum:"0" maximum:"5" default:"2"`
			IncludeHistory bool     `json:"include_history,omitempty"`
			SourceTypes    []string `json:"source_types,omitempty"`
		}
	}) (*body[map[string]any], error) {
		if _, err := a.require(ctx, auth.Search); err != nil {
			return nil, err
		}
		pack, degraded, err := a.service.Context(ctx, service.SearchRequest{
			Query: input.Body.Query, Mode: model.SearchHybrid, Scope: input.Body.Scope,
			Limit: 100, IncludeHistory: input.Body.IncludeHistory,
		}, input.Body.TokenBudget)
		if err != nil {
			return nil, err
		}
		return &body[map[string]any]{Body: map[string]any{
			"context": pack, "degraded_dependencies": degraded,
		}}, nil
	})
}

var _ = store.ErrNotFound
