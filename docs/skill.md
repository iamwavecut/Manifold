# Agent skill

Manifold publishes one external skill from `skills/manifold`. The skill is a
REST client for any network-reachable Manifold instance. It does not install a
server and does not use MCP.

## Install

Use the standard [`skills`](https://github.com/vercel-labs/skills) CLI. It can
discover `skills/manifold` directly from the GitHub repository.

Install Manifold globally and let the CLI select the target agents:

```bash
npx skills add iamwavecut/Manifold \
  --skill manifold \
  --global
```

`--global` selects user scope. The CLI handles agent selection and confirmation
according to the current environment. Inspect discovery without installing:

```bash
npx skills add iamwavecut/Manifold --list
```

The installer requires Node.js/npm because it runs through `npx`. The installed
Manifold skill does not: it contains native clients for macOS and Linux on
`amd64` and `arm64`.

## Select an instance

The agent process must receive:

```bash
export MANIFOLD_URL=https://memory.example.net
export MANIFOLD_API_KEY='a-key-with-the-required-capabilities'
```

`MANIFOLD_URL` is the public base URL of one remote deployment.
`MANIFOLD_API_KEY` is a bearer credential created by that deployment. The skill
does not have a localhost default, discover servers, or store credentials.

Environment variables are inherited when an agent starts. Changing variables
in another terminal does not retarget an already-running agent process. To use
another instance, load that instance's two values and start a new agent
process.

Keep profiles in an operating-system keychain or secret manager. Inject the
selected profile when launching the agent; do not put API keys in shell
history, the repository, `SKILL.md`, agent prompts, or notes. A typical
secret-manager wrapper sets the variables only for the child process:

```bash
MANIFOLD_URL=https://manifold.example.net \
MANIFOLD_API_KEY="$(your-secret-manager read manifold-production-api-key)" \
codex
```

The key needs only the capabilities required by the workflow. Prefer a
read-only key for search and context workflows; use an admin key only for key
administration.

## Use Manifold as memory

Start non-trivial work with retrieval, then read relevant canonical documents:

```bash
sh scripts/manifold context --token-budget 2500 "task, prior decisions, constraints, and runbooks"
sh scripts/manifold search --type document "specific decision or incident"
sh scripts/manifold read candidate-slug
```

Before preserving new knowledge, inspect the existing hierarchy with
`tree --glob '**'`. Keep durable cross-agent knowledge under `shared/<domain>`,
durable project knowledge under
`projects/<project>/<project-qualified-topic>`, and isolated work under a
distinct `tasks/<task-slug>`. Reuse a matching canonical folder instead of
creating a synonym or catch-all branch. Folder slugs are globally unique in
v1, so qualify ambiguous child slugs.

Use `remember` for mutation:

The selected key needs `search`, `read_documents`, and `write_documents` for
this guarded workflow.

```bash
sh scripts/manifold remember \
  --id agent-memory-practice \
  --title "Agent memory practice" \
  --folder-path shared/agent-practice \
  --file memory-practice.md
```

`remember` performs semantic discovery again. If it exits with
`memory_candidates_found`, read the listed candidates and then either update a
canonical document or justify a distinct lifecycle:

```bash
sh scripts/manifold remember --update existing-slug --file reconciled.md

sh scripts/manifold remember \
  --new \
  --reason "Incident-specific evidence with a short operational lifetime" \
  --id incident-42-findings \
  --title "Incident 42 findings" \
  --folder-path tasks/incident-42 \
  --file findings.md
```

The command creates missing nested folders atomically and succeeds only after
the durable job reaches `ready`. It returns nonzero for `partially_ready`,
`failed`, or a wait timeout. It never blind-retries `etag_mismatch`; reread,
merge, and issue a new update instead. It also refuses every mutation with
`discovery_degraded` when OpenViking or Brain search is incomplete.

## Verify

Start a new agent session after installation, then ask it to use `$manifold`
for a status check and a scoped memory search. From a shell, the bundled client can be verified without
another runtime:

```bash
sh ~/.agents/skills/manifold/scripts/manifold status
```

The exact canonical directory is managed by the `skills` CLI and can vary by
scope and agent. Discover installed paths instead of hard-coding them:

```bash
npx skills list --global
```

## Update or remove

Update the global skill from its recorded source:

```bash
npx skills update manifold --global
```

Remove it globally and let the CLI select the affected agents:

```bash
npx skills remove manifold --global
```
