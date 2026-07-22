---
name: manifold
description: Work with any network-reachable Manifold knowledge and memory service through its REST API. Use proactively before non-trivial work to retrieve prior decisions, procedures, incidents, and evidence; use after verified material work to preserve durable cross-agent knowledge, project knowledge, or task-scoped notes without duplicating canonical memory. Supports semantic discovery, context, canonical documents and history, nested folder browsing, durable writes, jobs, graph data, conflicts, and cascading renames. Does not use MCP.
---

# Manifold

Run the bundled client as `sh scripts/manifold`. It selects a self-contained native binary; using the installed skill requires no Python, Go, Node.js, or other runtime.

Read `MANIFOLD_URL` and `MANIFOLD_API_KEY` from the agent process environment. Treat the URL as operator-provided and remote: never assume localhost. Never print, log, pass as an argument, or store the API key.

## Retrieve memory first

Before non-trivial work:

1. Run `context` for a task briefing or `search` for targeted discovery.
2. Read every relevant canonical candidate with `read`; use `history` when the decision changed over time.
3. Reconcile memory with current repository or runtime evidence. Current user instructions and live sources override stale memory.
4. Cite consequential evidence as document slug plus revision, preferably the returned `canonical_ref`.

```bash
sh scripts/manifold context --token-budget 2500 "authentication incident history and current runbook"
sh scripts/manifold search --type document "retention decision"
sh scripts/manifold read retention-policy
sh scripts/manifold history retention-policy
```

Use `--scope-glob 'projects/ngbot/**'` only when the task is intentionally limited to that subtree. Global discovery is safer when checking for duplicates.

## Place knowledge deliberately

Inspect the existing hierarchy before choosing a destination:

```bash
sh scripts/manifold tree --glob '**' --types folder
sh scripts/manifold tree --glob 'shared/**'
sh scripts/manifold tree --glob 'tasks/**' --types document
```

Choose the narrowest truthful lifecycle:

- `shared/<domain>`: durable, project-independent knowledge useful to many agents, such as agent practice, security principles, or operational wisdom.
- `projects/<project-slug>/<project-qualified-topic>`: durable knowledge specific to one project, product, or service.
- `tasks/<task-slug>`: isolated investigations, one-off implementation notes, and evidence that can lose relevance with the task. Give unrelated problems different task folders.

Do not create a catch-all dumping folder. Reuse an existing folder when its meaning and lifecycle match. Do not reproduce an equivalent hierarchy under a new name. A new folder is justified when the problem, ownership, or retention lifecycle is genuinely separate.

Folder slugs are globally unique in Manifold v1, even when nested. Use one shared `tasks`, `projects`, and `shared` root; qualify reusable-looking child slugs with the project or task when necessary. A `folder_path_conflict` identifies the existing canonical path so the agent can reuse it or choose a genuinely distinct segment.

## Remember verified knowledge

Use `remember`, not raw REST mutations. The command performs another hybrid discovery pass as a safety gate, creates missing nested folders atomically, and waits until the indexing job reaches `ready`.

For a new durable document:

```bash
sh scripts/manifold remember \
  --id agent-memory-practice \
  --title "Agent memory practice" \
  --folder-path shared/agent-practice \
  --file memory-practice.md
```

If related documents exist, `remember` exits with `memory_candidates_found` and prints candidates. Read them before choosing:

```bash
sh scripts/manifold read existing-policy

# Merge the complete new canonical content into the existing document.
sh scripts/manifold remember \
  --update existing-policy \
  --file reconciled-policy.md

# Create separately only when it has a different meaning or lifecycle.
sh scripts/manifold remember \
  --new \
  --reason "Incident-specific evidence with a short operational lifetime" \
  --id incident-42-findings \
  --title "Incident 42 findings" \
  --folder-path tasks/incident-42 \
  --file findings.md
```

An update replaces the full active content. Always read and merge the current document first. On `etag_mismatch`, read it again, reconcile the concurrent change, and issue a new command; the client never retries a stale mutation blindly.

Persist only verified durable outcomes: decisions with rationale, stable constraints, incident causes and fixes, reusable procedures, deployed state, and provenance. Do not store chain-of-thought, guesses, transient progress, raw chat logs, credentials, or sensitive content without explicit authorization.

The legacy `write` command is an alias for `remember`; prefer `remember` in new instructions.

## Operate other surfaces

```bash
sh scripts/manifold status
sh scripts/manifold jobs
sh scripts/manifold job d5vqh9idc6b5u5sctq00
sh scripts/manifold graph customer-acme
sh scripts/manifold facts --entity customer-acme
sh scripts/manifold conflicts
sh scripts/manifold rename-preview document:old-slug:new-slug
sh scripts/manifold rename-apply d5vqh9idc6b5u5sctq00
```

Add `--json` before the command for machine-readable output. Use `retry-job` only after following the job's stored remediation. Review every rename preview; never auto-apply a changed or blocked plan.

## Handle semantic errors

Trust `code`, never classify by parsing `detail`. The client prints remediation and `request_id` and maps stable codes to exit statuses.

- `memory_candidates_found`: read candidates, then choose update or explicitly justified new memory.
- `discovery_degraded`: restore every listed search dependency before mutating memory.
- `folder_path_conflict`: inspect the existing path; reuse it unless the concepts differ.
- `etag_mismatch`: reread and manually merge; do not blind-retry.
- `stale_rename_plan`: create and review a new preview.
- `unrewritable_reference`: resolve every blocker manually.
- `dependency_unavailable`: inspect `status` and obey `retry_after`.
- `job_wait_timeout`: inspect the job; do not claim persistence until it is terminal.
- `server_incompatible`: the remote server does not advertise behavior required
  by this command. Ask the operator to deploy a compatible Manifold release.
  For a read-only request only, an unscoped fallback is allowed when removing
  the filter does not change the task's meaning. Never use that fallback for
  `remember` or another mutation.

Read [references/error-codes.md](references/error-codes.md) when handling failures or scripting exit statuses.
