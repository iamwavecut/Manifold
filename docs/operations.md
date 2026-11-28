# Operations

## Data ownership

Manifold keeps all state in the `manifold-data` volume:

- `manifold.db`: canonical documents and revisions, public IDs, jobs,
  idempotency records, rename plans, API-key hashes, sessions, and the public
  graph projection. It is the source of truth.
- `index.db`: the derived index. It holds document chunks, extracted entities,
  facts and relations, the FTS5 tables, the int8 vectors, and the extraction
  result of each document revision. Manifold can rebuild it from
  `manifold.db`; see [Rebuild the index](#rebuild-the-index).

Both files use SQLite WAL mode, so each has `-wal` and `-shm` companions. Copy
them only while Manifold is stopped. The image has no shell; use a throwaway
container for the archive. These commands assume the default Compose project
name `manifold`; check the volume name with `docker volume ls` first.

```bash
install -d -m 700 backups
docker compose stop manifold
docker run --rm -v manifold_manifold-data:/source:ro -v "$PWD/backups":/backup alpine:3.24.2 \
  tar -C /source -czf /backup/manifold-data.tgz .
docker compose start manifold
```

Record the archive hash with the source commit and image digest. Restrict
access to backups: they contain private documents. Keep provider credentials
and `.env` in a separate secret backup, never in the archive.

## Restore

1. Stop Manifold.
2. Restore the archive into an empty `manifold-data` volume.
3. Restore the matching `.env`.
4. Start Manifold and verify `/ready`, `/api/v1/status`, representative
   documents and revisions, graph queries, and one new document job.

Restoring `manifold.db` without `index.db` is safe: Manifold seeds a new index
at start. Restoring an `index.db` that is newer than its `manifold.db` is not:
remove that `index.db` instead.

## Rebuild the index

When `index.db` is missing, Manifold seeds a new index from `manifold.db`
before it serves requests. The seed copies every mapped entity, fact and
relation with its index ID, every live folder and document, and the projected
graph of every `ready` document revision. It needs no model call. A repeated
sync of a seeded revision (a retry, or a rename that touches every document)
returns the stored result, so it neither calls the model nor changes the public
projection. Entities created through the API keep no index ID; the seed finds
them by name, as every API call about them does. `manifold.db` keeps fact
statuses but not which fact superseded which, so the seed links each
superseded value of `status`, `os_version` and `code_memory__default_value` to
the next newer value of the same entity: retracting a seeded current value
makes the value before it current again.

The background embedder then gives every chunk and fact a vector, 64 texts per
provider call. Until it finishes, document search (`semantic`) returns only
documents that already have vectors; lexical document search and graph
full-text search work at once. The embedder logs `index embedded` with the row
count of each pass. A search request with `scope` (a locator prefix) compares
the query with every chunk under that prefix. Without `scope`, the exact
leading segments of `scope_glob` (`projects/ngbot` in `projects/ngbot/**`)
narrow it the same way, and the whole glob then filters the hits. With
neither, the index returns the nearest chunks of the whole corpus.

To rebuild on purpose, stop Manifold, move `index.db`, `index.db-wal` and
`index.db-shm` out of the volume, and start Manifold. A change of
`OPENAI_EMBEDDING_MODEL` or `MANIFOLD_EMBEDDING_DIMENSIONS` keeps the graph and
re-embeds only the vectors.

## Durable jobs

Jobs in `indexing` or `extracting` return to `accepted` when Manifold
restarts. Completed semantic errors persist in SQLite.

A document sync writes the revision to the index, stores a snapshot
checkpoint, then extracts the revision:

- The text is split into sections of at most 12,000 characters at headings,
  blank lines and code fences. Up to `MANIFOLD_EXTRACTION_CONCURRENCY` sections
  (4 by default, 1–16) go to the chat model at once, each as one
  `json_schema` request named `extraction`.
- A section whose answer is not valid JSON is extracted again as two halves,
  at most twice.
- Every fact must be grounded: its evidence or value has to appear in the
  section. Ungrounded facts, invalid indexes and empty predicates are dropped.
- Entities resolve by normalized name, then by alias, preferring the same
  kind. A known fact stated again keeps its ID and the higher confidence.
- `status`, `os_version` and `code_memory__default_value` hold one current
  value per entity: the statement with the latest `valid_from` wins and the
  others become `superseded`. Every other predicate keeps all its values.

`MANIFOLD_PROVIDER_TIMEOUT` (3 minutes) bounds one provider call.
`MANIFOLD_EXTRACTION_TIMEOUT` (30 minutes) bounds the extraction of one
revision. A call that fails with HTTP 429, a 5xx status or a network error is
repeated up to three attempts; `Retry-After` is honored up to 30 seconds. The
skill's two-minute default wait is independent: use
`remember --wait-timeout 20m` for longer jobs, or keep polling the existing
job ID after a client wait timeout.

```bash
curl -X POST "$MANIFOLD_URL/api/v1/jobs/$JOB_ID/retry" \
  -H "Authorization: Bearer $MANIFOLD_API_KEY" \
  -H "Idempotency-Key: retry-$JOB_ID-1"
```

Retry only `failed` or `partially_ready` jobs. Other states return
`invalid_state_transition`.

`status.state` and `status.components` describe component probes: `sqlite`,
`index`, and `provider` (`unknown` before the first call, then `ready` or
`degraded` after the last one). Inspect `status.pipeline` for current document
states; historical failed job counts are not a current read outage.

When the model provider refuses extraction, the job stores
`provider_credits_exhausted` (no credit) or `dependency_unavailable` (key, rate
limit, outage), and `status.pipeline` reports `blocked_documents` and
`blocked_reason`. `status.state` stays `ready` because the components answer.
Every `MANIFOLD_PROVIDER_RETRY_INTERVAL` (15 minutes, `0` turns it off)
Manifold retries the longest-blocked document as a probe. After one sync
succeeds it retries all blocked documents. You do not need to retry them by
hand. Search reports `semantic` in `degraded_dependencies` while the provider
refuses query embeddings; graph search then falls back to full-text matching.

After a failed embedding pass the embedder waits one minute, doubling up to 30
minutes. The next call the provider answers, such as an extraction or a query
embedding, ends the wait.

A failed pre-snapshot write marks the current document `failed`; a preserved
snapshot with incomplete extraction is `partially_ready`. Jobs for older
revisions cannot mark a newer revision ready. Inspect the stored error and
exact revision before selecting retries.

Rename work also checkpoints each affected document revision. A retryable
failure preserves the applying plan and its slug reservations; retry the same
job after recovery. Document, folder and entity creates and document updates
and deletes are checked transactionally against those reservations. A terminal
failure is compensated and rolled back; follow its remediation and create a
fresh reviewed plan rather than assuming the old rolled-back plan can resume.

Deleting a document removes its text and chunks from the index and retracts
the facts and relations that no other live document states.

## Job concurrency

Manifold runs `MANIFOLD_WORKER_CONCURRENCY` workers (2 by default, 1–16) in one
process. Jobs of different documents run in parallel. Jobs of one resource run
one at a time, in the order they were created. A `rename.apply` job runs only
when no other job runs, and jobs created after it wait until it finishes. Set
the value to 1 to run every job in sequence.

A document sync holds the derived-graph phase shared from its extraction until
its graph projection is stored. The retraction of facts and relations that a
document no longer states holds the phase exclusively, so it does not withdraw
a fact that another document is about to claim. The index serializes its write
transactions; reads run on up to four connections beside them.

## Memory and disk

The index runs in the Manifold process. Compose limits the container to
`MANIFOLD_MEMORY_LIMIT` (1 GiB) and sets `GOMEMLIMIT` to
`MANIFOLD_GOMEMLIMIT` (384 MiB): the Go runtime collects garbage harder as the
heap nears that value. Each SQLite connection keeps a page cache of at most
4 MB. A vector takes `MANIFOLD_EMBEDDING_DIMENSIONS` bytes (512 by default).

An API key check runs Argon2id with 64 MiB of memory. Manifold runs it once
per key and process, then remembers the result; an unknown bearer token runs
it on every request. At most two checks run at once, so a flood of invalid
tokens costs at most 128 MiB and two checks' worth of CPU; other requests with
unknown tokens wait, and stop waiting when their client disconnects. After the
first check the Go runtime keeps up to about 130 MiB for a few minutes before
it returns the memory. See [performance](performance.md) for measured figures.

## Container logs

Compose writes the log with the `local` driver: compressed, at most three
10 MB files. Docker's default `json-file` driver keeps logs without a limit.
A container keeps its log only while it exists; recreating it, as every
deployment does, starts an empty log.

## Data version 6 migration

Data version 6 replaces OpenViking, INITE Brain and SurrealDB with the
embedded index. `manifold.db` keeps its schema; the new release adds
`index.db` beside it and never opens the `openviking-data`, `brain-cache` or
`surrealdb-data` volumes. `DATA_VERSION=6` stops automatic deployment before
the checkout or runtime changes.

1. Confirm that no job is `indexing` or `extracting` and that the provider
   account has credit. Record the counts in `GET /api/v1/status`.
2. Stop the old stack and take the cold backup of all four old volumes with
   the commands of the previous release. Verify the archive hashes.
3. Replace the Compose project with the single `manifold` service of this
   release. Keep `MANIFOLD_BOOTSTRAP_API_KEY` and the `OPENAI_*` provider
   settings. `OPENAI_EMBEDDING_DIMENSIONS` is no longer read; the index uses
   `MANIFOLD_EMBEDDING_DIMENSIONS`.
4. Start Manifold. It logs `index seeded from the canonical database`, then
   `index embedded` while the embedder fills the vectors. Check that the
   status counts match step 1.
5. Acceptance: once the embedder is idle, search and context return results
   without `degraded_dependencies`. Write a small synthetic document, wait for
   `ready`, check its entity and fact in graph search, update it and check that
   the old fact disappears, then delete it.
6. Keep the old volumes until acceptance holds for some days. Remove them
   only on an explicit decision.

Rollback: stop Manifold and start the previous release on the backed-up
volumes. Documents written after the switch exist only in `manifold.db` and
the new index; the previous release does not know them.

Earlier data-version procedures (versions 2–5) are in the v0.2.2 tree of this
file.

## Local integration overrides

`make integration` owns only the `manifold-integration` Compose project and
removes that project's test volumes on exit. An optional
`MANIFOLD_INTEGRATION_COMPOSE_OVERRIDE=/absolute/path/override.yml` appends a
local Compose override, for example explicit test subnets when a host's
automatic Docker address pool is invalid. Keep credentials and production
volumes out of integration overrides.

## Release and production deployment

`GET /api/v1/meta` is public and contains only service version, exact build
commit, API major, protocol revision, and advertised feature names. Use it to
compare a remote deployment with its skill before authentication or mutation.

The `Deploy production` workflow runs after successful push CI on `main` (or
an explicit manual dispatch). It builds the Manifold image for the exact
immutable commit **on the GitHub runner**, publishes its digest to the existing
public GHCR package, and submits it to the host's restricted deployment broker.
No host script is uploaded or executed, and the CI user has no shell, sudo, or
Docker socket access. The old `scripts/deploy-production.sh` is a legacy
operator procedure, not the production CI entrypoint.

The host independently verifies the short-lived GitHub Actions OIDC identity
(audience `geta-deploy`, repository ID, main workflow ref, production
environment) and successful main-push CI for the target commit. Its
operator-owned runtime profile fixes the image repository, service name,
mounts, networks, and settings. Only the image digest changes. The broker uses
Compose without `down` and without builds, and preserves named volumes. It
checks container health, public readiness and build identity, authenticated
component status, and scoped semantic retrieval without degraded dependencies.

A failed rollout restores the previously recorded pinned image and returns a
failure to GitHub. Manual dispatch with `operation: rollback` restores the
previous compatible release. Data-version changes require a coordinated
operator migration; changes to the `docker-compose.yml` runtime-profile
fingerprint require explicit operator review and profile update. Neither is
silently accepted from CI. A green CI run, published GHCR image, GitHub
release, and live deployment remain distinct states; report their SHAs
separately.

The GitHub `production` environment holds `MANIFOLD_DEPLOY_SSH_KEY` and pinned
`MANIFOLD_DEPLOY_KNOWN_HOSTS` secrets, plus `MANIFOLD_DEPLOY_HOST`,
`MANIFOLD_DEPLOY_USER=deploy`, and `MANIFOLD_PUBLIC_URL` variables. OIDC is
minted only for the request and passed over SSH stdin, never printed or placed
in an SSH command argument. Production API credentials remain in root-only host
configuration, never in GitHub Actions. Privileged broker source and
configuration updates use a separate, administrator-reviewed private
infrastructure repository.

## Key rotation

Create a new admin key, verify it, switch clients, then revoke the old key from
the new credential. Manifold refuses self-revocation to prevent accidental
lockout. Rotate `OPENAI_API_KEY` with a Manifold restart.

## Scale boundary

Version 1 is one tenant and one Manifold replica. SQLite worker leasing
protects restart recovery but is not a multi-replica coordination contract. Do
not place two Manifold replicas over the same database.
