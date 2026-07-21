package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/iamwavecut/Manifold/internal/auth"
	"github.com/iamwavecut/Manifold/internal/config"
	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/problem"
	"github.com/iamwavecut/Manifold/internal/service"
	"github.com/iamwavecut/Manifold/internal/store"
)

var protectedSecurity = []map[string][]string{{"bearerAuth": {}}}

type API struct {
	Handler http.Handler
	Spec    *huma.OpenAPI

	huma    huma.API
	service *service.Service
	auth    *auth.Manager
	config  config.Config
	logger  *slog.Logger
}

func New(svc *service.Service, manager *auth.Manager, cfg config.Config, logger *slog.Logger, web http.Handler) *API {
	mux := http.NewServeMux()
	humaConfig := huma.DefaultConfig("Manifold API", "0.1.0")
	humaConfig.Info.Description = "A self-hosted knowledge and memory layer for AI agents."
	humaConfig.OpenAPIPath = "/openapi"
	humaConfig.DocsPath = "/docs/api"
	humaConfig.DocsRenderer = huma.DocsRendererScalar
	humaConfig.RejectUnknownQueryParameters = true
	humaConfig.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearerAuth": {
			Type: "http", Scheme: "bearer", BearerFormat: "Manifold API key",
			Description: "A Manifold API key with the capabilities required by the operation.",
		},
	}
	humaConfig.Security = protectedSecurity
	humaAPI := humago.New(mux, humaConfig)
	a := &API{huma: humaAPI, service: svc, auth: manager, config: cfg, logger: logger}
	a.installSemanticErrors()
	a.registerSystem()
	a.registerFolders()
	a.registerDocuments()
	a.registerJobs()
	a.registerSearch()
	a.registerGraph()
	a.registerRenames()
	a.registerKeysAndSessions()
	a.installOpenAPIExamples()
	if web != nil {
		mux.Handle("/", web)
	}
	a.Handler = a.middleware(mux)
	a.Spec = humaAPI.OpenAPI()
	return a
}

func (a *API) installOpenAPIExamples() {
	schema := a.huma.OpenAPI().Components.Schemas.Map()["Error"]
	if schema == nil {
		return
	}
	if property := schema.Properties["request_id"]; property != nil {
		property.Pattern = "^[0-9a-v]{20}$"
		property.Description = "XID used to correlate the response with structured logs."
	}
	if property := schema.Properties["code"]; property != nil {
		property.Description = "Stable machine-readable semantic error code. Never infer behavior from detail."
	}
	codes := make([]string, 0, len(semanticErrorDocs))
	for code := range semanticErrorDocs {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	for _, code := range codes {
		status := semanticErrorStatus(code)
		example := map[string]any{
			"type":       a.config.PublicURL + "/errors/" + code,
			"title":      strings.ReplaceAll(code, "_", " "),
			"status":     status,
			"detail":     "A safe request-specific explanation is returned here.",
			"code":       code,
			"request_id": "d5vqh9idc6b5u5sctq00",
			"retryable":  code == "dependency_unavailable" || code == "storage_unavailable",
			"remediation": map[string]any{
				"summary": semanticErrorDocs[code].Summary,
				"steps":   semanticErrorDocs[code].Steps,
			},
			"documentation_url": a.config.PublicURL + "/docs/errors/" + code,
		}
		switch code {
		case "validation_failed", "invalid_request":
			example["violations"] = []map[string]any{{
				"pointer": "/body/title", "code": "required",
				"message": "Title is required.", "expected": "a non-empty string, for example \"Agent memory\"",
			}}
		case "missing_capability":
			example["required_capabilities"] = []string{auth.WriteDocuments}
		case "resource_not_found":
			example["resource"] = "/api/v1/documents/old-slug"
			example["meta"] = map[string]any{"resource_type": "document", "requested_slug": "old-slug", "renamed_to": "new-slug"}
		case "slug_taken":
			example["suggested_slug"] = "agent-memory-2"
		case "folder_path_conflict":
			example["meta"] = map[string]any{
				"segment": "runbooks", "existing_path": "shared/operations/runbooks",
				"requested_path": "tasks/incident-42/runbooks",
			}
		case "idempotency_key_reused":
			example["resource"] = "/api/v1/documents"
		case "api_key_secret_not_replayable":
			example["resource"] = "/api/v1/api-keys"
		case "etag_mismatch":
			example["current_etag"] = `"v3"`
			example["resource"] = "/api/v1/documents/agent-memory"
		case "invalid_state_transition":
			example["current_state"] = "ready"
			example["allowed_states"] = []string{"failed", "partially_ready"}
		case "unrewritable_reference":
			example["blockers"] = []map[string]string{{
				"resource": "documents/archive", "reason": "Binary content cannot be safely rewritten.", "location": "archive@r2",
			}}
		case "dependency_unavailable":
			example["retry_after"] = "30s"
		case "job_failed":
			example["job"] = "/api/v1/jobs/d5vqh9idc6b5u5sctq00"
		}
		schema.Examples = append(schema.Examples, example)
	}
}

func semanticErrorStatus(code string) int {
	switch code {
	case "invalid_request":
		return http.StatusBadRequest
	case "invalid_api_key":
		return http.StatusUnauthorized
	case "missing_capability", "csrf_failed":
		return http.StatusForbidden
	case "resource_not_found", "error_code_not_found":
		return http.StatusNotFound
	case "slug_taken", "folder_path_conflict", "etag_mismatch", "idempotency_key_reused", "state_conflict", "invalid_state_transition",
		"stale_rename_plan", "unrewritable_reference", "cannot_revoke_current_key", "api_key_secret_not_replayable":
		return http.StatusConflict
	case "validation_failed":
		return http.StatusUnprocessableEntity
	case "dependency_unavailable", "storage_unavailable":
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func (a *API) installSemanticErrors() {
	huma.NewErrorWithContext = func(ctx huma.Context, status int, message string, errs ...error) huma.StatusError {
		stdctx := problem.WithRequestID(context.Background(), ctx.Header("X-Request-ID"))
		violations := make([]problem.Violation, 0, len(errs))
		for _, err := range errs {
			if err == nil {
				continue
			}
			violation := problem.Violation{Code: "invalid_value", Message: err.Error()}
			if detailer, ok := err.(huma.ErrorDetailer); ok {
				detail := detailer.ErrorDetail()
				violation.Pointer = "/" + strings.ReplaceAll(detail.Location, ".", "/")
				violation.Received = detail.Value
			}
			violations = append(violations, violation)
		}
		if status >= 400 && status < 500 {
			p := problem.Validation(stdctx, a.config.PublicURL, violations...)
			if status == http.StatusBadRequest {
				p.Status = status
				p.Code = "invalid_request"
				p.Title = "Invalid request"
				p.Type = a.config.PublicURL + "/errors/invalid_request"
				p.DocumentationURL = a.config.PublicURL + "/docs/errors/invalid_request"
			}
			if message != "" {
				p.Detail = message
			}
			return p
		}
		return problem.New(stdctx, a.config.PublicURL, status, "request_failed", http.StatusText(status), message,
			problem.Remediation{Summary: "Use request_id to inspect the matching server log and retry when the cause is fixed."})
	}
	huma.NewError = func(status int, message string, errs ...error) huma.StatusError {
		ctx := problem.WithRequestID(context.Background(), "")
		p := problem.New(ctx, a.config.PublicURL, status, "request_failed", http.StatusText(status), message,
			problem.Remediation{Summary: "Inspect the request and correct the reported problem."})
		for _, err := range errs {
			if err != nil {
				p.Violations = append(p.Violations, problem.Violation{Code: "invalid_value", Message: err.Error()})
			}
		}
		return p
	}
}

func (a *API) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := identity.NewXID()
		w.Header().Set("X-Request-ID", requestID)
		r.Header.Set("X-Request-ID", requestID)
		ctx := problem.WithRequestID(r.Context(), requestID)

		principal, authenticated := a.authenticate(ctx, r)
		if authenticated {
			ctx = auth.WithPrincipal(ctx, principal)
			if principal.FromSession && isUnsafe(r.Method) && !validCSRF(r.Header.Get("X-CSRF-Token"), principal.CSRFHash) {
				p := problem.New(ctx, a.config.PublicURL, http.StatusForbidden, "csrf_failed", "CSRF validation failed",
					"The session is valid, but the mutation did not include its matching CSRF token.",
					problem.Remediation{
						Summary: "Read the CSRF token returned by the UI session endpoint and send it in X-CSRF-Token.",
						Steps:   []string{"Create a new UI session if the token was lost.", "Retry the request with the X-CSRF-Token header."},
					})
				writeProblem(w, p)
				return
			}
		}

		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if recovered := recover(); recovered != nil {
				p := problem.New(ctx, a.config.PublicURL, http.StatusInternalServerError, "internal_error",
					"Internal server error", "The server could not complete the request.",
					problem.Remediation{
						Summary: "Report request_id to the Manifold operator.",
						Steps:   []string{"Do not repeat a mutation until its job or idempotency status is known."},
					})
				writeProblem(recorder, p)
				a.logger.Error("request panic", "request_id", requestID, "panic", recovered)
			}
			a.logger.Info("request",
				"request_id", requestID, "method", r.Method, "path", r.URL.Path,
				"status", recorder.status, "duration_ms", time.Since(start).Milliseconds(),
				"api_key_id", principal.KeyID)
		}()

		idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		scopedKey := a.scopedIdempotencyKey(ctx, idempotencyKey)
		cacheable := isUnsafe(r.Method) && strings.HasPrefix(r.URL.Path, "/api/v1/") &&
			r.URL.Path != "/api/v1/ui/session" && len(idempotencyKey) >= 8 && scopedKey != ""
		requestHash := ""
		if cacheable {
			var err error
			requestHash, err = fingerprintRequest(r, a.config.MaxBodyBytes)
			if err != nil {
				p := problem.New(ctx, a.config.PublicURL, http.StatusRequestEntityTooLarge, "invalid_request",
					"Request body is too large", "The mutation body exceeds the configured Manifold request limit.",
					problem.Remediation{
						Summary: "Reduce or split the request before retrying.",
						Steps:   []string{"Check MANIFOLD_MAX_BODY_BYTES.", "Split a large import into smaller batches.", "Retry with a new Idempotency-Key."},
					})
				writeProblem(recorder, p)
				return
			}
			status, data, storedHash, err := a.service.Store().GetIdempotency(ctx, scopedKey, r.Method, r.URL.Path)
			if err == nil {
				if subtle.ConstantTimeCompare([]byte(requestHash), []byte(storedHash)) != 1 {
					p := problem.New(ctx, a.config.PublicURL, http.StatusConflict, "idempotency_key_reused",
						"Idempotency key was reused for a different request",
						"This API key already used the supplied Idempotency-Key for different mutation content.",
						problem.Remediation{
							Summary: "Keep the original key only for an exact replay; use a new key for changed intent.",
							Steps:   []string{"Do not change an in-flight request while retaining its Idempotency-Key.", "Generate a new Idempotency-Key for the corrected or different mutation."},
						})
					p.Resource = r.URL.Path
					writeProblem(recorder, p)
					return
				}
				if isOneTimeSecretMutation(r) {
					recorder.Header().Set("Idempotency-Replayed", "true")
					p := problem.New(ctx, a.config.PublicURL, http.StatusConflict, "api_key_secret_not_replayable",
						"API key secret cannot be replayed",
						"The original request created the API-key record, but Manifold returned its plaintext secret only once and did not persist it.",
						problem.Remediation{
							Summary: "Use the secret from the original response, or revoke the key and issue a replacement if that response was lost.",
							Steps: []string{
								"GET /api/v1/api-keys and confirm that the requested key record exists.",
								"If the original secret was stored safely, use it without repeating this request.",
								"If the secret was lost, revoke the key and create a replacement with a new Idempotency-Key.",
							},
						})
					p.Resource = r.URL.Path
					writeProblem(recorder, p)
					return
				}
				recorder.Header().Set("Content-Type", "application/json; charset=utf-8")
				recorder.Header().Set("Idempotency-Replayed", "true")
				recorder.WriteHeader(status)
				_, _ = recorder.Write(data)
				return
			}
			if !errors.Is(err, store.ErrNotFound) {
				p := problem.New(ctx, a.config.PublicURL, http.StatusServiceUnavailable, "storage_unavailable",
					"Storage unavailable", "The idempotency record could not be read safely.",
					problem.Remediation{
						Summary: "Do not repeat the mutation until SQLite is healthy.",
						Steps:   []string{"Check GET /ready.", "Restore the Manifold data volume.", "Retry with the same Idempotency-Key."},
					})
				writeProblem(recorder, p)
				return
			}
		}

		capture := &captureWriter{ResponseWriter: recorder}
		next.ServeHTTP(capture, r.WithContext(ctx))
		if cacheable && recorder.status >= 200 && recorder.status < 300 {
			responseBody := capture.body.Bytes()
			if isOneTimeSecretMutation(r) {
				responseBody = []byte{}
			}
			if err := a.service.Store().SaveIdempotency(
				ctx, scopedKey, r.Method, r.URL.Path, requestHash, recorder.status, responseBody,
			); err != nil {
				a.logger.Error("save idempotency response", "request_id", requestID, "error", err)
			}
		}
	})
}

func fingerprintRequest(r *http.Request, maxBytes int64) (string, error) {
	if r.Body == nil {
		sum := sha256.Sum256([]byte(r.Method + "\x00" + r.URL.RequestURI()))
		return hex.EncodeToString(sum[:]), nil
	}
	if maxBytes <= 0 {
		maxBytes = 5 << 20
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > maxBytes {
		return "", fmt.Errorf("request body exceeds %d bytes", maxBytes)
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	hash := sha256.New()
	_, _ = io.WriteString(hash, r.Method)
	_, _ = hash.Write([]byte{0})
	_, _ = io.WriteString(hash, r.URL.RequestURI())
	_, _ = hash.Write([]byte{0})
	_, _ = io.WriteString(hash, r.Header.Get("Content-Type"))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (a *API) authenticate(ctx context.Context, r *http.Request) (auth.Principal, bool) {
	if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
		principal, err := a.auth.Authenticate(ctx, strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")))
		return principal, err == nil
	}
	cookie, err := r.Cookie("manifold_session")
	if err != nil {
		return auth.Principal{}, false
	}
	session, err := a.service.Store().GetSession(ctx, cookie.Value)
	if err != nil {
		return auth.Principal{}, false
	}
	return auth.NewPrincipal(session.APIKeyID, session.Capabilities, true, session.CSRFHash), true
}

func (a *API) require(ctx context.Context, capabilities ...string) (auth.Principal, error) {
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		p := problem.New(ctx, a.config.PublicURL, http.StatusUnauthorized, "invalid_api_key", "Authentication required",
			"No valid Manifold API key or UI session was provided.",
			problem.Remediation{
				Summary: "Send a valid API key using Authorization: Bearer <key>.",
				Steps:   []string{"Obtain a key from a Manifold administrator.", "Retry the request with the Authorization header."},
			})
		return auth.Principal{}, huma.ErrorWithHeaders(p, http.Header{
			"WWW-Authenticate": {`Bearer realm="manifold"`},
		})
	}
	if !principal.Has(capabilities...) {
		return auth.Principal{}, problem.MissingCapability(ctx, a.config.PublicURL, capabilities...)
	}
	return principal, nil
}

func (a *API) scopedIdempotencyKey(ctx context.Context, key string) string {
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || key == "" {
		return ""
	}
	return principal.KeyID + ":" + key
}

func (a *API) storeError(ctx context.Context, resourceType, id string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		p := problem.New(ctx, a.config.PublicURL, http.StatusNotFound, "resource_not_found", "Resource not found",
			resourceType+" "+id+" does not exist in the active Manifold state.",
			problem.Remediation{
				Summary: "Verify the resource type and slug, then retry with an existing resource.",
				Steps:   []string{"List resources of this type.", "Check the slug for spelling or a recent rename.", "Retry with the canonical slug."},
			})
		p.Resource = "/api/v1/" + resourceType + "/" + id
		if slugType, ok := semanticSlugType(resourceType); ok {
			p.Meta = map[string]any{"resource_type": slugType, "requested_slug": id}
			if suggestions, suggestionErr := a.service.Store().SimilarSlugs(ctx, slugType, id, 3); suggestionErr == nil && len(suggestions) > 0 {
				p.Meta["available_slugs"] = suggestions
			}
			if renamedTo, unambiguous, renameErr := a.service.Store().RenameDestination(ctx, slugType, id); renameErr == nil && unambiguous {
				p.Meta["renamed_to"] = renamedTo
				p.Remediation.Summary = "Use the canonical slug recorded by the rename audit."
				p.Remediation.Steps = append([]string{"Retry with slug " + renamedTo + "; Manifold does not redirect old slugs."}, p.Remediation.Steps...)
			}
		}
		return p
	}
	if errors.Is(err, store.ErrConflict) {
		return problem.New(ctx, a.config.PublicURL, http.StatusConflict, "state_conflict", "State conflict",
			"The operation conflicts with the current resource state.",
			problem.Remediation{
				Summary: "Read the latest resource state, update the request, and retry.",
				Steps:   []string{"GET the resource again.", "Use the new ETag or a different slug.", "Retry the mutation with a new Idempotency-Key."},
			})
	}
	return err
}

func semanticSlugType(resourceType string) (string, bool) {
	switch resourceType {
	case "folders":
		return "folder", true
	case "documents":
		return "document", true
	case "entities":
		return "entity", true
	case "api-keys":
		return "api_key", true
	default:
		return "", false
	}
}

func commonErrors() []int {
	return []int{400, 401, 403, 404, 409, 413, 422, 503}
}

func isUnsafe(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func isOneTimeSecretMutation(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/api/v1/api-keys"
}

func validCSRF(token, expectedHash string) bool {
	sum := sha256.Sum256([]byte(token))
	actual := hex.EncodeToString(sum[:])
	return token != "" && subtle.ConstantTimeCompare([]byte(actual), []byte(expectedHash)) == 1
}

func writeProblem(w http.ResponseWriter, p *problem.Error) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

type captureWriter struct {
	http.ResponseWriter
	body bytes.Buffer
}

func (w *captureWriter) Write(data []byte) (int, error) {
	_, _ = w.body.Write(data)
	return w.ResponseWriter.Write(data)
}
