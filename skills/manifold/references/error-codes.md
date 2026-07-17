# Semantic error handling

The CLI uses the response `code`, not free-form text.

| Exit | Codes | Safe action |
| ---: | --- | --- |
| 2 | local configuration or CLI usage | Correct the command or set both environment variables. |
| 10 | `validation_failed`, `invalid_request` | Correct every violation and repeat the request. |
| 11 | `invalid_api_key` | Replace the credential. Do not print it while debugging. |
| 12 | `missing_capability`, `csrf_failed` | Use a key with the required capabilities or refresh the UI session. |
| 13 | `resource_not_found` | Check close slugs or the explicit rename-audit destination. |
| 14 | `slug_taken` | Review `suggested_slug`; do not silently change a canonical ID. |
| 15 | `etag_mismatch`, `idempotency_key_reused`, `state_conflict`, `invalid_state_transition` | Refresh state; use a new key when the intended mutation changed. |
| 16 | `stale_rename_plan`, `unrewritable_reference` | Create a new preview or remove all blockers. |
| 17 | `dependency_unavailable`, `storage_unavailable` | Wait for readiness and follow `retry_after`. |
| 18 | `job_failed` | Inspect the stored job remediation before retrying. |
| 19 | any other semantic server error | Give the operator `request_id`; do not expose secrets or private content. |

An idempotent mutation may be retried with the same `Idempotency-Key`. A corrected request that changes intended data should use a new key.
