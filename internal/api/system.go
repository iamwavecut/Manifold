package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iamwavecut/Manifold/internal/auth"
	"github.com/iamwavecut/Manifold/internal/problem"
)

func (a *API) registerSystem() {
	huma.Register(a.huma, huma.Operation{
		OperationID: "health", Method: http.MethodGet, Path: "/health",
		Summary: "Liveness probe", Tags: []string{"System"}, Security: []map[string][]string{},
	}, func(ctx context.Context, _ *struct{}) (*body[map[string]string], error) {
		return &body[map[string]string]{Body: map[string]string{"status": "ok"}}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "ready", Method: http.MethodGet, Path: "/ready",
		Summary: "Readiness probe", Tags: []string{"System"}, Security: []map[string][]string{},
		Errors: []int{503},
	}, func(ctx context.Context, _ *struct{}) (*body[map[string]string], error) {
		if err := a.service.Store().Ping(ctx); err != nil {
			return nil, problem.New(ctx, a.config.PublicURL, http.StatusServiceUnavailable,
				"storage_unavailable", "Storage unavailable", "The SQLite control plane is not ready.",
				problem.Remediation{
					Summary: "Restore write access to the Manifold data volume and retry.",
					Steps:   []string{"Check the volume mount and available disk space.", "Restart Manifold after fixing storage."},
				})
		}
		return &body[map[string]string]{Body: map[string]string{"status": "ready"}}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "get-status", Method: http.MethodGet, Path: "/api/v1/status",
		Summary: "Inspect service and dependency status", Tags: []string{"System"}, Errors: commonErrors(),
	}, func(ctx context.Context, _ *struct{}) (*body[any], error) {
		if _, err := a.require(ctx, auth.ReadDocuments); err != nil {
			return nil, err
		}
		status, err := a.service.Status(ctx)
		if err != nil {
			return nil, err
		}
		return &body[any]{Body: status}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "get-metrics", Method: http.MethodGet, Path: "/api/v1/metrics",
		Summary: "Read operational counters", Tags: []string{"System"}, Errors: commonErrors(),
	}, func(ctx context.Context, _ *struct{}) (*body[map[string]int], error) {
		if _, err := a.require(ctx, auth.Admin); err != nil {
			return nil, err
		}
		metrics, err := a.service.Store().Metrics(ctx)
		if err != nil {
			return nil, err
		}
		return &body[map[string]int]{Body: metrics}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "get-error-documentation", Method: http.MethodGet, Path: "/errors/{code}",
		Summary: "Read semantic error documentation", Tags: []string{"Errors"}, Security: []map[string][]string{},
		Errors: []int{404},
	}, func(ctx context.Context, input *struct {
		Code string `path:"code" pattern:"^[a-z][a-z0-9_]+$"`
	}) (*body[map[string]any], error) {
		doc, ok := semanticErrorDocs[input.Code]
		if !ok {
			return nil, problem.New(ctx, a.config.PublicURL, http.StatusNotFound, "error_code_not_found",
				"Error code not found", fmt.Sprintf("No public semantic error named %q exists.", input.Code),
				problem.Remediation{
					Summary: "Use a code returned by the API.",
					Steps:   []string{"Inspect the code field of the original application/problem+json response."},
				})
		}
		return &body[map[string]any]{Body: map[string]any{
			"code": input.Code, "summary": doc.Summary, "steps": doc.Steps,
		}}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "get-error-remediation", Method: http.MethodGet, Path: "/docs/errors/{code}",
		Summary: "Read semantic error remediation", Tags: []string{"Errors"}, Security: []map[string][]string{},
		Errors: []int{404},
	}, func(ctx context.Context, input *struct {
		Code string `path:"code" pattern:"^[a-z][a-z0-9_]+$"`
	}) (*body[map[string]any], error) {
		doc, ok := semanticErrorDocs[input.Code]
		if !ok {
			return nil, problem.New(ctx, a.config.PublicURL, http.StatusNotFound, "error_code_not_found",
				"Error code not found", fmt.Sprintf("No public semantic error named %q exists.", input.Code),
				problem.Remediation{
					Summary: "Use a code returned by the API.",
					Steps:   []string{"Inspect the code field of the original application/problem+json response."},
				})
		}
		return &body[map[string]any]{Body: map[string]any{
			"code": input.Code, "summary": doc.Summary, "steps": doc.Steps,
		}}, nil
	})
}

var semanticErrorDocs = map[string]problem.Remediation{
	"validation_failed": {
		Summary: "Correct every violation before retrying.",
		Steps:   []string{"Use each JSON Pointer to locate the invalid value.", "Match the expected constraint shown in the violation."},
	},
	"invalid_request": {
		Summary: "Correct the request syntax or content type.",
		Steps:   []string{"Send valid JSON.", "Use Content-Type: application/json for JSON bodies."},
	},
	"invalid_api_key": {
		Summary: "Send a valid Manifold API key.",
		Steps:   []string{"Use Authorization: Bearer <key>.", "Ask an administrator to replace a revoked key."},
	},
	"missing_capability": {
		Summary: "Use a key with all required capabilities.",
		Steps:   []string{"Inspect required_capabilities.", "Ask an administrator to create a suitable key."},
	},
	"resource_not_found": {
		Summary: "Use an existing canonical slug or XID.",
		Steps:   []string{"List the relevant resource type.", "Check for a recent rename."},
	},
	"slug_taken": {
		Summary: "Choose the suggested available slug or another unused slug.",
		Steps:   []string{"Read suggested_slug.", "Retry with that slug and a new Idempotency-Key."},
	},
	"folder_path_conflict": {
		Summary: "Reuse the canonical folder path or choose a distinct slug for the new branch.",
		Steps:   []string{"Inspect meta.existing_path and the surrounding tree.", "Choose whether the knowledge belongs in the existing branch.", "Retry with a distinct segment only when the concepts are genuinely different."},
	},
	"etag_mismatch": {
		Summary: "Refresh the resource before retrying the update.",
		Steps:   []string{"GET the resource.", "Apply the intended change to the latest state.", "Send its ETag in If-Match."},
	},
	"idempotency_key_reused": {
		Summary: "Use the original key only for an exact replay.",
		Steps:   []string{"Keep the original request unchanged when retrying.", "Use a new Idempotency-Key for corrected or different mutation content."},
	},
	"api_key_secret_not_replayable": {
		Summary: "Use the original secret or replace the key if that one-time response was lost.",
		Steps:   []string{"List API-key records and confirm that the requested key exists.", "Revoke it if the plaintext secret was lost.", "Create a replacement with a new Idempotency-Key."},
	},
	"stale_rename_plan": {
		Summary: "Create a new rename preview.",
		Steps:   []string{"POST the same operations to /api/v1/rename-plans.", "Review the new preview.", "Apply the new plan XID."},
	},
	"unrewritable_reference": {
		Summary: "Remove or convert every blocking binary reference.",
		Steps:   []string{"Inspect blockers.", "Replace the binary asset with a rewritable text source or edit it externally.", "Create a new preview."},
	},
	"dependency_unavailable": {
		Summary: "Restore the named dependency and retry.",
		Steps:   []string{"Inspect /api/v1/status.", "Fix provider or network configuration.", "Retry the failed job."},
	},
	"invalid_state_transition": {
		Summary: "Perform an action allowed by the current state.",
		Steps:   []string{"Inspect current_state and allowed_states.", "Wait for an active job or select an allowed transition."},
	},
	"state_conflict": {
		Summary: "Refresh the resource and retry without conflicting with its current state.",
		Steps:   []string{"Read the latest resource.", "Choose a non-conflicting operation.", "Retry with a new Idempotency-Key."},
	},
	"csrf_failed": {
		Summary: "Use the CSRF token paired with the current browser session.",
		Steps:   []string{"Create a new UI session if needed.", "Send its csrf_token in X-CSRF-Token."},
	},
	"storage_unavailable": {
		Summary: "Restore access to the SQLite data volume before retrying.",
		Steps:   []string{"Check /ready.", "Check volume permissions and free space.", "Retry after readiness is restored."},
	},
	"job_failed": {
		Summary: "Inspect the job error, correct the cause, and retry the job.",
		Steps:   []string{"GET the job.", "Follow error.remediation.", "POST the retry endpoint after correcting the cause."},
	},
	"cannot_revoke_current_key": {
		Summary: "Authenticate with another administrator key.",
		Steps:   []string{"Create a second admin key.", "Use it to revoke the original key."},
	},
	"internal_error": {
		Summary: "Report request_id to the operator before repeating a mutation.",
		Steps:   []string{"Inspect the related job if one exists.", "Search structured logs by request_id.", "Retry only when the mutation outcome is known."},
	},
	"request_failed": {
		Summary: "Inspect request_id and correct the reported server-side cause.",
		Steps:   []string{"Search structured logs by request_id.", "Retry only when remediation is complete."},
	},
	"error_code_not_found": {
		Summary: "Use a code returned by a Manifold error response.",
		Steps:   []string{"Read the original response code field.", "Open documentation_url from that response."},
	},
}

func normalizeCapabilityList(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
