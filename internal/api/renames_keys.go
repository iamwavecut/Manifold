package api

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iamwavecut/Manifold/internal/auth"
	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/problem"
	"github.com/iamwavecut/Manifold/internal/store"
)

func (a *API) registerRenames() {
	huma.Register(a.huma, huma.Operation{
		OperationID: "create-rename-plan", Method: http.MethodPost, Path: "/api/v1/rename-plans",
		Summary: "Preview an atomic cascading rename", Tags: []string{"Renames"},
		DefaultStatus: http.StatusCreated, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		MutationHeaders
		Body struct {
			Operations []model.RenameOperation `json:"operations" minItems:"1" maxItems:"100"`
		}
	}) (*accepted[model.RenamePlan], error) {
		if _, err := a.require(ctx, auth.WriteDocuments, auth.WriteGraph); err != nil {
			return nil, err
		}
		plan, err := a.service.CreateRenamePlan(ctx, input.Body.Operations)
		if err != nil {
			return nil, a.operationError(ctx, "rename-plans", "", err)
		}
		return &accepted[model.RenamePlan]{
			Location: "/api/v1/rename-plans/" + plan.ID, Body: plan,
		}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "get-rename-plan", Method: http.MethodGet, Path: "/api/v1/rename-plans/{id}",
		Summary: "Read a rename preview or result", Tags: []string{"Renames"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		ID string `path:"id" pattern:"^[0-9a-v]{20}$"`
	}) (*body[model.RenamePlan], error) {
		if _, err := a.require(ctx, auth.ReadDocuments, auth.ReadGraph); err != nil {
			return nil, err
		}
		plan, err := a.service.Store().GetRenamePlan(ctx, input.ID)
		if err != nil {
			return nil, a.storeError(ctx, "rename-plans", input.ID, err)
		}
		return &body[model.RenamePlan]{Body: plan}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "apply-rename-plan", Method: http.MethodPost, Path: "/api/v1/rename-plans/{id}/apply",
		Summary: "Apply a reviewed cascading rename", Tags: []string{"Renames"},
		DefaultStatus: http.StatusAccepted, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		MutationHeaders
		ID string `path:"id" pattern:"^[0-9a-v]{20}$"`
	}) (*accepted[model.Job], error) {
		if _, err := a.require(ctx, auth.WriteDocuments, auth.WriteGraph); err != nil {
			return nil, err
		}
		plan, err := a.service.Store().GetRenamePlan(ctx, input.ID)
		if err != nil {
			return nil, a.storeError(ctx, "rename-plans", input.ID, err)
		}
		if len(plan.Preview.Blockers) > 0 {
			p := problem.New(ctx, a.config.PublicURL, http.StatusConflict, "unrewritable_reference",
				"Rename contains unrewritable references",
				"At least one active binary document contains a matching slug that Manifold cannot safely rewrite.",
				problem.Remediation{
					Summary: "Remove or externally rewrite every blocker, then create a new preview.",
					Steps:   []string{"Inspect blockers in this response.", "Update or convert the listed documents.", "Create and review a new rename plan."},
				})
			p.Blockers = plan.Preview.Blockers
			return nil, p
		}
		if len(plan.Preview.Conflicts) > 0 {
			p := problem.New(ctx, a.config.PublicURL, http.StatusConflict, "state_conflict",
				"Rename targets are occupied",
				"At least one target slug is active and is not another source in this batch.",
				problem.Remediation{
					Summary: "Choose free target slugs or include a complete swap/cycle in one batch.",
					Steps:   []string{"Inspect meta.conflicts.", "Create a new preview with non-conflicting targets."},
				})
			p.Meta = map[string]any{"conflicts": plan.Preview.Conflicts}
			return nil, p
		}
		version, err := a.service.Store().StateVersion(ctx)
		if err != nil {
			return nil, err
		}
		if version != plan.Preview.StateVersion {
			return nil, problem.New(ctx, a.config.PublicURL, http.StatusConflict, "stale_rename_plan",
				"Rename preview is stale", "Knowledge state changed after this preview was created.",
				problem.Remediation{
					Summary: "Create and review a fresh rename plan.",
					Steps:   []string{"POST the same operations to /api/v1/rename-plans.", "Review the new replacements and blockers.", "Apply the new plan XID."},
				})
		}
		job, err := a.service.ApplyRenamePlan(ctx, input.ID)
		if err != nil {
			return nil, a.operationError(ctx, "rename-plans", input.ID, err)
		}
		return &accepted[model.Job]{Location: "/api/v1/jobs/" + job.ID, Body: job}, nil
	})
}

type createKeyResponse struct {
	Key    model.APIKey `json:"key"`
	Secret string       `json:"secret" doc:"Shown once. Store it securely; Manifold only persists an Argon2id hash."`
}

func (a *API) registerKeysAndSessions() {
	huma.Register(a.huma, huma.Operation{
		OperationID: "list-api-keys", Method: http.MethodGet, Path: "/api/v1/api-keys",
		Summary: "List API-key records", Tags: []string{"Authentication"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		Cursor string `query:"cursor" pattern:"^c[0-9a-z]+$"`
		Limit  int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
	}) (*body[page[model.APIKey]], error) {
		if _, err := a.require(ctx, auth.Admin); err != nil {
			return nil, err
		}
		keys, err := a.service.Store().ListAPIKeys(ctx)
		if err != nil {
			return nil, err
		}
		return &body[page[model.APIKey]]{Body: paginate(keys, input.Cursor, input.Limit)}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "create-api-key", Method: http.MethodPost, Path: "/api/v1/api-keys",
		Summary: "Create an API key", Tags: []string{"Authentication"}, DefaultStatus: http.StatusCreated,
		Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		MutationHeaders
		Body struct {
			ID           string   `json:"id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$" maxLength:"63"`
			Capabilities []string `json:"capabilities" minItems:"1" maxItems:"7"`
		}
	}) (*accepted[createKeyResponse], error) {
		if _, err := a.require(ctx, auth.Admin); err != nil {
			return nil, err
		}
		capabilities := normalizeCapabilityList(input.Body.Capabilities)
		for index, capability := range capabilities {
			if !slices.Contains(auth.AllCapabilities, capability) {
				return nil, problem.Validation(ctx, a.config.PublicURL, problem.Violation{
					Pointer: "/body/capabilities/" + strconv.Itoa(index), Code: "unknown_capability",
					Message:  "Capability is not part of the Manifold capability registry.",
					Expected: auth.AllCapabilities, Received: capability,
				})
			}
		}
		available, err := a.service.Store().SlugAvailable(ctx, "api_key", input.Body.ID)
		if err != nil {
			return nil, err
		}
		if !available {
			return nil, a.slugTaken(ctx, "api_key", input.Body.ID)
		}
		secret, err := identity.Secret(32)
		if err != nil {
			return nil, err
		}
		hash, err := auth.HashSecret(secret)
		if err != nil {
			return nil, err
		}
		key, err := a.service.Store().CreateAPIKey(ctx, input.Body.ID, hash, capabilities)
		if err != nil {
			return nil, a.storeError(ctx, "api-keys", input.Body.ID, err)
		}
		response := createKeyResponse{Key: key, Secret: secret}
		return &accepted[createKeyResponse]{
			Location: "/api/v1/api-keys/" + key.ID, Body: response,
		}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "delete-api-key", Method: http.MethodDelete, Path: "/api/v1/api-keys/{id}",
		Summary: "Revoke an API key", Tags: []string{"Authentication"},
		DefaultStatus: http.StatusNoContent, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		MutationHeaders
		ID string `path:"id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
	}) (*struct{}, error) {
		principal, err := a.require(ctx, auth.Admin)
		if err != nil {
			return nil, err
		}
		if principal.KeyID == input.ID {
			return nil, problem.New(ctx, a.config.PublicURL, http.StatusConflict, "cannot_revoke_current_key",
				"Current key cannot revoke itself", "Revoking the credential used for this request could lock out the deployment.",
				problem.Remediation{
					Summary: "Use a different administrator key to revoke this key.",
					Steps:   []string{"Create another admin key.", "Authenticate with that key.", "Retry the revocation."},
				})
		}
		if err := a.service.Store().DeleteAPIKey(ctx, input.ID); err != nil {
			return nil, a.storeError(ctx, "api-keys", input.ID, err)
		}
		return nil, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "create-ui-session", Method: http.MethodPost, Path: "/api/v1/ui/session",
		Summary: "Exchange an API key for a browser session", Tags: []string{"Authentication"},
		Security: []map[string][]string{}, Errors: []int{401, 422},
	}, func(ctx context.Context, input *struct {
		Body struct {
			APIKey string `json:"api_key" minLength:"16" maxLength:"512"`
		}
	}) (*struct {
		SetCookie string `header:"Set-Cookie"`
		Body      struct {
			CSRFToken    string   `json:"csrf_token"`
			Capabilities []string `json:"capabilities"`
			ExpiresAt    string   `json:"expires_at"`
		}
	}, error) {
		principal, err := a.auth.Authenticate(ctx, input.Body.APIKey)
		if err != nil {
			return nil, problem.New(ctx, a.config.PublicURL, http.StatusUnauthorized, "invalid_api_key",
				"Invalid API key", "The supplied API key does not match an active Manifold key.",
				problem.Remediation{
					Summary: "Use an active API key.",
					Steps:   []string{"Check for accidental whitespace.", "Ask an administrator to issue a replacement key."},
				})
		}
		sessionToken, err := identity.Secret(32)
		if err != nil {
			return nil, err
		}
		csrfToken, err := identity.Secret(32)
		if err != nil {
			return nil, err
		}
		capabilities := make([]string, 0, len(principal.Capabilities))
		for capability := range principal.Capabilities {
			capabilities = append(capabilities, capability)
		}
		slices.Sort(capabilities)
		expires := time.Now().UTC().Add(12 * time.Hour)
		if err := a.service.Store().CreateSession(ctx, sessionToken, csrfToken, principal.KeyID, capabilities, expires); err != nil {
			return nil, err
		}
		output := &struct {
			SetCookie string `header:"Set-Cookie"`
			Body      struct {
				CSRFToken    string   `json:"csrf_token"`
				Capabilities []string `json:"capabilities"`
				ExpiresAt    string   `json:"expires_at"`
			}
		}{SetCookie: (&http.Cookie{
			Name: "manifold_session", Value: sessionToken, Path: "/", HttpOnly: true,
			SameSite: http.SameSiteStrictMode, Secure: strings.HasPrefix(a.config.PublicURL, "https://"),
			Expires: expires,
		}).String()}
		output.Body.CSRFToken = csrfToken
		output.Body.Capabilities = capabilities
		output.Body.ExpiresAt = expires.Format(time.RFC3339)
		return output, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "get-ui-session", Method: http.MethodGet, Path: "/api/v1/ui/session",
		Summary: "Inspect the active browser session", Tags: []string{"Authentication"}, Errors: commonErrors(),
	}, func(ctx context.Context, _ *struct{}) (*body[map[string]any], error) {
		principal, err := a.require(ctx)
		if err != nil {
			return nil, err
		}
		capabilities := make([]string, 0, len(principal.Capabilities))
		for capability := range principal.Capabilities {
			capabilities = append(capabilities, capability)
		}
		slices.Sort(capabilities)
		return &body[map[string]any]{Body: map[string]any{
			"api_key_id": principal.KeyID, "capabilities": capabilities,
		}}, nil
	})
}

var _ = store.ErrNotFound
