# Restore the complete Manifold memory pipeline

Status: DEPLOYED AND ACCEPTED. Baseline: `527273a`. Requested on 2026-09-16.

## Scope and success

The user authorized an architectural review, dependency updates, implementation
and refactoring, plus correction of global agent instructions and the installed
Manifold skill. Local implementation and isolated integration verification are
in scope. Commits, pushes, releases, production migration/restart and production
job retries require separate authorization. The user subsequently explicitly
authorized deployment on September 16; migration, real-provider acceptance and
reviewed backfill are now in scope. Commit/push publication remains unrequested.

Success means a document can be durably indexed, produce useful Brain-derived
entities, facts and relations with canonical provenance, and be retrieved
through the existing Manifold REST/CLI surface. Updating or deleting a document
must not present stale derived content as current. Tests must verify these
effects, not only healthy containers or an accepted request.

Preserve public slugs/XIDs, immutable revisions, ETags, idempotency, scoped
retrieval, capability checks, and the no-MCP product boundary. Keep provider
secrets outside artifacts. Use only the deterministic test provider in local
integration tests. Preserve production data and the original checkout.

## Verified starting findings

- Agent sessions on 2026-09-15/16 declared Manifold unavailable after empty MCP
  tool discovery without loading its CLI skill or making a service request.
  Live CLI status, context and canonical document reads work.
- Production runs v0.2.2 at the baseline commit. Its job history contains 133
  jobs: 53 ready, 58 failed, 22 partially ready. The current 21 documents contain
  9 ready, 9 accepted and 3 partially ready records despite no pending jobs.
- The September 8 write failed after exactly 30 seconds in OpenViking
  `POST /api/v1/content/write`: the shared interactive HTTP timeout also covers
  synchronous model-backed writes (`wait: true`).
- The September 2 Brain ingest failed at the two-minute client deadline;
  Brain's request cancellation then aborted the provider operation. Its access
  logger's nominal 201 is not evidence of completed extraction.
- Component probes report ready independently of document pipeline completion.
  Manifold's public graph tables currently count zero entities, facts and edges.
- Brain is pinned to v0.8.1. The current stable release found from its official
  repository is v2.2.0 (`b19b209fec92069d91df22af61db18b0a810ce82`).

These are bounded observations, not proof that Brain's internal graph is empty.

## Architecture

Keep SQLite as the public identity, revision and durable workflow control plane;
OpenViking as document/index storage; and Brain as the extraction, graph and
memory semantics engine. Complete the missing adapters and projections rather
than adding a second memory service or exposing internal identifiers.

Separate interactive requests from durable model-backed operations. Retain
upstream operation checkpoints so retries and restarts reconcile existing work.
Bind completion and provenance to the exact document revision. Surface current
pipeline degradation separately from historical failures and process liveness.

Update stable dependencies as a compatible set, adapting APIs and build inputs
where needed. Retain or replace downstream patches only with source evidence
and a regression test. Document any upstream capability intentionally left off
instead of enabling experimental, paid or autonomous behavior wholesale.

## Execution

1. Correct the global instruction and source skill's CLI/REST discovery route;
   validate and install through the standard skills CLI. Complete; installed files match the source and live read-only requests pass.
2. Audit the graph/provenance and job/revision paths, and inspect current Brain
   and OpenViking contracts. Independent graph, workflow, adapter and retry reviews are complete; all findings in the document pipeline are closed.
3. Update Go/npm/runtime/container dependencies and the Brain build/config
   integration. Use immutable release identities where supported.
4. Add failing regressions for durable timeouts, job/document state, revision
   races, graph projection, canonical provenance and stale/deleted retrieval;
   then implement the smallest coherent repairs.
5. Strengthen the deterministic provider and full integration scenario to prove
   extraction and graph retrieval, updates, failures, retry and restart recovery.
6. Run targeted tests, `make verify`, Compose validation, full integration and
   amd64/arm64 build checks. Independently review the final uncommitted diff.
7. Deliver a concise review, dependency matrix, verification evidence and a
   production migration/rollback procedure. Do not deploy implicitly.

## Baseline and progress

- Isolated worktree: `/Users/wavecut/.codex/worktrees/manifold-refresh`, branch
  `manifold-refresh`; original main checkout was clean.
- Baseline `go test ./...`: 81 tests passed across 15 packages.
- The initial local stage changed no production data or runtime. The subsequent
  authorized deployment is recorded in [deployment verification](../docs/deployment-verification.md).

Local `make` commands used
`DEVELOPER_DIR=/Library/Developer/CommandLineTools`; the host's selected Xcode
requires license acceptance, which this task did not perform. Integration used
`MANIFOLD_INTEGRATION_COMPOSE_OVERRIDE=/tmp/manifold-refresh-local-networks.yml`
for explicit test subnets because the Docker daemon's default address pool is
invalid. No daemon configuration was changed. The temporary Buildx container
driver was removed after the successful two-platform build.

## Verified local progress

- `make verify` passed after the graph/workflow review fixes: all Go packages,
  race checks, vet, TypeScript checks, web build, exact OpenAPI comparison and
  skill checks. Scheduling-sensitive deadline tests use deterministic triggers.
- Linux amd64 and arm64 application build stages passed using a task-owned
  Buildx container driver. The host's default Docker driver cannot build both
  platforms in one invocation; no global Docker configuration was changed.
- The preliminary full-stack tests passed the extracted graph lifecycle
  (create, fact/relation projection, canonical retrieval, update, immutable
  history and deletion) and the existing end-to-end API/rename suite.
- OpenViking same-volume v0.4.10 to v0.4.20 verification preserved the exact
  old snapshot OID and bytes, then created and read a distinct new snapshot.
- Brain v0.8.1 / SurrealDB v3.1.5 data was cold-backed-up and opened by
  Brain v2.2.0 / SurrealDB v3.2.4: 61 migrations applied and the synthetic
  document, two entities, fact and source pointers matched. Cold restore into
  replacement volumes also passed under the old stack. The exact tested
  images and limits are in [migration verification](../docs/migration-verification.md).
- Compose boundaries, skill validation, deployment guard tests and actionlint
  pass. The OpenAPI Make recipe now reliably fails on generator errors/drift.
- Runtime-free skill binaries for Darwin/Linux amd64/arm64 were rebuilt and
  the updated skill was installed with the standard skills CLI.

Final five-patch Brain image: `sha256:2d247f3f72dae2f12a61d75b7e654d024aba6ad9b7a2b99b10f85fddc79a33a9`.
The complete fresh-stack integration passed: graph lifecycle 24.60s, API/rename
24.95s, intentional dependency failure 180.73s, persistence after Manifold restart
0.14s, recovery of the same job 11.42s and cleanup 1.74s. The bundled skill and
Brain reconnection after a SurrealDB restart also passed. Test containers,
networks and volumes were removed by the integration cleanup.

Upstream retry validation passed 57 unit/OpenAPI tests and 10 e2e tests; the
worker startup regression passed in its 18-test suite. The final Docker build
applied all five patches to the pinned upstream commit and built successfully.
No real hosted model provider, production backfill or production migration was
run during that initial local stage. Subsequent deployment evidence follows
the separate authorization and is linked above; no public release or commit
publication was performed.

The final retry patch is SHA-256
`9c029d9f6d7f5a9a1fa43481e78987f4d12409175da1a2cc8413480c43060560`.
Its purge-during-extraction regression was independently rechecked and passed;
the final full integration above includes that exact patch.

## Real-provider extraction correction

Production acceptance found truncated model output becoming a successful empty
graph. A seventh patch rejects incomplete completions and malformed response
shapes, preserves valid empty extraction, and uses a bounded configurable
16,384-token output budget. All 82 extractor unit tests, TypeScript and focused
lint pass; 14 regressions failed before the fix. The seven-patch chain applies
to the exact upstream commit. Fresh Manifold verification and both Linux
architecture builds pass. The final deployment report records the immutable
artifact, complete integration and existing-document recovery evidence.

## Completed production delivery

Production acceptance completed at 2026-09-16 14:12 UTC. All 21 current documents
and jobs are ready; original content/metadata and 125 old revisions are preserved.
Graph, semantic and context retrieval passed through public HTTPS; installed
CLI reports component and pipeline readiness with no pending jobs. Exact
artifact identity and 396 seconds of stable runtime were verified. Test resources
and deployment lock were cleaned; protected cold backups remain. Full evidence
and the upstream hard-crash stale-run limitation are recorded in
[deployment verification](../docs/deployment-verification.md).
