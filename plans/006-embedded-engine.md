# Embedded graph engine: replace Brain, SurrealDB and OpenViking

**Goal:** Run Manifold as one Go process with SQLite. Remove the Brain, SurrealDB and OpenViking containers while the public API, IDs and stored history stay the same.

**Why:** On 2026-11-28 the stack used about 10.2 GiB of RAM (SurrealDB 7.55 GiB idle with peaks at its 10 GiB limit, Brain 1.8 GiB, OpenViking 0.82 GiB, Manifold 29 MiB) and 4.5 GiB of SurrealDB disk for 202 live documents with 1.41 MB of current text. A survey of ready engines found no Go replacement that keeps Manifold's fact model (Manifold doc `manifold-graph-engine-alternatives-2026-11-28`).

## Architecture

- `internal/llm`: a small OpenAI-compatible client (chat completions in JSON mode, embeddings) with retries and provider-refusal classes (`billing`, `auth`, `rate_limit`, `outage`).
- `internal/engine`: one SQLite file `index.db` beside `manifold.db`. It holds derived data only and can be rebuilt:
  - the graph: entities, aliases, facts, relations, FTS5 tables;
  - vectors: `sqlite-vec` `vec0` tables from `modernc.org/sqlite/vec`, `int8[512]` with cosine distance, quantized per vector by its largest component (cosine ignores the scale);
  - the document index: path registry and text chunks with vectors.
- `engine.Graph` implements `upstream.GraphStore`. `engine.Documents` implements `upstream.DocumentStore`. The service keeps its interfaces, projection, claims and retraction logic.
- The engine keeps the upstream IDs it seeds from Manifold's projection, so `facts.upstream_id`, `relations.upstream_id` and `entities.upstream_id` stay valid.

## Decisions

| Topic | Decision | Reason |
| --- | --- | --- |
| Vector size | 512 dimensions, `int8` | 40,000 vectors take about 20 MB and a brute-force KNN takes 22 ms (spike, Apple M-series); the top results matched `float32` |
| Embedding dimensions | Ask for 512; truncate and renormalize a longer reply | `text-embedding-3-*` are Matryoshka models; works when a proxy drops `dimensions` |
| Extraction | One JSON-mode chat call per chunk of up to 12,000 characters; up to 4 chunks in parallel | The average live document is 7 KB, so most documents need one call |
| Vocabulary | The 42 predicates and 10 entity kinds already in production; relation predicates stay free-form `snake_case` | New facts must corroborate or supersede the seeded ones |
| Fact resolution | Same entity, predicate and normalized value: reuse the fact (a retracted one becomes active again). A single-valued predicate with a new value supersedes the older active value. Everything else coexists | Brain's pack marks most predicates multi-valued; this needs no judge call |
| Entity resolution | Normalized name or alias, then the same kind first; extraction prompts list known entities that match the document's terms | Keeps one entity per name without an extra model call |
| Seeding | On first start, copy entities, facts (all statuses) and relations from `manifold.db`, and chunks of live documents. A background loop embeds rows that have no vector | No re-extraction, no lost history; the index rebuilds after a reset |
| Search | Graph: FTS5 + vector KNN over facts + facts of matching entities, fused with RRF. Documents: KNN over chunks, best chunk per document | Replaces Brain hybrid search and OpenViking `find` |
| Status components | `sqlite`, `index`, `provider` | Brain and OpenViking no longer exist |
| Search source names | `semantic` (document vectors) and `graph` | Appear in `degraded_dependencies` |
| Runtime image | `gcr.io/distroless/static-debian13:nonroot`, with a `manifold healthcheck` subcommand | No shell or package manager; the binary is static (`CGO_ENABLED=0`) |

## Tasks

1. Refresh dependencies and images: Go modules to latest, `golang:1.27.4-alpine3.24`, `node:26.13.1-alpine3.24`, distroless runtime.
2. `internal/llm` with tests against an `httptest` server.
3. `internal/engine` storage, quantization, document index, embedding backfill, with tests.
4. Graph engine: extraction, resolution, ingest, search, retraction and revival, manual facts and relations, with tests.
5. Seeding from the projection, with tests.
6. Wiring: configuration, `cmd/manifold`, status, search sources, provider refusals; remove the Brain and OpenViking clients and the asynchronous Brain path.
7. Compose, Dockerfile, CI, integration suite and test provider; remove `deploy/brain`, `deploy/openviking` and the Brain scripts.
8. Documentation, `AGENTS.md`, OpenAPI; `DATA_VERSION` 6.
9. Verification: `make verify`, Compose config, multi-arch build, `make integration`, footprint measured on a copy of production data with the deterministic provider.

## Review focus

1. A re-synced document that states the same facts must keep the same fact IDs, or Manifold retracts and recreates them on every revision.
2. Retracting a successor must revive the fact it superseded and report it, so Manifold's orphan retraction still works.
3. A provider refusal (402, 401/403) must block the document as before, not fail it permanently.
4. An embedding outage must not fail document syncs; the backfill loop must finish later.
5. A changed embedding model or dimension must rebuild the vectors instead of mixing spaces.

## Rollout

`DATA_VERSION` becomes 6. The first start seeds `index.db`; the old volumes stay untouched until the new build is verified, so rollback means starting the previous images. Production rollout needs the user's go-ahead: the frozen broker profile changes (services removed, provider settings moved to Manifold), and embedding the seeded rows calls the paid provider (estimated under $0.10).

## Result (2026-11-28)

All nine tasks are implemented on `feat/embedded-engine`. Rulings made during execution:

| Ruling | Reason | Cost if wrong |
| --- | --- | --- |
| Images use `alpine3.24`; no `alpine3.25` tags exist yet | Latest published tags on 2026-11-28 | A later rebuild picks a newer base |
| Entities created through the API seed by name (`manifold:<id>` key), not by index ID | They never had an index ID; their explicit facts were lost on reseed without it | A name collision joins two entities, as API calls already do |
| The seed links superseded values of single-valued predicates to the next newer value | `manifold.db` stores no `superseded_by`; without links, retracting a seeded current value revived nothing | A link can point to a value Brain did not record as the successor; only the order by `valid_from` decides |
| Provider health appears in `components` and does not change `status.state` | `docs/operations.md` documents it so: refusals block documents, which the pipeline reports | An operator who watches only `state` misses a provider outage |
| A provider answer after a failed embedding pass ends the embedder's backoff; answers during the pass do not | Otherwise vectors waited up to 30 minutes after the provider recovered, and the embedder's own successful batch retried a failing provider at once | An answer that arrives during a failing pass is ignored until the next one |
| `journal_size_limit` 4 MiB on `index.db` | The WAL kept its 35 MB seed peak | None measured |
| `scope` searches compare the query with every chunk under the prefix; without `scope`, the exact leading segments of `scope_glob` give the prefix | KNN over the whole corpus, then a filter, lost in-scope documents | Exact distances cost more on a scope that covers most of the corpus; a document whose locator does not follow its path would drop out of scoped semantic search (production locators were checked) |
| A search that waits for another caller's query embedding repeats the call when that caller gave up | A cancelled client request failed concurrent searches for the same text | One more provider call for that text |
| Footprint measured on a synthetic corpus shaped like production, not on a production copy | A copy would move private content off the host | Real text and name distributions can change sizes and timings |
| `docker buildx` ran once per platform | The local `docker` driver refuses a multi-platform build | None: each platform's build stage compiled |

Verification: `make verify`, `go test -race` for engine, service and store, `go vet` with the `integration` and `footprint` tags, both Compose configurations, `scripts/validate-compose.sh`, the release-request script tests, both platform builds and `make integration` (deterministic provider only; no real-provider run). Measurements are in `docs/performance.md`.

Production rollout (done 2026-11-28, release `2d899bb`): CI published the image and the broker refused it at the data-version boundary as expected. A run of the image on a copy of `manifold.db` beside production checked the seed, the real provider (structured extraction and Matryoshka embeddings at 512 dimensions) and search. The cutover then stopped the old stack, took a cold backup of the four old volumes with verified hashes, installed the frozen profile with only the `manifold` service and the provider settings from the old profile, set `profile_digest` and `data_version` 6, started Manifold, waited for the seed and the embedder, compared the record counts and adopted the release. A synthetic document passed create, update (the old fact disappeared) and delete. Measurements are in `docs/performance.md`. The old volumes and their backups stay on the host as the rollback point.
