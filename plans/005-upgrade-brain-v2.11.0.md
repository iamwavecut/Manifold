# Brain v2.11.0 Upgrade Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move Manifold from INITE Brain `a567f696` (v2.3.0 plus 39 commits) to release v2.11.0 without losing data, current behavior, or cost control.

**Architecture:** Keep the model "pinned upstream source plus narrow patches". Rebase the patches that upstream does not cover, drop the two that it now covers, move the local migrations out of upstream's id range, and freeze upstream's changed flag defaults in the image. Production moves by a manual `DATA_VERSION=5` activation with a restore-based rollback.

**Tech Stack:** Brain v2.11.0 (Node 24, NestJS 12, surrealdb-js 2.1.1, openai 7.43), SurrealDB 3.3.5, Docker multi-arch build, Go (Manifold adapter and integration suite), Python (repository scripts).

**Spec:** [`docs/brain-patch.md`](../docs/brain-patch.md) states the current integration contract and the purpose of each patch.

## Global Constraints

- Source pin: tag `v2.11.0`, commit `6ba986054a546c71c93c39e0e118e0f3dddc983f`. Do not use upstream `main`.
- Runtime image: `node:24-slim@sha256:55fb5f5a527ea2c5c4a95f963e48ef90aa38c7b346054fdc8ee9957a2b55d205` (upstream's digest). Copy upstream's `packs/` directory into the runtime stage.
- SurrealDB stays at v3.3.5.
- Manifold uses these Brain routes: `/health`, `/v1/ingest/document|fact|link`, `/v1/documents/{id}`, `/candidates`, `/retry`, `/v1/entities/{id}`, `/connections`, `/v1/facts/{id}`, `/retract`, `/v1/search`. Their request and response contracts must not change for Manifold.
- `providerRefusal` values stay `billing`, `auth`, `rate_limit`, `outage`.
- Every Manifold document must be extracted. Brain must never keep one raw.
- Local migration ids start at `9001`. Each local migration is DDL only, idempotent, and listed in `test/golden/migration-checksums.json`.
- `DATA_VERSION` becomes `5`. Upstream migrations 0168–0199 are one-way. Rollback means: restore the cold backup and start the old images.
- The repository is public. Do not commit secrets, host names that are not already published, account details, or production content.
- Do not push, merge, change production, or call a paid provider without explicit authorization.
- Before each commit, run the checks in [`AGENTS.md`](../AGENTS.md) that apply to the changed files.

## Review Focus

1. A database upgraded from the current build holds ledger ids 0168–0170 for local files. Upstream's files with the same ids must still apply. (Task 4)
2. 150 flag defaults changed upstream, most from off to on. The upgrade must not start new background model work. (Task 5)
3. Upstream can keep a document raw and close its run as `skipped`. Manifold counts `skipped` as done. A document must never become `ready` without extraction. (Task 6)
4. Upstream no longer queues work when a committed revision is submitted again with `mode: async`. A Manifold retry or restart must still reach `ready`. (Task 6)
5. Brain's new nightly retry pass and Manifold's retry can pick the same failed run. The document must be extracted once. (Task 2)

## Findings that shape the plan

| Current patch | Upstream v2.11.0 | Action |
| --- | --- | --- |
| `hnsw-provision-early-tenants` | `onTenantSchemaReady` replays known databases | Drop |
| `knn-empty-result-fallback` | `knnIndexUsable` reads the catalogue and uses the exact scan | Drop; keep the e2e check |
| `resolve-fact-entity-index` | Ingest now calls `fn::settle_fact` (0177), without an index hint | Replace with migration 9001 |
| `selective-entity-fact-indexes`, `derived-stale-cascade` | Not covered; migration ids collide | Renumber to 9002 and 9003 |
| `provider-refusal` | Classifies `402` for search and synthesize only, not for ingest | Rebase on `classifyProviderFailure` |
| `failed-run-retry` | No per-document retry route; a nightly retry pass was added | Rebase |
| `openai-api-key-format`, `document-origin-identity`, `worker-startup-order`, `corroborated-commit-ref`, `extraction-completion`, `fatal-signal-default`, `openai-chat-extra-body`, `entity-fact-read-index-hints`, `edge-retraction`, `revive-superseded-index`, `search-throttle-limit`, `pool-stall-liveness`, `predicate-vocabulary-refresh` | Not covered | Rebase |

Also new upstream: migrations 0168–0199, hourly data migrations in `src/db/data-migrations/`, conflicts judged by a model instead of weights, and `MODEL_PLATFORM_GATE` (default `observe`, which keeps server models available).

---

### Task 1: One patch list for the build, CI, and docs

Today the Dockerfile, CI, and `AGENTS.md` each repeat the patch list. Make one source before the chain changes.

**Files:**
- Create: `deploy/brain/series`, `scripts/brain-apply-patches.sh`
- Modify: `deploy/brain/Dockerfile`, `.github/workflows/ci.yml`, `AGENTS.md`, `docs/brain-patch.md`

**Interfaces:**
- Produces: `deploy/brain/series`, one patch name per line without path or `.patch`, in apply order.
- Produces: `scripts/brain-apply-patches.sh <brain-checkout>`: applies the series with `git apply --check` then `git apply`; exits non-zero and prints the failing patch name on the first error.

- [ ] **Step 1: Write the series** with the current 20 names in the current Dockerfile order.
- [ ] **Step 2: Write the script**, and use it in the CI patch job and the `AGENTS.md` and `docs/brain-patch.md` command blocks. In the Dockerfile, copy `series` and `*.patch` to `/tmp/brain/` and loop over `series`.
- [ ] **Step 3: Verify on the current pin.**

Run: `git init` a checkout of `a567f6960c031b916549699fec7b89423c650d1e`, then `scripts/brain-apply-patches.sh <dir>`, then `git -C <dir> diff | sha256sum` before and after this task.
Expected: exit 0 and identical hashes.

Run: `docker build -f deploy/brain/Dockerfile .`
Expected: success.

- [ ] **Step 4: Commit** `build(brain): keep the patch order in one series file`.

### Task 2: Pin v2.11.0 and rebase the runtime patches

**Files:**
- Modify: `deploy/brain/Dockerfile`, `deploy/brain/series`, `docker-compose.yml` (`BRAIN_REF`), `.github/workflows/ci.yml`, `AGENTS.md`
- Modify: the 14 patches listed in Step 2
- Delete: `hnsw-provision-early-tenants.patch`, `knn-empty-result-fallback.patch`, `resolve-fact-entity-index.patch`
- Move out of the series until Task 4: `selective-entity-fact-indexes`, `derived-stale-cascade`

**Interfaces:**
- Consumes: Task 1's series and script.
- Produces: a series that applies and builds on v2.11.0, without `provider-refusal` and without local migrations.

Rebase procedure for each patch, in series order: apply the earlier patches, apply this one with `git apply --3way`, resolve, run the patch's own specs, and write the new patch from `git diff` against the state before it.

- [ ] **Step 1: Change the pin and the base image** (values from Global Constraints) and add `COPY --from=source /source/packs ./packs` to the runtime stage.
- [ ] **Step 2: Rebase** in this order: `openai-api-key-format`, `document-origin-identity`, `worker-startup-order`, `corroborated-commit-ref`, `failed-run-retry`, `extraction-completion`, `fatal-signal-default`, `openai-chat-extra-body`, `entity-fact-read-index-hints`, `edge-retraction`, `revive-superseded-index`, `search-throttle-limit`, `pool-stall-liveness`, `predicate-vocabulary-refresh`.
  - `document-origin-identity`: salt upstream's new content hash, which now has a circle variant and a user variant.
  - `corroborated-commit-ref`: `CORROBORATED` still returns `corroboratedFactId`; keep the `vertical === 'manifold'` condition.
  - `failed-run-retry`: regenerate the OpenAPI files with `scripts/build-openapi.ts`.
- [ ] **Step 3: Add the double-retry test** to `test/document-async-retry.unit-spec.ts`: `does not queue a run that the nightly retry pass already reopened`. Arrange one `_general` run in status `running`. Assert that the retry call queues no job and does not change the run.
- [ ] **Step 4: Run Brain's checks.**

Run: `pnpm install --frozen-lockfile && pnpm build && pnpm test:unit`
Expected: pass, except failures that also occur on unpatched v2.11.0. Record those by name after a run on the unpatched tag.

Run with a `surrealdb/surrealdb:v3.3.5` server: `pnpm test:e2e` for `document-retry`, `documents-pipeline`, retraction, supersession, and HNSW specs.
Expected: pass. Add a vector-only search on a tenant without the HNSW index to the HNSW e2e spec; it must return rows (this replaces `knn-empty-result-fallback`).

- [ ] **Step 5: Build both platforms.**

Run: `docker buildx build --platform linux/amd64,linux/arm64 -f deploy/brain/Dockerfile .`
Expected: success.

- [ ] **Step 6: Commit** `build(brain): move to v2.11.0 and rebase the runtime patches`.

### Task 3: Provider refusals on the ingest path

**Files:**
- Modify: `deploy/brain/provider-refusal.patch`, `deploy/brain/series`
- Test: `test/provider-refusal.unit-spec.ts` (inside the patch)

**Interfaces:**
- Consumes: upstream `classifyProviderFailure(err): 'billing'|'key'|'quota'|'outage'|'harness'|'other'` from `src/ai/provider-failure.ts`; `halvingCanHelp(err)` from `src/documents/read-failure.ts`.
- Produces: run message `model provider refused the request (provider_refusal=<class>)` and `providerRefusal` on failed runs in `GET /v1/documents/:id`, with the same four values as today.

- [ ] **Step 1: Write the failing tests.**
  - `maps upstream classes to the Manifold values`: status 402 → `billing`; 429 with `insufficient_quota` → `billing`; 401 and 403 → `auth` (upstream `key`); other 429 → `rate_limit` (upstream `quota`); 503 → `outage`; 400 → no refusal.
  - `does not halve a group the provider refused for payment`: `halvingCanHelp({ status: 402 })` is `false`.
  - `stops the retry passes after a billing or auth refusal`.
- [ ] **Step 2: Run them.** Expected: FAIL.
- [ ] **Step 3: Implement.** Use upstream's classifier and remove the local classification table. Keep the wire names. Make `halvingCanHelp` return `false` for `billing`. Carry over the rethrow when every pass fails, the `providerRefusal` field in `listRuns`, and the batch stop.
- [ ] **Step 4: Run** `pnpm test:unit provider-refusal document-async-retry` and the `document-retry` e2e spec. Expected: PASS.
- [ ] **Step 5: Commit** `fix(brain): keep provider refusals on the v2.11.0 ingest path`.

### Task 4: Local migrations 9001–9003 and release of the old ids

The migrator selects files by their 4-digit id. The current build recorded local files as 0168, 0169, and 0170, without checksums. Without this task, v2.11.0 treats upstream's three files as applied and never runs them.

**Files:**
- Create: `deploy/brain/ledger-release.patch`, `deploy/brain/settle-fact-entity-index.patch`
- Modify: `deploy/brain/selective-entity-fact-indexes.patch`, `deploy/brain/derived-stale-cascade.patch`, `deploy/brain/series`
- Test: `test/ledger-release.e2e-spec.ts`, `test/settle-fact-plan.unit-spec.ts` (inside the patches)

**Interfaces:**
- Produces: `9001_settle_fact_entity_index.surql`, `9002_selective_entity_fact_indexes.surql`, `9003_derived_from_element_index.surql`.
- Produces: in `src/db/migrator.service.ts`, before the applied set is read, a delete of ledger rows whose `name` is one of `0168_resolve_fact_entity_index.surql`, `0169_selective_entity_fact_indexes.surql`, `0170_derived_from_element_index.surql`.

- [ ] **Step 1: Write the failing e2e test** `applies upstream 0168-0170 over a ledger with the old local ids`. Insert the three old rows into a new tenant database, then run the migrator. Assert: no row has an old name; the ledger names for ids 0168, 0169, 0170 are `0168_indexer_run_priority.surql`, `0169_membership_plane.surql`, `0170_progressive_catalogue.surql`; `INFO FOR TABLE external_identity` succeeds; ids 9001–9003 are recorded. Second case: a second migrator run applies nothing.
- [ ] **Step 2: Write the failing unit test** for the plan guard. Assert: 9001 contains `FROM knowledge_fact WITH INDEX fact_entity_idx`; it contains no `WHERE id IN $day_peers.id`; the newest upstream file that defines `fn::settle_fact` is `0177_resolve_fact_without_weights.surql`.
- [ ] **Step 3: Run both.** Expected: FAIL.
- [ ] **Step 4: Implement.**
  - 9001: copy the `DEFINE FUNCTION OVERWRITE fn::settle_fact` block from 0177 unchanged except: add the index hint to the `$candidates` query; replace `UPDATE knowledge_fact SET status = 'competing' WHERE id IN $day_peers.id` with `UPDATE $day_peers.id SET status = 'competing'`; replace the form with `OR id = $new.id` with `UPDATE array::append($day_peers.id, $new.id) SET status = 'competing'`.
  - 9002 and 9003: the SQL of the current 0169 and 0170, renamed. Keep their guard specs.
  - Add the three checksums to `test/golden/migration-checksums.json`.
- [ ] **Step 5: Run** the two new specs, `migration-*` unit specs, and the resolver, supersession, and retraction e2e specs on SurrealDB 3.3.5. Expected: PASS.
- [ ] **Step 6: Commit** `fix(brain): move local migrations to 9001-9003 and release the old ids`.

### Task 5: Freeze upstream's changed defaults

Defaults live in code. An explicit environment value restores the old behavior. Bake the values into the image so that deployment profiles do not carry 150 variables. Compose and `.env` values still win, because `node --env-file` does not replace an existing variable.

**Files:**
- Create: `deploy/brain/flags/baseline.json`, `deploy/brain/flags/adopt.txt`, `deploy/brain/flags/defaults.env`, `scripts/brain-flags.py`, `scripts/test-brain-flags.py`
- Modify: `deploy/brain/Dockerfile`, `.github/workflows/ci.yml`

**Interfaces:**
- Produces: `scripts/brain-flags.py --baseline <json> --current <json> --adopt <txt> --out <env> [--check]`. A catalog JSON is `{"KEY": {"default": "0", "boolean": true}}`.
- Source of a catalog JSON: `CONFIG_CATALOG` (`key`, `defaultValue`, `isBooleanFlag`) from `dist/admin/config-catalog.data.js`, read with `node -e` in the Docker `builder` stage. Upstream's `config-catalog-truth` spec checks the catalog against the code.
- Rules: for a key in both catalogs whose default differs, write the baseline value; for a new boolean key whose default is on, write `0`; skip keys in `adopt.txt` and removed keys; fail on a changed key that has no baseline default.

- [ ] **Step 1: Write failing tests** in `scripts/test-brain-flags.py`: a default `0`→`1` writes `KEY=0`; a number `60000`→`20000` writes `KEY=60000`; a new on flag writes `KEY=0`; an adopted key writes nothing; `--check` exits 1 when the file differs.
- [ ] **Step 2: Run** `python3 scripts/test-brain-flags.py`. Expected: FAIL.
- [ ] **Step 3: Implement the script.** Generate `baseline.json` from the `a567f696` builder stage and `defaults.env` from v2.11.0. Start with an empty `adopt.txt`.
- [ ] **Step 4: Run the tests.** Expected: PASS, and `defaults.env` holds the changed keys and `RETRIEVAL_CLAIM_CHECK=0`, `RETRIEVAL_COMMUNITY_EVIDENCE=0`.
- [ ] **Step 5: Use the file.** Copy it to `/app/defaults.env` and set `CMD ["node", "--env-file=/app/defaults.env", "dist/main.js"]`. Add a CI step that dumps the catalog and runs `--check`.
- [ ] **Step 6: Boot the image** with the integration environment. Expected: environment validation passes and `/health` returns 200. If validation rejects a pinned value, add the key to `adopt.txt` with a comment that names the reason.
- [ ] **Step 7: Commit** `build(brain): freeze the flag defaults that v2.11.0 changed`.

### Task 6: Manifold against the new Brain

**Files:**
- Modify: `docker-compose.yml`, `test/integration/provider/main.go`, `test/integration/integration_test.go`, `scripts/integration.sh`
- Modify only if a test requires it: `internal/upstream/brain.go`, `internal/upstream/brain_test.go`

**Interfaces:**
- Consumes: the image from Tasks 2–5.
- Produces: `DECISIONS_LANES: ""` fixed in the Compose Brain service (not read from `.env`), so that triage cannot keep a document raw.

- [ ] **Step 1: Add the failing assertions** to the integration suite:
  - `every ready document has extracted facts`: after a write reaches `ready`, `GET /api/v1/status` shows more entities and facts, and the Brain run for `_general` is `succeeded`, not `skipped`.
  - `a resubmitted revision converges`: restart Manifold while the job waits for Brain, and retry a finished revision. Both reach `ready` and the fact count does not change.
- [ ] **Step 2: Run** `make integration`. Expected: failures for the new assertions or for model calls that the test provider does not answer yet.
- [ ] **Step 3: Implement.** Set `DECISIONS_LANES`. Teach the test provider the new structured calls that appear in its log (fact and conflict judges). Change the adapter only for a contract difference that a test shows.
- [ ] **Step 4: Run** `make verify`, `make integration`, and `docker compose --env-file .env.example config --quiet`. Expected: all pass. The existing refusal, recovery, restart, and SurrealDB-restart phases must pass unchanged.
- [ ] **Step 5: Test the in-flight query case.** Restart SurrealDB while Brain has queries in flight. Record whether surrealdb-js 2.1.1 rejects them. Keep `pool-stall-liveness` in both cases; state the result in `docs/brain-patch.md`.
- [ ] **Step 6: Commit** `test(brain): hold the Manifold contract on v2.11.0`.

### Task 7: Documentation and the data version boundary

**Files:**
- Modify: `DATA_VERSION` (`5`), `docs/operations.md`, `docs/brain-patch.md`, `docs/dependencies.md`, `README.md`, `THIRD_PARTY_NOTICES.md`

**Interfaces:**
- Consumes: the final series and the results of Tasks 2–6.

- [ ] **Step 1: Write "Data version 5 migration"** in `docs/operations.md`: cold four-volume backup with verified hashes; the ledger release; upstream migrations 0168–0199 and local 9001–9003; hourly data migrations; frozen defaults; the deployment profile digest changes with `docker-compose.yml`; acceptance (write, `ready`, graph counts grow, `INFO FOR TABLE knowledge_fact` has `fact_derived_from_el_idx` and no `fact_user_idx`, update, delete); rollback by restore only.
- [ ] **Step 2: Update the patch notes** to the new series: remove the dropped patches, add `ledger-release` and `settle-fact-entity-index`, and update the pin in all listed files.
- [ ] **Step 3: Verify** that no old pin remains: `git grep -n a567f696 -- . ':!plans' ':!deploy/brain/flags'` prints only historical text in `docs/operations.md`.
- [ ] **Step 4: Commit** `docs: describe the data version 5 upgrade`.

### Task 8: Rehearsal on a copy of production data

Needs explicit authorization: it copies production data into an isolated project on the host. Part B also needs an approved provider budget.

- [ ] **Step 1: Restore** the latest cold backup into a separate Compose project with no published ports.
- [ ] **Step 2: Part A, no paid calls** (test provider, no provider network). Record the old counts of facts, entities, and edges, then start the new images. Pass when:
  - every tenant database applies all migrations without error, and a second start applies none;
  - counts of facts, entities, and edges do not decrease;
  - hourly data migrations report done;
  - `EXPLAIN` of the `settle_fact` candidate query uses `fact_entity_idx`;
  - one resolve takes under 100 ms and one entity profile under 50 ms;
  - the p95 of 20 fixed searches is at most 1.2 times the current build's on the same copy.
- [ ] **Step 3: Part B, real provider.** Extract the same 20 documents with the current and the new build. Pass when tokens per document are at most 1.5 times the current build, facts per document are at least 0.8 times, and no run fails. Report the cost.
- [ ] **Step 4: Remove** the rehearsal project and its volumes, and record the numbers in this plan.

If a gate fails, stop and report. Do not adjust a gate without the operator's decision.

### Task 9: Production activation

Needs explicit authorization for the merge and the production change.

- [ ] **Step 1: Confirm** that no indexing job is active and that the provider account has credit.
- [ ] **Step 2: Follow** the "Data version 5 migration" section: stop writers, back up the four volumes, verify hashes, update the profile digest, merge, and activate by hand (automatic deployment refuses the data version change).
- [ ] **Step 3: Verify** the running commit, component readiness, search and context without `degraded_dependencies`, Brain logs without migration or validation errors, and one real extraction with update and delete.
- [ ] **Step 4: Watch** the 03:45 UTC sweep and one day of token spend. Compare with the previous week.
- [ ] **Step 5: Roll back** on a failed check: stop the stack, restore the four volumes, and start the previous images.
- [ ] **Step 6: Record** the deployed revision and the results in Manifold memory.
