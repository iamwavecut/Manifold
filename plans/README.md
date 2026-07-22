# Manifold compatibility and release plans

Generated on 2026-07-22 against commit `e03651e`. Execute in order. The plans
cover the client/server schema incident, the independently confirmed Brain
retrieval failure, automatic production delivery, and the first versioned
release after the discovery-first memory work.

## Confirmed incident state

- The globally installed skill matches repository commit `e03651e` byte for
  byte.
- The checkout on `geta.moe` is still at `a21bd0`; its running Manifold binary
  reports the stale build label `3c15ac2` because `.env` pins
  `MANIFOLD_VERSION` independently of the checked-out code.
- The production OpenAPI schema rejects `scope_glob` on context, rejects both
  `scope_glob` and `source_types` on search, and rejects `glob`, `types`, and
  `limit` on tree.
- The new skill always emits those optional values, including empty strings,
  empty arrays, and default query parameters. Consequently even unscoped
  read-only context, search, and tree fail with `validation_failed`.
- The same requests succeed against production when the unsupported optional
  properties are omitted.
- A compatible live search then reports `degraded_dependencies: ["brain"]`.
  Brain logs show SurrealDB rejecting the scoped connection as anonymous after
  the service has been running for several days, while shallow health remains
  green.
- No Manifold GitHub workflow has ever deployed to `geta.moe`. CI builds with
  `push: false`; container publication runs only for `v*` tags; there are no
  tags, releases, production environment, or deployment secrets.

## Execution order and status

| Plan | Title | Priority | Effort | Depends on | Status |
| --- | --- | --- | --- | --- | --- |
| [001](001-negotiate-skill-server-capabilities.md) | Make the skill/server protocol skew-safe | P0 | M | — | TODO |
| [002](002-repair-brain-scoped-session-recovery.md) | Keep Brain retrieval authenticated after SurrealDB session loss | P0 | M | 001 | TODO |
| [003](003-release-and-deploy-v0.2.0.md) | Release v0.2.0 and deploy every green main SHA | P0 | M | 001, 002 | TODO |

Status values: `TODO`, `IN PROGRESS`, `DONE`, `BLOCKED`, or `REJECTED` with a
short reason.

## Dependency notes

- Plan 001 restores legacy-safe read-only behavior and introduces the feature
  metadata that deployment verification needs.
- Plan 002 must land before release because the new `remember` safety gate will
  correctly refuse writes while Brain discovery is degraded.
- Plan 003 is last because production must never advertise or deploy `v0.2.0`
  until both protocol compatibility and durable retrieval recovery pass.

## Immediate containment

Do not mutate production as part of planning. When execution starts, the first
safe operational action is to deploy the completed fixes, not merely restart
Brain or fast-forward the old server to `e03651e`. Fast-forwarding alone fixes
the current schema mismatch but leaves future client/server skew undetected;
restarting Brain only refreshes the scoped session temporarily.

Until `v0.2.0` is deployed, agents may use raw legacy-compatible read-only
requests that omit the new selectors. They must not write through `remember`,
because production discovery is degraded and the old server lacks atomic
`folder_path` support.

## Findings considered and rejected

- **Only redeploy `e03651e`**: rejected as the full fix. It unblocks today's
  schema but does not prevent the same outage on the next additive API change.
- **Retry `validation_failed` after stripping arbitrary fields**: rejected.
  Mutating or retrying based on free-form validation text violates the semantic
  error contract and can silently change explicitly requested filters.
- **Disable Brain's scoped SurrealDB user**: rejected. It restores searches by
  falling back to root but removes the database-level PII defense-in-depth.
- **Treat `/health` as the release gate**: rejected. The live incident proves
  HTTP liveness and root-pool health can remain green while real retrieval is
  broken.

