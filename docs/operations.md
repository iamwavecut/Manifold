# Operations

## Data ownership

Persist all four named volumes:

- `manifold-data`: SQLite public identity and workflow state.
- `openviking-data`: canonical content, indexes, and snapshots.
- `brain-cache`: Brain model/cache and baseline state.
- `surrealdb-data`: graph data.

The volumes form one logical backup set. Quiesce external writers and stop all
four services before a cold backup; do not archive a running RocksDB database.
These commands assume the default Compose project name `manifold`; verify the
actual volume names with `docker volume ls` first.

```bash
install -d -m 700 backups
docker compose stop manifold brain openviking surrealdb
docker run --rm -v manifold_manifold-data:/source:ro -v "$PWD/backups":/backup alpine:3.24.2 \
  tar -C /source -czf /backup/manifold-data.tgz .
docker run --rm -v manifold_openviking-data:/source:ro -v "$PWD/backups":/backup alpine:3.24.2 \
  tar -C /source -czf /backup/openviking-data.tgz .
docker run --rm -v manifold_surrealdb-data:/source:ro -v "$PWD/backups":/backup alpine:3.24.2 \
  tar -C /source -czf /backup/surrealdb-data.tgz .
docker run --rm -v manifold_brain-cache:/source:ro -v "$PWD/backups":/backup alpine:3.24.2 \
  tar -C /source -czf /backup/brain-cache.tgz .
docker compose start surrealdb openviking brain manifold
```

Use a fresh backup directory for each migration and record hashes of the four
archives with the source commit and image identities. Restrict backup access:
they contain private documents and graph data. Provider credentials and `.env`
belong in a separate secret backup, never in the archives.

## Restore

1. Stop the stack.
2. Restore each archive into an empty volume with the same logical role.
3. Restore the matching `.env`.
4. Start `surrealdb`, then `openviking` and `brain`, then `manifold`.
5. Verify `/ready`, `/api/v1/status`, representative documents/revisions, graph queries, and one new document job.

Do not restore only SQLite over newer OpenViking or Brain state. Public mappings and derived state can diverge.

## Durable jobs

Jobs in `indexing` or `extracting` are returned to `accepted` when Manifold restarts. Completed semantic errors persist in SQLite. After a dependency recovers:

Ordinary dependency requests use `MANIFOLD_HTTP_TIMEOUT` (30 seconds by
default). OpenViking writes use `MANIFOLD_OPENVIKING_WRITE_TIMEOUT` (5 minutes)
with `wait: true`; an HTTP 200 response is accepted only when semantic and
vector indexing report completion or an intentional skip. OpenViking exposes
no per-write operation ID, so a timed-out write is reconciled against its
existing resource before retrying.

Brain receives an asynchronous submission with a bounded request timeout
`MANIFOLD_BRAIN_INGEST_TIMEOUT` (2 minutes). Its returned document ID is stored
privately in the job before polling. `MANIFOLD_BRAIN_EXTRACTION_TIMEOUT`
(30 minutes) bounds each polling attempt. Retry resumes the same operation
after a timeout or restart. Increase these limits to match measured provider
latency; do not disable deadlines. The skill's two-minute default wait is
independent: use `remember --wait-timeout 20m` for longer jobs, or keep polling
the existing job ID after a client wait timeout.

Brain's provider request has its own deadline. Compose sets
`BRAIN_OPENAI_TIMEOUT_MS=300000` and `BRAIN_OPENAI_MAX_RETRIES=1` (one SDK retry)
so extraction can outlast the upstream default 30-second interactive deadline.
The production provider exceeded that old deadline even for a 1,469-character
document. These bounds remain inside the normal 30-minute Manifold extraction
attempt; the durable job checkpoint survives a later retry. Brain validates
`BRAIN_OPENAI_MAX_RETRIES` as a positive integer: zero prevents startup.
The September 16 production deployment overrides the per-request timeout to
900000 ms after repeated provider timeouts; see the deployment evidence below.

Document extraction uses `BRAIN_EXTRACTOR_MAX_COMPLETION_TOKENS=16384`
(valid range 1–65536). Incomplete model output fails the run; an empty graph
is successful only after a valid completed extraction. Private Brain request
buckets default to 600 requests per minute (`BRAIN_THROTTLE_LIMIT` and
`BRAIN_THROTTLE_EXPENSIVE_LIMIT`) so per-candidate hydration fits the extraction
deadline. Authentication, tenant checks, and endpoint-specific limits remain.

```bash
curl -X POST "$MANIFOLD_URL/api/v1/jobs/$JOB_ID/retry" \
  -H "Authorization: Bearer $MANIFOLD_API_KEY" \
  -H "Idempotency-Key: retry-$JOB_ID-1"
```

Retry only `failed` or `partially_ready` jobs. Other states return `invalid_state_transition`.

`status.state` and `status.components` describe component probes. Inspect
`status.pipeline` for current document states; historical failed job counts are
not a current read outage. A failed pre-snapshot write marks the current
document `failed`; a preserved canonical snapshot with incomplete extraction
is `partially_ready`. Jobs for older revisions cannot mark a newer revision
ready. Inspect the stored error and exact revision before selecting retries.

Rename work also checkpoints each affected document revision. A retryable
upstream failure preserves the applying plan, its slug reservations and saved
Brain operations; retry the same job after recovery. Document/folder/entity
creates and document updates/deletes are checked transactionally against those
reservations. A terminal failure is compensated and rolled back; follow its
remediation and create a fresh reviewed plan rather than assuming the old
rolled-back plan can resume.

Async Brain extraction retains normalized source chunks. Deleting a Manifold
document withdraws it from current public retrieval but does not purge those
private Brain chunks. Review the [retention boundary](brain-patch.md) before
production migration; the service key is not granted administrator scope.

## Brain liveness

Compose reports an unhealthy container but never restarts it. Brain's
healthcheck (`deploy/brain/healthcheck.sh`) therefore acts as a watchdog: once
the current Brain process has answered `/health`, `BRAIN_HEALTH_RESTART_FAILURES`
consecutive failures (default 20, five minutes at the 15-second interval) send
it `SIGTERM`, and a further failure sends `SIGKILL`. `restart: unless-stopped`
then replaces the container. Failures before the first successful probe are
not counted, so a slow startup or migration is left to `compose up --wait`.
Brain's `/health` stays successful while SurrealDB is unreachable, so the
watchdog reacts to a wedged process rather than to a dependency outage. The
log of the restarted container contains the watchdog line, and its restart
count increases; investigate both instead of treating the restart as recovery.

## Data version 3 migration

This release moves Brain v2.2.0 to v2.3.0, SurrealDB v3.2.4 to v3.3.0 and
OpenViking v0.4.20 to v0.4.22. Brain applies migrations 0130–0160 on startup
and SurrealDB 3.3 rewrites its storage on first open; neither can be undone by
starting older images. `DATA_VERSION=3` therefore stops automatic deployment
before the checkout or runtime changes. Follow the steps of the
[data version 2 procedure](#brain-v2--data-version-2-migration) with these
differences:

- The cold four-volume backup in step 2 is the only rollback path. Verify the
  archive hashes before any new image opens the volumes.
- Brain v2.3 changes these defaults, which the Compose file sets explicitly:
  `EXTRACTOR_SC_PASSES` (three extraction samples per document,
  `BRAIN_EXTRACTOR_SC_PASSES`), and `EPISODE_SUBSTRATE_ENABLED=0`, because
  Manifold sends documents rather than conversational turns.
- `scoped-session-recovery.patch` is gone: v2.3 re-authenticates scoped
  SurrealDB sessions itself. The integration run still restarts SurrealDB under
  a running Brain to prove it.
- With an OpenRouter-compatible `OPENAI_BASE_URL`, set
  `OPENAI_CHAT_EXTRA_BODY` (see `.env.example`) before starting the stack.
  Brain merges it into its chat calls, and OpenViking's start renders it into
  `vlm.extra_request_body` (`deploy/openviking/render-config.py`, because the
  template must stay valid JSON before placeholder expansion); OpenViking's own `thinking: false` only reaches
  DashScope endpoints. Without it, hybrid reasoning models spend the small
  output budgets of rerank and classification calls on hidden reasoning and
  return no JSON, and OpenViking's semantic summaries run long enough for
  waited writes to hit their 300-second timeout.
- After acceptance, recover documents that are `partially_ready` from the
  September 30 Brain outage through their jobs' retry action, one at a time.

## Brain v2 / data version 2 migration

This release moves Brain v0.8.1 to v2.2.0, SurrealDB v3.1.5 to v3.2.4 and
OpenViking v0.4.10 to v0.4.20. Brain applies its versioned migrations on startup.
The upgrade must not use a code-only downgrade after new services open the
volumes. `DATA_VERSION=2` makes automatic deployment refuse the transition
from older releases (which implicitly have data version 1) before checkout or
runtime mutation. Deployments within the same data version retain the existing
automatic rollback behavior.

After the operator authorizes the production migration:

1. Record the exact running commit, container image digests, Compose project
   name, current document/revision counts and pending jobs. Build the reviewed
   target images in a separate checkout and run the full deterministic suite.
   Keep existing embedding provider, model and dimensions; changing embedding
   spaces is a separate migration.
2. Quiesce clients, stop all services, and create and verify the four-volume
   cold backup above plus a protected configuration backup. Rehearse restoring
   this backup into isolated volumes before opening production data with the
   new dependencies. Do not print private payloads during validation.
3. Advance the clean source checkout to the approved exact commit. Add the new
   timeout values if defaults are unsuitable, preserve secrets, and validate
   `docker compose --env-file .env config --quiet`. Start SurrealDB, then Brain
   and OpenViking. Verify their migrations and readiness before starting
   Manifold. Keep the old images and full backup until acceptance.
4. Check public build identity, current canonical content and old revision
   snapshots. Write a small approved synthetic document; wait for terminal
   `ready`, verify extracted entity/fact/relation and canonical provenance in
   graph search, update it and verify stale evidence disappears, then delete
   it. Restart Manifold and verify completed data plus an interrupted job's
   checkpoint recovery. Health alone does not meet acceptance.
5. Recover current failed/partial jobs individually after reviewing their
   stored errors. Existing `ready` documents are not silently re-extracted:
   backfill them through reviewed same-content updates with current ETags and
   fresh idempotency keys. Each update creates an auditable new revision and
   fills the public graph. Check per-document completion and provider cost
   before proceeding to the next batch.
6. If acceptance fails after migration, stop every service and restore **all
   four** backup volumes into clean replacement volumes, the matching protected
   config and the recorded old code/images. Keep the failed migrated set for
   investigation. Verify document/revision counts and representative retrieval
   before reopening clients. Never start old Brain against migrated SurrealDB
   data or restore only SQLite.

Execute these production steps only with explicit deployment authorization.
The September 16 authorized migration is recorded in
[deployment verification](deployment-verification.md).

## Local integration overrides

`make integration` owns only the `manifold-integration` Compose project and
removes that project's test volumes on exit. An optional
`MANIFOLD_INTEGRATION_COMPOSE_OVERRIDE=/absolute/path/override.yml` appends a
local Compose override. This supports explicit, non-overlapping test subnets
when a host's automatic Docker address pool is invalid, without modifying the
daemon or production Compose networks. Keep credentials and production volumes
out of integration overrides.

## Release and production deployment

`GET /api/v1/meta` is public and contains only service version, exact build
commit, API major, protocol revision, and advertised feature names. Use it to
compare a remote deployment with its skill before authentication or mutation.

The `Deploy production` workflow runs only after a successful push CI run on
`main` (or an explicit manual dispatch). It checks out the event's immutable
SHA and invokes `scripts/deploy-production.sh` on the host. The script requires
a clean `/home/wavecut/Manifold` checkout on `main`, `.env` mode `0600`, and a
target commit reachable from `origin/main`. It uses `docker compose up` without
`down`, preserves every named volume, and verifies:

- health and readiness through the public URL;
- exact version/commit/protocol metadata;
- authenticated status with every component ready;
- scoped semantic search, context, and tree without degraded dependencies;
- the running binary's build identity.

If deep smoke fails after source activation, the script restores the recorded
clean source SHA, rebuilds that stack, verifies health, and returns failure to
GitHub. A green CI run, published GHCR image, GitHub release, and live
deployment are distinct states; report their SHAs separately.

The GitHub `production` environment holds only deployment transport material:
`MANIFOLD_DEPLOY_SSH_KEY` and pinned `MANIFOLD_DEPLOY_KNOWN_HOSTS` secrets,
plus `MANIFOLD_DEPLOY_HOST`, `MANIFOLD_DEPLOY_USER`, and
`MANIFOLD_PUBLIC_URL` variables. The Manifold API key remains only in the
host's protected `.env`; it is never copied into GitHub Actions.

## Key rotation

Create a new admin key, verify it, switch clients, then revoke the old key from the new credential. Manifold refuses self-revocation to prevent accidental lockout.

`OPENVIKING_API_KEY` is the root key for the internal trusted deployment and must match in both the Manifold and OpenViking containers. Rotate it during a controlled stack restart.

Brain uses a SHA-256 registry because it is an internal pinned dependency. Regenerate `BRAIN_API_KEYS_JSON` with `scripts/brain-key-hash.sh` whenever `BRAIN_API_KEY` rotates.

## Scale boundary

Version 1 is one tenant and one Manifold replica. SQLite worker leasing protects restart recovery but is not a multi-replica coordination contract. Do not place two Manifold replicas over the same database.
