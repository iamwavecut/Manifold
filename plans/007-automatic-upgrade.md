# Automatic upgrade from every earlier release

**Goal:** An operator of any earlier release (v0.2.0 and later, data versions 1–5) upgrades by starting the current image on the same `manifold-data` volume. Manifold then brings all canonical data into the embedded index and a consistent public graph by itself, with no manual backfill.

**Why:** The data version 2 procedure required reviewed same-content updates of every `ready` document, and the data version 6 procedure required manual steps. An external operator can follow neither reliably.

## What older data looks like

| Release | `manifold.db` graph | Gap after the seed |
| --- | --- | --- |
| v0.2.0–v0.2.2 (data version 1) | Only facts and relations created through the API, with Brain IDs when the Brain call succeeded; the derived graph lived in Brain | Every `ready` document has an empty projected result; API facts without an index ID are not indexed |
| Data versions 2–5 | Derived graph with claims and fact snapshots | Revisions whose claims are missing; derived facts and relations that lost every current claim but were never retracted (production on 2026-11-28: 1,073 facts and 342 relations from 13 documents) |

The schema of every earlier release is a subset of the current one (`CREATE TABLE IF NOT EXISTS` adds the claim tables), the volume name and path are unchanged, and the job kinds are the same.

## Decisions

| Topic | Decision | Reason |
| --- | --- | --- |
| Where | One idempotent catch-up pass at every start, after the seed, in the background | The same code repairs an interrupted upgrade and a damaged index; normally it finds nothing |
| Documents without a graph | The seed stores no result for a revision whose projection is empty. The catch-up queues an ordinary `document.sync` job for the current revision of each live `ready` document that has no stored result and no unfinished job | Re-extraction from the canonical text is the only source of the old Brain graph; the durable job path already handles provider refusals, retries, restarts and newer revisions |
| API facts and relations without an index ID | The catch-up gives them one through the same graph calls that API creation uses | Without it they never reach graph search |
| Orphans | The catch-up runs the existing orphan retraction for every document that is deleted, or `ready` with no unfinished job, and once claimed a record that no live document claims now | Same semantics as after a sync; a document in the middle of an update is skipped, because its old claims are not current yet |
| Cost | Re-extraction calls the provider once per section of each affected document; data versions 2–5 need almost none | An operator of v0.2.x pays one extraction of the live corpus, as the old manual backfill did |

## Tasks

1. Seed: skip empty projected results. Engine: report whether a revision has a stored result.
2. Service catch-up: queue missing extractions; index API facts and relations without an index ID; retract orphans. Start it from `cmd/manifold`.
3. Tests: a database shaped like v0.2.2 upgrades end to end; repeated starts queue nothing twice; orphans of a ready document are retracted and those of a document with a pending update are not.
4. Documentation: upgrade from any release for operators, README.
5. Verification: unit tests, `make verify`, `make integration`, a local upgrade of a database built with the v0.2.2 schema.

## Result (2026-11-28)

All five tasks are implemented. Rulings made during execution:

| Ruling | Reason | Cost if wrong |
| --- | --- | --- |
| The catch-up runs in the background beside the workers, not before listening | Readiness must not wait for orphan retraction or queueing | A search right after start can still rank an orphan that the public reads hide anyway |
| A record whose entity the public graph hides is skipped, not reported | Otherwise every start logs the same failure for a relation to a derived entity that no document states | Such a record stays out of graph search, as its entity does |
| No rehearsal on a copy of production; the tests, a local upgrade of a v0.2.2-shaped database and the post-deploy checks cover it | The user asked for no rehearsals; the production effect is the retraction of records that public reads already hide | A wrong retraction needs a re-sync of the affected documents |
| `failed` documents of an earlier release are left to the operator's retry | Their stored errors may need a decision (provider, content) that the pass cannot make | They stay without a graph until retried |

Verification: unit tests of engine, store and service, `make verify`, `make integration` (deterministic provider only), and an upgrade of a database built with the exact v0.2.2 schema (two documents extracted again, one API fact and one API relation indexed, a deleted and a failed document left alone, a second start with nothing to do).
