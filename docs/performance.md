# Performance and footprint

This page records what the embedded index (v0.3.0) costs in memory, disk and
time, and how to measure it again. Measurements before v0.3.0, when OpenViking,
Brain and SurrealDB did this work, are in the v0.2.2 tree of this file.

## Stages of one document sync

1. A worker picks the job up (`MANIFOLD_WORKER_INTERVAL`, 1 s).
2. The index stores the revision text in chunks of at most 1,500 characters and
   records a snapshot checkpoint. This is local SQLite work.
3. Extraction: one `json_schema` chat call per section of at most 12,000
   characters, up to `MANIFOLD_EXTRACTION_CONCURRENCY` (4) at once. This is the
   only stage that waits for the provider.
4. Resolution in one index transaction: entities, facts, supersession,
   relations. Manifold then projects the result into `manifold.db`.
5. The background embedder gives the new chunks and facts vectors, 64 texts
   per provider call. The job does not wait for it.

## Production after the data version 6 migration (2026-11-28)

Measured on the production host right after the switch, with the OpenRouter
provider (`deepseek/deepseek-v4.1-flash-1017` for extraction,
`perplexity/pplx-embed-v1-0.6b` at 512 dimensions for embeddings). The corpus
held 202 documents, 9,130 entities, 46,525 facts and 11,101 relations.

| Measure | Result |
| --- | --- |
| Seed of a new `index.db` | 8.9 s |
| Embedding backfill | 20,594 texts (1,315 chunks, 19,279 live facts) in about 95 s |
| Container memory after the backfill | 47.8 MiB anonymous, 105 MiB with page cache (cgroup `memory.stat`) |
| Container memory peak (seed and backfill, with page cache) | 225 MiB |
| `index.db` | 52.5 MB, WAL 4 MB; `manifold.db` 98 MB |
| Sync of a short new document, extraction included | 2.0 s |
| Search on a copy of production, first query of a text | hybrid 261 ms (includes the query embedding call), semantic 67 ms, graph 71 ms |
| Host memory in use | 11.0 GB before the switch, 5.1 GB after |

The previous stack used 6.0 GiB at the moment of the switch (SurrealDB
3.36 GiB, Brain 1.81 GiB, OpenViking 0.85 GiB, Manifold 20 MiB) and 4.3 GB of
derived data on disk (SurrealDB 3.6 GB, Brain cache 622 MB, OpenViking 91 MB).
The service was unavailable for 86 seconds, most of it the cold backup of the
old volumes.

## Footprint on a production-sized corpus (2026-11-28)

The corpus is synthetic, shaped like production on 2026-11-28: 214 documents
with 766 revisions and 8.45 MB of current text, 9,098 entities, 46,260 facts
(19,245 active, 4,676 superseded, 22,339 retracted) and 11,036 relations. Its
`manifold.db` is 81 MB (production: 98 MB). The image ran in Docker on a
12-core workstation with `--memory 1g`, `GOMEMLIMIT=384MiB`, 512-dimension
vectors and the deterministic test provider, which answers at once. A real
provider adds its own latency to extraction, to the embedding backfill and to
every query embedding that the query cache does not hold.

| Measure | Result |
| --- | --- |
| Image size | 15.6 MB |
| Seed of a new `index.db` | 2.7 s |
| Embedding backfill | 30,461 texts (6,540 chunks, 23,921 live facts) in 3.6 s, 476 provider calls |
| `index.db` | 50 MB, WAL at most 4 MB |
| Container memory, peak during seed and backfill | 143 MiB (1-second samples) |
| Container memory after 900 searches and 20 s idle | 50 MiB |
| Native peak RSS of the whole run (macOS `time -l`) | 170 MiB |
| `hybrid` search, 300 random three-word queries | p50 47 ms, p95 57 ms |
| `graph` search | p50 22 ms, p95 27 ms |
| `semantic` search | p50 27 ms, p95 31 ms |

For comparison, production on the same date ran the previous stack at about
10.2 GiB of idle memory (SurrealDB 7.55 GiB, Brain 1.8 GiB, OpenViking
0.82 GiB, Manifold 29 MiB), and SurrealDB alone used 4.5 GiB of disk.

Space in `index.db` by table: fact vectors 12.0 MB, chunk text 11.0 MB, facts
7.8 MB, stored extraction results 7.1 MB, chunk vectors 3.5 MB, the rest under
2 MB each. Superseded facts keep their vectors because history search uses
them.

The backfill of this corpus sends about 2.3 million tokens (about four
characters per token). At $0.02 per million tokens that is about $0.05; check
the provider's current price before a backfill.

## Measuring again

The synthetic corpus generator is a test behind the `footprint` build tag:

```bash
FOOTPRINT_DB=/tmp/footprint/manifold.db go test -tags footprint -run TestWriteFootprintCorpus ./internal/store
```

Start Manifold on that file with the deterministic provider
(`go run ./test/integration/provider`, port 8081), wait for the
`index embedded` log line, then sample `docker stats` or `/usr/bin/time` and
send searches through `POST /api/v1/search`. Delete the corpus afterwards; it
is large and serves no other purpose.
