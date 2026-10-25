# INITE Brain integration

Manifold builds INITE Brain from upstream commit
`a567f6960c031b916549699fec7b89423c650d1e`: `main` after v2.3.0 (39 later commits, the last
on 2026-09-26). Brain has published no release since v2.3.0. `main` adds the
batched entity judge for document commits (#705) and migrations 0161-0167;
they are one-way, like every Brain migration. The exact ref, base-image digests, and patch application
order live in [`deploy/brain/Dockerfile`](../deploy/brain/Dockerfile). The build
uses Node 22 LTS, the major line of upstream's Dockerfile, and an Alpine
source stage pinned by digest.

The downstream patches are narrow and covered by the patch-application check
in [CI](../.github/workflows/ci.yml):

- [`openai-api-key-format.patch`](../deploy/brain/openai-api-key-format.patch)
  accepts API keys that do not use OpenAI's `sk-` prefix. Upstream `main`
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
  validation. The worker rechecks source availability after extraction and
  before automatic commit; conditional header writes preserve `purged` during
  concurrent extraction or commit. Purge does not retract already committed
  knowledge or change manual candidate review. A commit that passed its content
  check before a concurrent purge can still finish graph writes; serializing
  those operations would require a shared transaction or lock across the
  commit pipeline. The conditional header write still preserves `purged`.
- [`worker-startup-order.patch`](../deploy/brain/worker-startup-order.patch)
  starts the worker scheduler in `onApplicationBootstrap`, after module-owned
  job handlers register in `onModuleInit`. In v2.3 an early lease tick with no
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
- [`resolve-fact-entity-index.patch`](../deploy/brain/resolve-fact-entity-index.patch)
  is migration 0168: `fn::resolve_fact` (and the unchanged batch mapper
  `fn::resolve_facts`) with `WITH INDEX fact_entity_idx` on the `$candidates`
  query, and with the two `UPDATE knowledge_fact SET status = 'competing'
  WHERE id IN ...` statements rewritten as direct record updates. Inside a
  function body the planner scans the whole fact table for both forms (the
  `WHERE` runs against every row even for an empty list). A document commit
  calls the function once per fact. On the copy of the production graph an
  inserting or corroborating resolve fell from 290 ms to 10-15 ms and a
  superseding one from 1.1 s to 24 ms; the production VPS took 0.8-1.2 s for
  the candidate query alone. Parameters are unchanged, so the rolling-deploy
  signature gate still holds.
- [`selective-entity-fact-indexes.patch`](../deploy/brain/selective-entity-fact-indexes.patch)
  is migration 0169 plus a source guard test. It removes `fact_user_idx` and
  `entity_user_idx` (per-user memory indexes this deployment never uses) and
  adds element indexes `aliases[*]` and `nameKeys[*]`. SurrealDB 3 indexes an
  array field as a single value, so `aliases CONTAINS $x` and
  `nameKeys CONTAINS $x` scanned all entities (46 ms per lookup on the copy,
  0 ms with the element indexes). A deployment with per-user memory should
  re-create the two removed indexes.
- [`derived-stale-cascade.patch`](../deploy/brain/derived-stale-cascade.patch)
  is migration 0170. `fn::mark_derived_stale` and `fn::cascade_retract` find a
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
- [`edge-retraction.patch`](../deploy/brain/edge-retraction.patch) lets
  `POST /v1/facts/:id/retract` take a `knowledge_edge:` id. Brain `main` serves
  every live edge of a matched entity as a fact-shaped line in search (the
  relation leg), and v2.3 had no way to withdraw an edge, so the relation of a
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
- [`knn-empty-result-fallback.patch`](../deploy/brain/knn-empty-result-fallback.patch)
  makes the search vector leg fall back to the exact scan when SurrealDB 3.3
  answers a KNN statement with no rows and the HNSW index is absent, building,
  or failed. SurrealDB 3.2.4, which upstream measured, returns unranked rows
  with a null distance in that state, and Brain detects only that shape; 3.3.0
  plans a table scan with a KNN filter and returns nothing, so the vector leg
  silently returned nothing for an unindexed tenant. With a ready index an
  empty result still means the filters removed every candidate, and the leg
  stays empty as upstream intends. The patch adjusts the HNSW unit and e2e
  specs to both engine versions; the e2e spec now also checks a vector-only
  search on a tenant without the index.
- [`hnsw-provision-early-tenants.patch`](../deploy/brain/hnsw-provision-early-tenants.patch)
  makes Brain build the HNSW indexes of a tenant that it opened during startup.
  Brain provisions a tenant when it reports the tenant's schema as ready, but
  `CalibrationService` opens the host tenant in its own `onModuleInit`, before
  `AdminModule` registers the provisioning listener. That notice is not
  repeated, so the `manifold` tenant never received an index and the vector
  leg kept the exact scan. The patch adds `SurrealService.readyTenantIds()`, and
  the provisioning service notes those tenants when it registers. A unit spec
  covers a tenant migrated before the listener.
- [`search-throttle-limit.patch`](../deploy/brain/search-throttle-limit.patch)
  makes the per-credential limit of `POST /v1/search` configurable with
  `THROTTLE_SEARCH_LIMIT`. Upstream hardcodes 10 searches a minute on the
  route, so `THROTTLE_EXPENSIVE_LIMIT` does not reach it, and Manifold uses one
  Brain credential for every agent: the eleventh search in a minute received
  `429`, and Manifold reported Brain as a degraded dependency of that search
  or context request. A value that is not a positive integer keeps 10. A unit
  spec reads the route's throttle metadata.

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

Compose enables Brain's HNSW vector index (`SEARCH_HNSW_ENABLED`); see
[operations](operations.md#brain-vector-index-and-slow-query-log). With these
patches, Brain's HNSW, retraction, and supersession e2e specs pass on SurrealDB
3.3.0 (23 tests), and its HNSW provisioning and search throttle unit specs
pass.

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
git -C "$brain_src" fetch -q --depth 1 origin a567f6960c031b916549699fec7b89423c650d1e
git -C "$brain_src" checkout -q FETCH_HEAD
test "$(git -C "$brain_src" rev-parse HEAD)" = a567f6960c031b916549699fec7b89423c650d1e
for patch in openai-api-key-format document-origin-identity worker-startup-order corroborated-commit-ref failed-run-retry extraction-completion fatal-signal-default openai-chat-extra-body entity-fact-read-index-hints resolve-fact-entity-index selective-entity-fact-indexes derived-stale-cascade edge-retraction revive-superseded-index knn-empty-result-fallback hnsw-provision-early-tenants search-throttle-limit; do
    git -C "$brain_src" apply --check "$PWD/deploy/brain/$patch.patch"
    git -C "$brain_src" apply "$PWD/deploy/brain/$patch.patch"
done
git -C "$brain_src" diff --check
```
