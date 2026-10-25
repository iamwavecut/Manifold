# Document sync performance

A document write returns `202` in about 0.3 s. The `document.sync` job behind it
decides when `remember` finishes. This page records where that job spent its time
on 2026-10-14, what changed, and what is left. Numbers come from a copy of the
production graph (3,496 facts, 1,462 entities, 44 documents) on a 12-core
workstation, unless a line says production.

## Stages of one job

1. The Manifold worker picks the job up (`MANIFOLD_WORKER_INTERVAL`, 1 s).
2. OpenViking: waited `content/write` and snapshot. Its semantic summaries call
   the chat model several times.
3. Brain: `POST /v1/ingest/document`, then a queue wait, the extraction pass
   (chat model, three self-consistency samples in parallel), and the candidate
   commit (entity resolution, one `fn::resolve_fact` per fact, edges).
4. Manifold polls Brain, then reads every committed entity, fact, and
   connection back and projects them into SQLite.

## What was slow

Production, before any change (27 jobs since 2026-10-12): 79-662 s, median
251 s. The commit stage took 50-65 % and the read-back 10-35 % of that. The
language-model provider, the CPU, and the disk were not the limit. SurrealDB
3.3.0 planned Brain's graph queries as scans of whole tables:

| Pattern | Why it scans | Fix |
| --- | --- | --- |
| `userId IS NONE` on facts and entities | Matches every row when no per-user memory exists, and the planner picks that index over `entityId` | Drop `fact_user_idx`, `entity_user_idx` (0169); `WITH INDEX fact_entity_idx` on the profile query |
| `entityId = $entity_id` inside `fn::resolve_fact` | Function arguments are runtime values to the planner | `WITH INDEX fact_entity_idx` (0168) |
| `UPDATE ... WHERE id IN $list` inside the function | The `WHERE` runs against every row, even for an empty list | `UPDATE $list SET ...` (0168) |
| `entityId = $t.entityId` in a multi-statement query | A value computed earlier in the same query is not a constant | Read the key first, bind it as client parameters |
| `aliases CONTAINS $x`, `nameKeys CONTAINS $x` | SurrealDB 3 indexes an array field as one value | Element indexes `aliases[*]`, `nameKeys[*]` (0169) |
| `derivedFrom CONTAINSANY $parents` in `UPDATE` and in a `DEFINE EVENT` | An `UPDATE` ignores the index; inside an event no spelling uses it | Find rows with an indexed `SELECT`; remove the `fact_staleness` event (0170) |

Costs on the copy: an entity profile 278 ms, a resolve that inserts or
corroborates 290 ms, one that supersedes 1.1 s, an alias lookup 46 ms. After
the patches they take 1 ms, 10-15 ms, 24 ms, and 0 ms. The same queries ran
roughly 4-7 times slower on the production VPS.

Two settings added dead time in the new Brain: the batched extraction pass
gathers documents for 20 s (`EXTRACTION_BATCH_WINDOW_SECONDS`), and the job loops
sleep up to six seconds between claims (`WORKER_LOOP_POLL_MS`,
`WORKER_LOOP_EMPTY_BACKOFF_MS`). Manifold slept ten seconds between status
checks and read the graph back one request at a time.

## Results

Deterministic provider with zero latency, so the table shows the compute-bound
part. Mean of 2-3 jobs per cell, seconds. Medium: 36 entities, 40 facts, 26
relations, half of them new. Large: 83, 111, 47.

| Build | Medium | Large | Medium, all known |
| --- | --- | --- | --- |
| v2.3.0, OpenViking 0.4.22, previous Manifold | 37.0 | 97.9 | 47.8 |
| Brain `main` with its default 20 s window | 52.1 | 117.5 | 58.4 |
| `main`, window 1 s | 41.9 | 104.1 | 41.1 |
| + planner patches 0168-0170 and hints | 6.5 | 9.0 | 6.1 |
| + queue loops 250/750 ms, poll 2 s (final) | 4.7 | 8.0 | 5.8 |

Slow queries (over 20 ms) added up to 373 s across the eight baseline jobs and
3.7 s across the same eight jobs on the final build.

With the provider delays set to measured production values (embeddings 150 ms,
chat 300 ms, a 20 s extraction call, 2 s per OpenViking summary), the medium
job took 64 s before and 34 s after, the large job 128 s and 50 s.

## What is left

On the final build with those delays, a medium job spends about 24 s waiting for
the extraction call, 2.3 s in Brain's queue, 2.6 s in the commit, and 5.5 s in
OpenViking, polling, and read-back together. The remaining database cost is
`SELECT predicate, count() ... GROUP BY predicate` (200 ms, once per job, a scan
that feeds the extraction prompt) and `fn::resolve_fact` at 25-30 ms per fact.
Ideas that were not applied:

- Start the Brain submission while OpenViking indexes (hides the OpenViking
  stage, 8.6 s on average on production, behind the extraction call, but changes
  the failure semantics of the job).
- Cache or materialize the predicate statistics.
- Make the Manifold worker wake on enqueue instead of ticking every second.
- A smaller or faster extraction model, or one sample instead of three if
  quality allows; the wall time is the slowest sample.

## Search and retraction (2026-10-25)

Production grew from 3,572 to 13,054 Brain facts after 2026-10-14, and the
median `POST /api/v1/search` grew from 3.3 s to 6.8 s (`context` from 4.7 s to
7.6 s, with one call at the client's 30 s limit). Three causes, measured on
production and on a copy of its graph in an isolated SurrealDB 3.3.0 container
limited to three cores:

| Cause | Before | After |
| --- | --- | --- |
| Search vector leg read every fact row with its embedding (no HNSW index) | exact scan 5.1-5.3 s on production, 2.26 s on the copy | KNN 0.47 s on the copy, recall@100 0.998 against the exact scan |
| Fact retraction revived predecessors with a table-wide `UPDATE ... WHERE` | 980 ms per fact on production (78 ms per edge) | 53 ms with nothing to revive, 105-117 ms with a revival (copy) |
| Manifold verified the API key with argon2id on every request | 110-150 ms per authenticated request | one argon2id per secret and process |

Reading a fact row is the expensive part, not the cosine: on production
`count()` took 0.16 s, `count() WHERE embedding != NONE` 4.3 s. Only 5,350 of
the 13,054 facts were active. Of the 0.47 s KNN, about 50 ms is the index walk
and about 300 ms the search filters, which read each of the 400 candidates
(`SEARCH_HNSW_OVERFETCH=4` × 100) in full; 8× gave the same recall. A hybrid
search can run the retrieval pass up to three times (language backoff, entity
expansion).

In a 155 s update job before the changes, the extraction call took 20 s, the
candidate commit 41 s, the graph read-back 7 s, and 74 fact plus 39 edge
retractions 77 s. Entity resolution scans `knowledge_entity` by type (1.1 s for
the 1,874 `asset` entities); the slow-query log below attributes the rest of the
commit.

## Measuring again

- Compose sets `SURREAL_SLOW_QUERY_LOG_THRESHOLD` to 250 ms
  (`SURREALDB_SLOW_QUERY_LOG_THRESHOLD`) and hides parameter values; see
  [operations](operations.md#brain-vector-index-and-slow-query-log). Without
  the allow list SurrealDB logs every parameter value, including document text.
- `EXPLAIN FULL` after a query shows the operators; `IndexScan[fact_user_idx]`
  with `rows` equal to the table size is the scan described above.
- Brain records the stages: `source_document.createdAt`, the
  `extract_documents` row in `job_run` (`visibleAfter`, `claimedAt`,
  `finishedAt`), and the `createdAt` / `decidedAt` of the document's rows in
  `candidate`. Manifold logs `job started` and `job completed`.
- A change to a Brain query needs a plan check on a graph of production size;
  the small integration data set does not show these scans.
