# Semantic error catalogue

Every entry is returned as `application/problem+json`. `request_id` is an XID suitable for structured-log lookup. `retryable` describes whether repeating the same intended operation can be safe after the reported condition changes.

| Code | Status | Meaning | Agent action |
| --- | ---: | --- | --- |
| `validation_failed` | 422 | One or more independent values violate the schema. | Correct every JSON Pointer in `violations`. |
| `invalid_request` | 400 | JSON, content type, or request structure is malformed. | Correct syntax using the OpenAPI schema. |
| `invalid_api_key` | 401 | No active credential matched. | Use an active bearer key. |
| `missing_capability` | 403 | The key lacks one or more capabilities. | Inspect `required_capabilities`; use a suitable key. |
| `csrf_failed` | 403 | Session mutation lacks its paired token. | Create/inspect the session and send `X-CSRF-Token`. |
| `resource_not_found` | 404 | The slug or XID is not active. | Inspect close slugs and any `meta.renamed_to` audit hint. |
| `error_code_not_found` | 404 | The requested documentation code is unknown. | Use `code` from an actual problem response. |
| `slug_taken` | 409 | A semantic slug is occupied. | Review `suggested_slug`; retry with a new idempotency key. |
| `folder_path_conflict` | 409 | A folder segment already belongs to another canonical parent. | Inspect `meta.existing_path`; reuse it or choose a genuinely distinct segment. |
| `etag_mismatch` | 409 | `If-Match` is stale. | GET the current resource, merge, and retry with `current_etag`. |
| `idempotency_key_reused` | 409 | The same key was used for different mutation content. | Replay the exact request or use a new key for changed intent. |
| `api_key_secret_not_replayable` | 409 | The key record was created, but its one-time plaintext secret cannot be replayed. | Use the original secret or revoke the key and issue a replacement. |
| `state_conflict` | 409 | The operation conflicts with active state. | Refresh state and choose a non-conflicting operation. |
| `invalid_state_transition` | 409 | Workflow state disallows the requested action. | Use `current_state` and `allowed_states`. |
| `stale_rename_plan` | 409 | State changed after preview. | Create and review a fresh preview. |
| `unrewritable_reference` | 409 | A binary document blocks safe rewrite. | Manually update every item in `blockers`, then re-preview. |
| `cannot_revoke_current_key` | 409 | The current admin key tried to revoke itself. | Authenticate with another admin key. |
| `dependency_unavailable` | 503 | OpenViking, Brain, or the provider is unavailable. | Inspect `/api/v1/status`; honor `retry_after`. |
| `storage_unavailable` | 503 | SQLite cannot safely serve state. | Restore volume access before retrying. |
| `job_failed` | 500 | A durable worker operation failed. | Inspect the job's stored remediation and retry only after correction. |
| `request_failed` | 4xx/5xx | A framework-level request failure was normalized. | Use `request_id` and remediation. |
| `internal_error` | 500 | An unexpected failure was contained. | Determine mutation outcome before repeating; report `request_id`. |

Problem responses must never include raw provider bodies, internal addresses, stack traces, SQL, bearer keys, session tokens, CSRF tokens, or private document content.
