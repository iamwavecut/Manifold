# Plan 002: Keep Brain retrieval authenticated after SurrealDB session loss

> **Executor instructions**: Preserve Brain's scoped-user defense-in-depth.
> Patch the pinned upstream source minimally, add a restart/re-auth integration
> test, and do not use root fallback as the production fix.
>
> **Drift check**: `git diff --stat e03651e..HEAD -- deploy/brain docker-compose.yml scripts/integration.sh test/integration docs .github/workflows/ci.yml`

## Status

- **Priority**: P0
- **Effort**: M
- **Risk**: HIGH; this touches database authentication and production search.
- **Depends on**: Plan 001
- **Category**: bug, security, reliability
- **Planned at**: commit `e03651e`, 2026-07-22

## Why this matters

Production Brain is HTTP-healthy but real `/v1/search` fails because a pooled
scoped SurrealDB connection has lost authentication. Manifold therefore marks
Brain discovery degraded, and the new `remember` command correctly refuses all
writes to avoid duplicate canonical memory. Restarting Brain refreshes the
session temporarily but does not fix a long-running service.

## Confirmed evidence

- Live Brain logs on 2026-07-22 show SurrealDB
  `NotAllowedError: Anonymous access not allowed` on `POST /v1/search`.
- `src/db/surreal.service.ts` in pinned upstream `v0.8.1` re-signs or rebuilds a
  root connection on every root acquisition (`ensureRootSession`).
- The same upstream file's `withScopedCompany` acquires a scoped connection and
  immediately calls `use/query` without re-signing it.
- Upstream comments explicitly document surrealdb-js v2.0.3 zombie/session
  invalidation behavior for root sessions, so applying the same bounded
  recovery principle to the scoped pool is consistent with upstream design.
- The live services have been up for roughly three days, consistent with a
  long-lived pooled session failure rather than bad initial credentials.
- `deploy/brain/openai-base-url.patch` currently patches only provider base URL
  and API-key validation; `docs/brain-patch.md` claims exactly those two changes.

Primary source: [INITE Brain v0.8.1 surreal.service.ts](https://github.com/inite-ai/inite-brain-service/blob/v0.8.1/src/db/surreal.service.ts).

## Commands

| Purpose | Command | Expected result |
| --- | --- | --- |
| Patch applicability | `git -C /tmp/brain apply --check "$PWD/deploy/brain/scoped-session-recovery.patch"` | exit 0 |
| Compose validation | `docker compose --env-file .env.example config --quiet` | exit 0 |
| Integration | `MANIFOLD_PORT=28080 make integration` | exit 0 including SurrealDB restart |
| Full verification | `make verify` | exit 0 |

## Scope

**In scope**:

- new `deploy/brain/scoped-session-recovery.patch`;
- `deploy/brain/Dockerfile`;
- `docs/brain-patch.md`, `THIRD_PARTY_NOTICES.md`, `AGENTS.md`;
- `.github/workflows/ci.yml` patch checks;
- `scripts/validate-compose.sh` if needed;
- `scripts/integration.sh` and `test/integration/integration_test.go`;
- status/deep-smoke assertions introduced by Plan 001.

**Out of scope**:

- disabling `SURREALDB_SCOPED_USER` or `SURREALDB_SCOPED_PASS`;
- changing SurrealDB credentials;
- upgrading the pinned Brain version or SurrealDB major version;
- exposing Brain ports;
- editing or publishing the upstream repository;
- unrelated Brain MCP modules.

## Steps

### Step 1: Encode scoped connection recovery as a separate minimal patch

Patch pinned `v0.8.1` rather than editing generated container output. Mirror
the proven root recovery shape:

- retain scoped credentials and namespace in private fields;
- add a bounded `ensureScopedSession` that re-signs the acquired connection;
- if re-signin fails, close and replace the connection, connect, authenticate
  as the scoped namespace user, and replace the same slot in `all`;
- change `withScopedCompany` to use `let conn`, assign the returned recovered
  connection, then call `use`, schema checks, and request scope binding;
- always return the actual recovered connection to the scoped pool.

Do not fall back to root after initialization. First-boot migration behavior
must remain supported: the existing root fallback may create the scoped user,
after which `resignScopedConns` transitions the pool.

Keep this in a second patch file so provider customization and auth recovery
can be audited or upstreamed independently.

**Verify**: clone exact `v0.8.1`, apply both patches with `--check`, apply them,
and run Brain's build/typecheck. Expected: exit 0 and no unrelated upstream
files changed.

### Step 2: Build the patched Brain deterministically

Update `deploy/brain/Dockerfile` to copy/check/apply both patches in a fixed
order. Update CI and patch documentation with the exact upstream tag and the
reason for each patch.

**Verify**: `docker compose build brain` exits 0; the resulting source diff is
limited to provider configuration and scoped-session recovery.

### Step 3: Add a restart-induced authentication regression test

Extend the isolated integration suite after a canonical document has been
indexed:

1. prove hybrid search has no degraded dependencies;
2. restart only SurrealDB while Brain remains running;
3. wait for SurrealDB health;
4. retry a bounded Brain-backed search until the scoped pool reconnects;
5. require a successful search with no `brain` degradation;
6. prove a subsequent `remember` still reaches `ready`.

Do not merely restart Brain; that would bypass the recovery path being tested.
Keep the deterministic provider and destroy the integration project/volumes on
both success and failure.

**Verify**: run the integration test twice consecutively to catch stale pooled
connections and cleanup errors.

### Step 4: Strengthen release readiness without making `/health` expensive

Keep liveness shallow. Add an explicit authenticated deep retrieval smoke used
by deployment/release automation. It must call search/context and fail when
`degraded_dependencies` contains Brain or OpenViking, even if HTTP status is
200. It must use a non-sensitive fixed query and never print the key or full
provider payload.

**Verify**: the smoke fails against a fixture returning degraded Brain and
passes against the recovered integration stack.

## Test plan

- Patch applicability against exact upstream tag.
- Brain TypeScript build after both patches.
- SurrealDB restart while Brain stays alive.
- No-root-fallback assertion where practical in upstream/unit test support.
- Manifold deep smoke rejects a 200 response with degraded dependencies.
- Repeat integration run and teardown verification.

## Done criteria

- [ ] Scoped pooled connections are re-authenticated or rebuilt before use.
- [ ] Production security still uses `brain_caller`, not root, for caller-facing
      reads.
- [ ] Exact `v0.8.1` accepts both patches cleanly and builds.
- [ ] Integration restarts SurrealDB without Brain and retrieval recovers.
- [ ] Deep search reports no degraded dependency after recovery.
- [ ] `remember` reaches `ready` after the recovery scenario.
- [ ] Patch documentation and notices are accurate.
- [ ] `make verify` and the repeated integration suite pass.

## STOP conditions

- Scoped re-signin cannot be made idempotent with the pinned surrealdb-js API.
- The patch requires weakening field permissions or switching searches to root.
- The first-boot migration path cannot recover the scoped pool deterministically.
- Integration cannot distinguish a real Brain search from shallow health.

## Maintenance notes

Submit this recovery upstream separately when practical. Keep the local patch
until the pinned upstream release contains equivalent scoped-session recovery
and the SurrealDB restart regression test passes without it.

