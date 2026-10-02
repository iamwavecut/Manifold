# INITE Brain v2.2.0 integration

Manifold builds INITE Brain from tag `v2.2.0`, pinned to commit
`b19b209fec92069d91df22af61db18b0a810ce82`. The exact ref, base-image
digests, and patch application order live in
[`deploy/brain/Dockerfile`](../deploy/brain/Dockerfile). The build uses the
Node 22 runtime from that release's upstream Dockerfile and an Alpine source
stage pinned by digest.

The downstream patches are narrow and covered by the patch-application check
in [CI](../.github/workflows/ci.yml):

- [`openai-base-url.patch`](../deploy/brain/openai-base-url.patch) passes
  `OPENAI_BASE_URL` into the shared OpenAI client and accepts API keys that do
  not use OpenAI's `sk-` prefix.
- [`scoped-session-recovery.patch`](../deploy/brain/scoped-session-recovery.patch)
  restores the scoped SurrealDB session after websocket reconnects. It retries
  a bounded signin, replaces a dead connection when needed, and restores its
  namespace/database selection. First-boot schema migrations still use the
  root connection before the scoped user is available.
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
  pack version. The queued payload pins that version for execution-time
  validation. The worker rechecks source availability after extraction and
  before automatic commit; conditional header writes preserve `purged` during
  concurrent extraction or commit. Purge does not retract already committed
  knowledge or change manual candidate review. A commit that passed its content
  check before a concurrent purge can still finish graph writes; serializing
  those operations would require a shared transaction or lock across the
  commit pipeline. The conditional header write still preserves `purged`.
- [`worker-startup-order.patch`](../deploy/brain/worker-startup-order.patch)
  starts the worker scheduler in `onApplicationBootstrap`, after module-owned
  job handlers register in `onModuleInit`. This prevents an early lease tick
  from setting the one-time loop-start guard before the document handlers are
  available.
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
  budget. Truncated responses, malformed JSON, missing arrays, and failed
  extraction passes fail the durable run instead of producing a successful
  empty graph. Valid explicit empty results remain valid. Errors contain no
  model output. Reprocess earlier false-success runs through new revisions.

Manifold submits `POST /v1/ingest/document` with `mode: "async"`,
`storeContent: true`, the general indexer, tenant context, and a canonical
`originUri` of `manifold://documents/<slug>@rN`. Brain returns its durable
document ID; the Manifold worker stores that ID as its checkpoint and polls
`GET /v1/documents/:id` and `GET /v1/documents/:id/candidates`. It considers
the run ledger and candidate statuses when deciding completion. Brain v2.2 can
leave the document header at `indexing` after all runs and candidates have
settled, so the header status alone is not a completion signal.
The worker polls every ten seconds. Rate-limited durable submissions, polls,
and graph hydration honor Brain's `Retry-After` within the extraction deadline;
interactive reads and legacy writes remain single-attempt.

Committed entity and fact candidates are hydrated through the entity and fact
read APIs. Relations are resolved by exact committed edge ID and endpoints
through entity connections. A later document can confirm an existing edge;
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
scope. Manifold document deletion currently removes its OpenViking source and
withdraws the public graph projection, but it does not purge Brain's retained
chunks. The Brain document header, content hash, and candidate audit rows can
remain after a Brain content purge as well. Treat this as a known retention
boundary when reviewing deletion or retention policy.

Brain has no published host port. Manifold and SurrealDB communicate with it
over the private Compose network; Brain also joins the provider network for
outbound model requests. The Compose service enables
`DOCUMENT_INGEST_ENABLED`, `DOCUMENT_MULTI_INDEXER_ENABLED`, and
`FACTS_API_ENABLED` for this integration.

To verify the patch chain against a clean source checkout:

```bash
brain_src=$(mktemp -d)
git clone --depth 1 --branch v2.2.0 https://github.com/inite-ai/inite-brain-service.git "$brain_src"
test "$(git -C "$brain_src" rev-parse HEAD)" = b19b209fec92069d91df22af61db18b0a810ce82
git -C "$brain_src" apply --check "$PWD/deploy/brain/openai-base-url.patch"
git -C "$brain_src" apply "$PWD/deploy/brain/openai-base-url.patch"
git -C "$brain_src" apply --check "$PWD/deploy/brain/scoped-session-recovery.patch"
git -C "$brain_src" apply "$PWD/deploy/brain/scoped-session-recovery.patch"
git -C "$brain_src" apply --check "$PWD/deploy/brain/document-origin-identity.patch"
git -C "$brain_src" apply "$PWD/deploy/brain/document-origin-identity.patch"
git -C "$brain_src" apply --check "$PWD/deploy/brain/failed-run-retry.patch"
git -C "$brain_src" apply "$PWD/deploy/brain/failed-run-retry.patch"
git -C "$brain_src" apply --check "$PWD/deploy/brain/worker-startup-order.patch"
git -C "$brain_src" apply "$PWD/deploy/brain/worker-startup-order.patch"
git -C "$brain_src" apply --check "$PWD/deploy/brain/corroborated-commit-ref.patch"
git -C "$brain_src" apply "$PWD/deploy/brain/corroborated-commit-ref.patch"
git -C "$brain_src" apply --check "$PWD/deploy/brain/extraction-completion.patch"
git -C "$brain_src" apply "$PWD/deploy/brain/extraction-completion.patch"
git -C "$brain_src" diff --check
```
