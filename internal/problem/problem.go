package problem

import (
	"context"
	"fmt"
	"net/http"
)

type contextKey string

const requestIDKey contextKey = "request-id"

type Violation struct {
	Pointer  string `json:"pointer"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Expected any    `json:"expected,omitempty"`
	Received any    `json:"received,omitempty"`
}

type Remediation struct {
	Summary string   `json:"summary"`
	Steps   []string `json:"steps,omitempty"`
}

type Error struct {
	Type                 string         `json:"type"`
	Title                string         `json:"title"`
	Status               int            `json:"status"`
	Detail               string         `json:"detail"`
	Instance             string         `json:"instance,omitempty"`
	Code                 string         `json:"code"`
	RequestID            string         `json:"request_id"`
	Retryable            bool           `json:"retryable"`
	RetryAfter           string         `json:"retry_after,omitempty"`
	Violations           []Violation    `json:"violations,omitempty"`
	CurrentState         string         `json:"current_state,omitempty"`
	AllowedStates        []string       `json:"allowed_states,omitempty"`
	Remediation          Remediation    `json:"remediation"`
	DocumentationURL     string         `json:"documentation_url"`
	Job                  string         `json:"job,omitempty"`
	Resource             string         `json:"resource,omitempty"`
	CurrentETag          string         `json:"current_etag,omitempty"`
	SuggestedSlug        string         `json:"suggested_slug,omitempty"`
	RequiredCapabilities []string       `json:"required_capabilities,omitempty"`
	Blockers             []Blocker      `json:"blockers,omitempty"`
	Meta                 map[string]any `json:"meta,omitempty"`
}

type Blocker struct {
	Resource string `json:"resource"`
	Reason   string `json:"reason"`
	Location string `json:"location,omitempty"`
}

func (e *Error) Error() string {
	return e.Detail
}

func (e *Error) GetStatus() int {
	return e.Status
}

func (e *Error) ContentType(contentType string) string {
	if contentType == "application/json" {
		return "application/problem+json"
	}
	return contentType
}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func New(ctx context.Context, publicURL string, status int, code, title, detail string, remediation Remediation) *Error {
	requestID := RequestID(ctx)
	return &Error{
		Type:             fmt.Sprintf("%s/errors/%s", publicURL, code),
		Title:            title,
		Status:           status,
		Detail:           detail,
		Instance:         fmt.Sprintf("%s/api/v1/requests/%s", publicURL, requestID),
		Code:             code,
		RequestID:        requestID,
		Remediation:      remediation,
		DocumentationURL: fmt.Sprintf("%s/docs/errors/%s", publicURL, code),
	}
}

func Validation(ctx context.Context, publicURL string, violations ...Violation) *Error {
	err := New(ctx, publicURL, http.StatusUnprocessableEntity, "validation_failed", "Validation failed",
		"One or more request values are invalid.",
		Remediation{
			Summary: "Correct every listed violation, then submit the request again.",
			Steps: []string{
				"Inspect each JSON Pointer in violations.",
				"Replace the received value with one matching expected.",
				"Retry with the same Idempotency-Key if the original request was a mutation.",
			},
		})
	err.Violations = violations
	return err
}

func MissingCapability(ctx context.Context, publicURL string, required ...string) *Error {
	err := New(ctx, publicURL, http.StatusForbidden, "missing_capability", "Missing capability",
		"The API key is valid but cannot perform this operation.",
		Remediation{
			Summary: "Use an API key that has every required capability.",
			Steps: []string{
				"Inspect required_capabilities in this response.",
				"Ask a Manifold administrator to create or update a suitable key.",
				"Retry with the replacement key.",
			},
		})
	err.RequiredCapabilities = required
	return err
}
