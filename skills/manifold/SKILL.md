---
name: manifold
description: Work with a self-hosted Manifold knowledge and memory service through its REST API. Use when an agent needs to read or write canonical documents, search memory, build evidence context, inspect history or jobs, traverse the knowledge graph, resolve conflicts, or preview and apply cascading semantic-ID renames. Do not use MCP; this skill calls the public OpenAPI-described HTTP API.
---

# Manifold

Use the bundled launcher as `sh scripts/manifold` for deterministic API calls. It selects a self-contained native binary shipped inside this skill and repairs executable permissions lost by ZIP-based installers; the user does not need Python, Go, Node.js, or another language runtime.

This skill is distributed from `iamwavecut/Manifold` with the standard
`npx skills` CLI. Do not create another installer or copy the skill into
agent-specific directories manually.

The CLI connects to any network-reachable Manifold deployment and reads:

- `MANIFOLD_URL`, the operator-provided remote base URL, for example `https://memory.example.net`
- `MANIFOLD_API_KEY`, a bearer key with the capabilities required by the command

Never print the key, put it in command arguments, or copy it into notes. The CLI generates an idempotency key for each mutation and uses `ETag`/`If-Match` when updating a document.

## Choose a workflow

- Find knowledge: run `search`, then use `context` when the result will be passed to another agent.
- Read canonical content: run `read`; use `history` when the current revision is not enough.
- Persist knowledge: run `write`. It creates a document when the slug is absent and updates it against the current ETag otherwise.
- Inspect asynchronous work: run `jobs` or `job`; use `retry-job` only after following the stored remediation.
- Inspect structured memory: run `graph`, `facts`, `relations`, or `conflicts`.
- Rename semantic IDs: run `rename-preview`, review every replacement and blocker, then run `rename-apply` with the returned plan XID.

## Commands

```bash
sh scripts/manifold status
sh scripts/manifold tree
sh scripts/manifold read agent-memory
sh scripts/manifold write --id agent-memory --title "Agent memory" --file memory.md
sh scripts/manifold search "What did we decide about retention?"
sh scripts/manifold context --token-budget 2500 "Retention decision"
sh scripts/manifold history agent-memory
sh scripts/manifold jobs
sh scripts/manifold graph customer-acme
sh scripts/manifold conflicts
sh scripts/manifold rename-preview document:old-slug:new-slug
sh scripts/manifold rename-apply d5vqh9idc6b5u5sctq00
```

Add `--json` before the command when another program needs the unmodified response.

Do not assume the server is local. If `MANIFOLD_URL` is missing, stop and ask the user or deployment operator for the instance URL. Prefer HTTPS for any non-loopback deployment. The bundled binaries support macOS and Linux on `amd64` and `arm64`.

The two environment variables select exactly one instance for the current
agent process. To switch between production, staging, or another remote
installation, load a different credential profile and start a new agent
process. Never store an instance API key in this skill directory.

## Handle semantic errors

Trust `code`, never classify failures by parsing `detail`. The CLI maps stable codes to distinct exit statuses, prints the server's remediation steps, and includes `request_id` for operator lookup.

- `etag_mismatch`: `write` rereads the current document and retries once with the new ETag.
- `slug_taken`: choose `suggested_slug` only if changing the canonical ID is acceptable.
- `stale_rename_plan`: create and review a new preview; never auto-apply a changed preview.
- `unrewritable_reference`: update every listed blocker manually before creating a new preview.
- `dependency_unavailable`: follow `retry_after` and inspect `status` before retrying.

Read [references/error-codes.md](references/error-codes.md) for the exit-code table and safe recovery rules.

## Preserve knowledge invariants

- Treat folders, documents, entities, sources, and API-key IDs as semantic ASCII slugs.
- Treat fact, relation, conflict, job, request, and rename-plan IDs as opaque XIDs.
- A display-name edit does not rename a slug.
- Use a rename plan for every ID change, including batches, swaps, and cycles.
- Review the preview. Historical snapshots stay immutable; active documents may receive a new revision.
- Cite document slug and revision when carrying evidence into another system.
