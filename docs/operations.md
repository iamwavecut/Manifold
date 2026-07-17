# Operations

## Data ownership

Persist all four named volumes:

- `manifold-data`: SQLite public identity and workflow state.
- `openviking-data`: canonical content, indexes, and snapshots.
- `brain-cache`: Brain model/cache and baseline state.
- `surrealdb-data`: graph data.

The volumes form one logical backup set. Stop writers before a cold backup:

```bash
docker compose stop manifold brain openviking
docker run --rm -v manifold_manifold-data:/source:ro -v "$PWD/backups":/backup alpine \
  tar -C /source -czf /backup/manifold-data.tgz .
docker run --rm -v manifold_openviking-data:/source:ro -v "$PWD/backups":/backup alpine \
  tar -C /source -czf /backup/openviking-data.tgz .
docker run --rm -v manifold_surrealdb-data:/source:ro -v "$PWD/backups":/backup alpine \
  tar -C /source -czf /backup/surrealdb-data.tgz .
docker compose start surrealdb openviking brain manifold
```

Back up `brain-cache` when local downloaded models or baselines must be retained. Provider credentials and `.env` belong in a separate secret backup, never in the archives.

## Restore

1. Stop the stack.
2. Restore each archive into an empty volume with the same logical role.
3. Restore the matching `.env`.
4. Start `surrealdb`, then `openviking` and `brain`, then `manifold`.
5. Verify `/ready`, `/api/v1/status`, representative documents/revisions, graph queries, and one new document job.

Do not restore only SQLite over newer OpenViking or Brain state. Public mappings and derived state can diverge.

## Durable jobs

Jobs in `indexing` or `extracting` are returned to `accepted` when Manifold restarts. Completed semantic errors persist in SQLite. After a dependency recovers:

```bash
curl -X POST "$MANIFOLD_URL/api/v1/jobs/$JOB_ID/retry" \
  -H "Authorization: Bearer $MANIFOLD_API_KEY" \
  -H "Idempotency-Key: retry-$JOB_ID-1"
```

Retry only `failed` or `partially_ready` jobs. Other states return `invalid_state_transition`.

## Key rotation

Create a new admin key, verify it, switch clients, then revoke the old key from the new credential. Manifold refuses self-revocation to prevent accidental lockout.

`OPENVIKING_API_KEY` is the root key for the internal trusted deployment and must match in both the Manifold and OpenViking containers. Rotate it during a controlled stack restart.

Brain uses a SHA-256 registry because it is an internal pinned dependency. Regenerate `BRAIN_API_KEYS_JSON` with `scripts/brain-key-hash.sh` whenever `BRAIN_API_KEY` rotates.

## Scale boundary

Version 1 is one tenant and one Manifold replica. SQLite worker leasing protects restart recovery but is not a multi-replica coordination contract. Do not place two Manifold replicas over the same database.
