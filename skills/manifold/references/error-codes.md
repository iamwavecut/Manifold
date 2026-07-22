# Semantic error handling

The CLI uses the response `code`, not free-form text.

| Exit | Codes | Safe action |
| ---: | --- | --- |
| 2 | local configuration or CLI usage | Correct the command or set both environment variables. |
| 10 | `validation_failed`, `invalid_request` | Correct every violation and repeat the request. |
| 11 | `invalid_api_key` | Replace the credential. Do not print it while debugging. |
| 12 | `missing_capability`, `csrf_failed` | Use a key with the required capabilities or refresh the UI session. |
| 13 | `resource_not_found` | Check close slugs or the explicit rename-audit destination. |
| 14 | `slug_taken`, `folder_path_conflict` | Review the suggested slug or existing canonical folder path; do not silently fork identity. |
| 15 | `etag_mismatch`, `idempotency_key_reused`, `state_conflict`, `invalid_state_transition` | Refresh state; use a new key when the intended mutation changed. |
| 16 | `stale_rename_plan`, `unrewritable_reference` | Create a new preview or remove all blockers. |
| 17 | `dependency_unavailable`, `storage_unavailable`, `discovery_degraded` | Restore readiness; do not create memory from incomplete discovery. |
| 18 | `job_failed` | Inspect the stored job remediation before retrying. |
| 19 | any other semantic server error | Give the operator `request_id`; do not expose secrets or private content. |
| 20 | `api_key_secret_not_replayable` | Use the original one-time secret or revoke the key and issue a replacement. |
| 21 | `memory_candidates_found` (local skill decision) | Read relevant candidates, then choose `--update` or explicitly justified `--new`. |
| 22 | `job_wait_timeout` (local skill state) | Inspect the job and wait for a terminal state before claiming persistence. |
| 23 | `server_incompatible` (local protocol gate) | Deploy a compatible server; never silently remove requested filters or write through an unverified legacy contract. |

An idempotent mutation may be retried with the same `Idempotency-Key`. A corrected request that changes intended data should use a new key.
The `remember` command does not retry `etag_mismatch`; the agent must reread and reconcile concurrent content first.
