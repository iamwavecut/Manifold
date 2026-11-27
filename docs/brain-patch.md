# INITE Brain integration

Manifold builds INITE Brain from release v2.11.0, commit
`6ba986054a546c71c93c39e0e118e0f3dddc983f`. The exact ref and base-image digests
live in [`deploy/brain/Dockerfile`](../deploy/brain/Dockerfile); the patch
application order lives in [`deploy/brain/series`](../deploy/brain/series). The
build uses Node 24, upstream's base-image digest, and an Alpine source stage
pinned by digest. The runtime image also carries upstream's `packs/` directory.

Brain's flag defaults live in code, so the image freezes the ones that the
previous Manifold build ran with: `deploy/brain/flags/defaults.env` pins every
changed default to its old value and every new on-by-default switch to `0`, and
`node --env-file` loads it. Compose and `.env` values still win. CI rebuilds the
config catalog (`scripts/brain-catalog.sh`) and fails when
`scripts/brain-flags.py --check` finds the file stale. List a key in
`deploy/brain/flags/adopt.txt` to take its new upstream default.

The downstream patches are narrow.
[`scripts/brain-apply-patches.sh`](../scripts/brain-apply-patches.sh) applies
them in series order, and [CI](../.github/workflows/ci.yml) checks that they
apply:

- [`openai-api-key-format.patch`](../deploy/brain/openai-api-key-format.patch)
  accepts API keys that do not use OpenAI's `sk-` prefix. Upstream
  reads `OPENAI_BASE_URL` itself, so this patch no longer touches the client.
- [`document-origin-identity.patch`](../deploy/brain/document-origin-identity.patch)
  salts the content hash with canonical Manifold document/revision identity.
  Retries for one revision remain idempotent, while different documents or
  revisions with identical text retain distinct provenance. Other verticals
  keep their existing content-hash behavior.
- [`failed-run-retry.patch`](../deploy/brain/failed-run-retry.patch) adds
  `POST /v1/documents/:id/retry` for the current document's failed internal
  runs. It requires `brain:write`, applies the same tenant and user visibility
  checks as document reads, and ignores pending, running, succeeded, and
  external runs. A bounded `retryKey` makes request replays idempotent; the
  existing run-ledger compare-and-swap admits only one worker to reopen a
  failed run. Retries require retained source chunks and the original installed
  pack version (for the general indexer, upstream's `GENERAL_INDEXER_VERSION`;
  runs recorded under an older extraction contract report
  `skipped_version_mismatch`). The queued payload pins that version for execution-time
  validation. Until 2026-11-27 the execution check still compared the general
  indexer against `0`, so after upstream moved it to `3` every accepted general
  retry was skipped without reading the document; a unit spec now pins the
  check to `GENERAL_INDEXER_VERSION`. An accepted retry also puts the failed
  run back to `pending`, so a caller that polls before a worker claims the job
  does not read the old failure as the result; a replayed `retryKey` then finds
  nothing to queue. Upstream's nightly sweep also retries failed runs, so a
  queued retry and the sweep can reopen one run; the run-ledger reads retry the
  write conflict that this can cause instead of failing the request. The worker
  rechecks source availability after extraction and
  before automatic commit; conditional header writes preserve `purged` during
  concurrent extraction or commit. Purge does not retract already committed
  knowledge or change manual candidate review. A commit that passed its content
  check before a concurrent purge can still finish graph writes; serializing
  those operations would require a shared transaction or lock across the
  commit pipeline. The conditional header write still preserves `purged`.
- [`worker-startup-order.patch`](../deploy/brain/worker-startup-order.patch)
  starts the worker scheduler in `onApplicationBootstrap`, after module-owned
  job handlers register in `onModuleInit`. Without it, an early lease tick with no
  handlers retries later, but one with some handlers starts only those and
  never the ones registered afterwards.
- [`corroborated-commit-ref.patch`](../deploy/brain/corroborated-commit-ref.patch)
  makes Manifold document candidates reference the resolver's serving fact
  when an identical claim corroborates it. Brain retains the corroborating
  audit row, but excludes that row from search. Returning its ID previously
  disconnected current revision projections from searchable facts. Other
  verticals keep their existing commit-reference contract. This correction
  applies to newly committed candidates; reprocess previously completed
  affected documents through a new same-content revision.

- [`extraction-completion.patch`](../deploy/brain/extraction-completion.patch)
  replaces the 1,500-token extraction cap with a configurable 16,384-token
  budget. The configured value also replaces upstream's input-size allowance
  (`extractionOutputAllowance`, 1,500 to 16,000 tokens), so the operator's
  setting stays authoritative. Truncated responses, malformed JSON, missing arrays, and failed
  extraction passes fail the durable run instead of producing a successful
  empty graph. Valid explicit empty results remain valid. Errors contain no
  model output. Reprocess earlier false-success runs through new revisions.

- [`fatal-signal-default.patch`](../deploy/brain/fatal-signal-default.patch)
  limits Nest's shutdown hooks to `SIGTERM` and `SIGINT`. Upstream calls
  `enableShutdownHooks()` without a list, which also subscribes to `SIGSEGV`,
  `SIGBUS`, `SIGILL`, and `SIGFPE`. On September 30 a real segmentation fault
  after an onnxruntime worker crash then re-faulted about 3,000 times per
  second while Nest closed the HTTP listener, leaving Brain without a port and
  unhealthy for two days. Fatal signals now keep their default action so the
  container exits and restarts.
- [`openai-chat-extra-body.patch`](../deploy/brain/openai-chat-extra-body.patch)
  merges the JSON object in `OPENAI_CHAT_EXTRA_BODY` into every
  chat-completions request; fields a call sets itself win, and an invalid value
  fails boot validation. It applies to the primary provider only, not to
  the optional fallback provider. Brain treats non-OpenAI model names as deterministic
  and never sets their reasoning controls, so a hybrid model behind OpenRouter
  runs its default reasoning. Measured on 2026-10-02 with
  `deepseek/deepseek-v4.1-flash`, the reranker's 256-token json_schema call
  spent all 256 tokens on reasoning and returned no content (24–32 s); with
  `{"reasoning":{"enabled":false},"provider":{"require_parameters":true}}` it
  returned schema-valid JSON in about 3 s. `require_parameters` keeps strict
  json_schema calls off providers that silently ignore it.

- [`entity-fact-read-index-hints.patch`](../deploy/brain/entity-fact-read-index-hints.patch)
  reads facts by entity through `WITH INDEX fact_entity_idx` in two places:
  `GET /v1/entities/:id` (the entity profile) and upstream's post-resolve
  expectation stamp, which runs for every corroborated (restated) fact.
  SurrealDB 3.3 otherwise picks the single-field `userId` index for
  `userId IS NONE`, which matches every row of a tenant without per-user
  memory, or scans the table for `entityId = $t.entityId`. Measured on a copy
  of the production graph (3,496 facts): a profile took 278 ms before and
  1 ms after, an expectation stamp 236 ms before; on the production VPS a
  profile took 1.4-4.7 s. Hydration calls the profile once per entity.
- [`edge-retraction.patch`](../deploy/brain/edge-retraction.patch) lets
  `POST /v1/facts/:id/retract` take a `knowledge_edge:` id. Brain serves
  every live edge of a matched entity as a fact-shaped line in search (the
  relation leg), and upstream has no way to withdraw an edge, so the relation of a
  deleted document kept ranking. The route closes the edge on both time axes
  (`validUntil` unless it already ended, `invalidatedAt`) and records the
  retraction and the earlier end in `source.retraction`, with the same
  ownership fence as a fact and idempotent repeats. When a later document
  states the same triple, `createEdgeBetween` finds the closed edge through
  the unique index and reopens it from that record; an edge closed by
  supersession or by a stated end has no record and stays closed. There is no
  migration.
- [`revive-superseded-index.patch`](../deploy/brain/revive-superseded-index.patch)
  rewrites the predecessor revival of `POST /v1/facts/:id/retract`. SurrealDB
  3.3 evaluates the `WHERE` of `UPDATE knowledge_fact SET ... WHERE
  supersededBy = ...` against every row although `fact_superseded_by_idx`
  exists; the same predicate in a `SELECT` is an index scan. The patch reads
  the predecessors with `SELECT VALUE id ... WITH INDEX fact_superseded_by_idx`
  and updates only those records, repeating the status conditions. On a copy
  of the production graph (13,367 facts) a retraction with nothing to revive
  took 549 ms before and 53 ms after, one that revives a predecessor 590-660 ms
  and 105-117 ms. On production a fact retraction took 980 ms (median) against
  78 ms for an edge, and a document update can retract more than a hundred.
- [`search-throttle-limit.patch`](../deploy/brain/search-throttle-limit.patch)
  makes the per-credential limit of `POST /v1/search` configurable with
  `THROTTLE_SEARCH_LIMIT`. Upstream hardcodes 10 searches a minute on the
  route, so `THROTTLE_EXPENSIVE_LIMIT` does not reach it, and Manifold uses one
  Brain credential for every agent: the eleventh search in a minute received
  `429`, and Manifold reported Brain as a degraded dependency of that search
  or context request. A value that is not a positive integer keeps 10. A unit
  spec reads the route's throttle metadata.
- [`pool-stall-liveness.patch`](../deploy/brain/pool-stall-liveness.patch)
  makes `GET /health` answer `503` with `state: "pool_stalled"` when a
  SurrealDB connection pool has no idle connection and every acquire has timed
  out, without one success, for `SURREALDB_POOL_STALL_MS` (2 minutes by
  default; it must not be shorter than `SURREALDB_ACQUIRE_TIMEOUT_MS`).
  surrealdb-js 2.0.8 does not reject a query that is in flight when its socket
  drops. On 2026-11-21 SurrealDB was OOM-killed while every root-pool
  connection carried a query; the sockets reconnected, but the queries never
  settled, `withCompany` never released the leases, and every later acquire
  timed out for two days while `/health` answered `ok`. Brain cannot release
  such a lease itself, so the failing liveness probe lets the
  [container watchdog](operations.md#brain-liveness) replace the process.
  A pool that hands released connections to its waiters is busy, not stalled,
  and keeps `/health` green. Unit specs cover the stall, its reset, a
  saturated pool that recovers, the scoped pool, and the `503` body.
  Brain v2.11.0 ships surrealdb-js 2.2.0, which does reject such a query:
  tested on 2026-11-27 with a restart and with `SIGKILL` of SurrealDB under a
  `SLEEP 30s` query, the query failed at once with `CallTerminatedError` and
  the next query ran after the reconnect. The patch stays for the cases where
  no close reaches the client, such as a lost network path.
- [`unindexed-tenant-vector-spec.patch`](../deploy/brain/unindexed-tenant-vector-spec.patch)
  keeps an end-to-end check that a vector-only search finds a fact on a tenant
  without a ready HNSW index. SurrealDB 3.3 answers such a KNN query with no
  rows; upstream v2.11.0 reads the index catalogue and uses the exact scan.
  Test only.
- [`provider-refusal.patch`](../deploy/brain/provider-refusal.patch) keeps the
  reason when the model provider refuses extraction. Upstream classifies
  `402` and other refusals for search and synthesis, not for document
  extraction. `src/ai/provider-refusal.ts` maps upstream's classes to `billing`,
  `auth`, `rate_limit`, and `outage` for errors that carry an HTTP status; a
  timeout without one still splits the text and retries. A refusal fails the
  run with `model provider refused the request (provider_refusal=<class>)`,
  stops text splitting, and stops the batch. A `billing` or `auth` refusal also
  stops the retry passes until an operator acts. `GET /v1/documents/:id`
  returns the class as `providerRefusal` on the failed run, and Manifold reports
  `provider_credits_exhausted` from it. Unit and end-to-end specs cover the
  classes, the rethrow, the batch stop, and the ledger field.
- [`settle-fact-entity-index.patch`](../deploy/brain/settle-fact-entity-index.patch)
  is migration 9001. Ingest in v2.11.0 settles facts with `fn::settle_fact`
  (0177), whose candidate query and `competing` updates scan the whole fact
  table inside a function body. The migration redefines the function unchanged
  except for `WITH INDEX fact_entity_idx` and record-targeted updates. A unit
  spec requires the body to equal 0177 with those edits.
- [`selective-entity-fact-indexes.patch`](../deploy/brain/selective-entity-fact-indexes.patch)
  is migration 9002 plus a source guard test. It removes `fact_user_idx` and
  `entity_user_idx` (per-user memory indexes this deployment never uses) and
  adds element indexes `aliases[*]` and `nameKeys[*]`. SurrealDB 3 indexes an
  array field as a single value, so `aliases CONTAINS $x` and
  `nameKeys CONTAINS $x` scanned all entities (46 ms per lookup on a copy of
  the production graph, 0 ms with the element indexes). A deployment with
  per-user memory should re-create the two removed indexes.
- [`derived-stale-cascade.patch`](../deploy/brain/derived-stale-cascade.patch)
  is migration 9003. `fn::mark_derived_stale` and `fn::cascade_retract` find a
  fact's dependents with `UPDATE ... WHERE derivedFrom CONTAINSANY $parents`,
  which scans the table; they now find them with a SELECT served by a new
  element index on `derivedFrom[*]` (2 ms instead of 330 ms) and update only
  those rows. Inside a `DEFINE EVENT` the planner cannot use the index for any
  spelling of the lookup (280-470 ms), so the migration removes the
  `fact_staleness` event (0089). Upstream documents that removal: the nightly
  drain then marks stale derived summaries instead of the write that superseded
  their source. Manifold never reads derived summaries. Restore the event with
  0089's definition to get write-time marking back at the cost of one table
  pass per superseded fact. The patch also skips the event's e2e spec.
- [`derived-version-index.patch`](../deploy/brain/derived-version-index.patch)
  is migration 9004 plus a source guard test. It removes
  `fact_derived_version_idx` (0074), which serves derived worlds. Without them
  `derivedVersion IS NONE` matches every fact, and SurrealDB 3.3 still chose
  this index for entity reads: after the v2.11.0 upgrade one entity's candidate
  read took 39 s on 51,000 facts, against 1.3 s through `fact_entity_idx`, and
  hybrid search had a 12 s median. A deployment that builds derived worlds
  should re-create the index.
- [`ledger-release.patch`](../deploy/brain/ledger-release.patch) releases the
  migration ids that earlier Manifold builds used for their own files.
  `0168_resolve_fact_entity_index`, `0169_selective_entity_fact_indexes`, and
  `0170_derived_from_element_index` collide with upstream's 0168–0170, so the
  migrator deletes those ledger rows by file name before it applies migrations.
  Local migrations now start at 9001 and are DDL only and idempotent.
- [`predicate-vocabulary-refresh.patch`](../deploy/brain/predicate-vocabulary-refresh.patch)
  stops each committed document from forcing the next extraction to recount the
  predicate vocabulary. The count scans every active fact: on a graph of about
  50,000 facts its median was 6.3 seconds and its maximum 138 seconds, inside
  the document's extraction. Once the vocabulary holds its full 40 entries, Brain now serves
  the cached list and refreshes it in the background at most every 10 minutes.
  A smaller vocabulary is still recounted after each commit, so a new tenant
  sees its predicates at once. A unit spec covers the stale read.
- [`same-name-check-flag.patch`](../deploy/brain/same-name-check-flag.patch)
  adds `INGEST_SAME_NAME_CHECK` around upstream's v2.11.0 check of same-named
  entities (upstream #821). With the check, a mention whose facts contradict the
  entity of the same name creates a second entity: a document update from
  "Mira Chen works at Meridian Labs" to "... Borealis Labs" produced
  `mira-chen-2` and left the old facts on the first node. Manifold's dedup pass
  is off, so such a split stays. Upstream's default stays on; the frozen
  defaults of the image set it to `0`, which reuses the first entity of that
  name as before. Set `INGEST_SAME_NAME_CHECK: "1"` on the Brain service in a
  Compose override where two different people with one name matter more than
  facts that change.
- [`opt-in-boot-reread.patch`](../deploy/brain/opt-in-boot-reread.patch) stops
  v2.11.0 from re-reading every stored document at start. Upstream enqueues
  that walk on each boot of a new day after its extraction contract changes.
  Manifold stores each revision as its own Brain document, so superseded and
  deleted revisions were read again: their facts replaced older current facts
  in Brain, and Manifold never shows a fact that its own jobs did not project.
  The walk now needs `REINDEX_ON_PACK_INSTALL=1`, upstream's opt-in for a
  backfill after a pack upgrade. Leave it unset, and do not call
  `POST /v1/admin/documents/reindex` for the general indexer: both read old
  revisions again. Failed runs are still retried by the nightly sweep. A unit
  spec covers both settings.
- [`bounded-name-scans.patch`](../deploy/brain/bounded-name-scans.patch) lets
  at most four name scan statements run at once. A name scan reads every entity
  of a type with its embedding, about 0.3 GiB in SurrealDB on 9,000 entities,
  and a commit asks about every mention of a document at the same moment. On
  production 37 statements overlapped and SurrealDB was killed at its 10 GiB
  limit. Judge calls are not limited, and the same candidates reach them. A
  large commit can take longer. A unit spec covers the bound and a failed
  statement.
- [`single-resolution-pass.patch`](../deploy/brain/single-resolution-pass.patch)
  runs one fact resolution pass per tenant at a time. Every commit asks for a
  pass (upstream #828), and passes that overlapped read the same oldest facts,
  each holding about 1 GiB in SurrealDB. Requests made during a pass share the
  one that follows it, so the facts of those commits are still judged. A unit
  spec covers overlap, the following pass, and a failed pass.

Manifold submits `POST /v1/ingest/document` with `mode: "async"`,
`storeContent: true`, the general indexer, tenant context, and a canonical
`originUri` of `manifold://documents/<slug>@rN`. Brain returns its durable
document ID; the Manifold worker stores that ID as its checkpoint and polls
`GET /v1/documents/:id` and `GET /v1/documents/:id/candidates`. It considers
the run ledger and candidate statuses when deciding completion. Brain can
leave the document header at `indexing` after all runs and candidates have
settled, so the header status alone is not a completion signal.
The worker first polls after 250 ms and doubles the wait up to
`MANIFOLD_BRAIN_POLL_INTERVAL` (2 seconds by default). Rate-limited durable
submissions, polls, and graph hydration honor Brain's `Retry-After` within the
extraction deadline; interactive reads and legacy writes remain single-attempt.

Committed entity and fact candidates are hydrated through the entity and fact
read APIs, up to six reads at a time and once per distinct committed ID.
Relations are resolved by exact committed edge ID and endpoints through entity
connections. A later document can confirm an existing edge;
its committed candidate provides the new source revision while the edge keeps
its original source. The adapter skips candidates Brain redacts and
does not use search's `sourceKey` as a document reference. Manifold projects
the resulting entities, facts, and relations into its stable public graph IDs
with the exact source document revision. Brain conflict resolution may run
while facts are committed, but Manifold does not project a separate Brain
conflict or belief object.

Async indexing requires stored source text. Manifold therefore sends
`storeContent: true`, which retains normalized source chunks in Brain after
extraction. Manifold's service key has read/write access, not `brain:admin`;
Brain's `DELETE /v1/documents/:id/content` purge route requires the admin
scope. Manifold document deletion removes its OpenViking source and withdraws
the public graph projection, but it does not purge Brain's retained chunks.
The Brain document header, content hash, and candidate audit rows can remain
after a Brain content purge as well. Treat this as a known retention boundary
when reviewing deletion or retention policy.

Deletion and every new revision retract, through `POST /v1/facts/:id/retract`,
the Brain facts that the document claimed and that no live document claims at
its current revision any more. Facts that another live document corroborates
stay active. The retraction reason is `manifold_source_withdrawn`, never
`superseded`: Brain revives a predecessor only when that predecessor was
retracted as superseded. When retracting a successor revives a predecessor
that is also unclaimed, Manifold retracts the predecessor again. Brain answers
404 for a fact that no longer exists, which counts as done, and 403 for the
predicates it reserves for `brain:admin`; Manifold logs and skips those. A
transient failure fails the job, which retries. Relations follow the same
rule through the same route with `edge-retraction.patch`: after the facts, the
job retracts the relations that no live document claims, and a relation that a
later document states again is reopened in Brain. Facts and relations of
documents deleted before this behavior existed stay active in Brain until they
are retracted separately.

Compose sets `DECISIONS_LANES` to an empty string: a decision lane such as
`triage` can keep a document raw, with a `skipped` run and no extraction, while
Manifold counts the document as read.

Compose enables Brain's HNSW vector index (`SEARCH_HNSW_ENABLED`); see
[operations](operations.md#brain-vector-index-and-slow-query-log). On
v2.11.0 with this series, Brain's unit suite passes (642 suites, 7,440 tests on
2026-11-27) on SurrealDB 3.3.5.

Brain has no published host port. Manifold and SurrealDB communicate with it
over the private Compose network; Brain also joins the provider network for
outbound model requests. The Compose service enables
`DOCUMENT_INGEST_ENABLED`, `DOCUMENT_MULTI_INDEXER_ENABLED`, and
`FACTS_API_ENABLED` for this integration.

To verify the patch chain against a clean source checkout:

```bash
brain_src=$(mktemp -d)
git init -q "$brain_src"
git -C "$brain_src" remote add origin https://github.com/inite-ai/inite-brain-service.git
git -C "$brain_src" fetch -q --depth 1 origin 6ba986054a546c71c93c39e0e118e0f3dddc983f
git -C "$brain_src" checkout -q FETCH_HEAD
test "$(git -C "$brain_src" rev-parse HEAD)" = 6ba986054a546c71c93c39e0e118e0f3dddc983f
"$PWD/scripts/brain-apply-patches.sh" "$brain_src"
git -C "$brain_src" diff --check
```
