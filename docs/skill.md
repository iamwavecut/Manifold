# Agent skill

Manifold publishes one external skill from `skills/manifold`. The skill is a
REST client for any network-reachable Manifold instance. It does not install a
server and does not use MCP.

## Install

Use the standard [`skills`](https://github.com/vercel-labs/skills) CLI. It can
discover `skills/manifold` directly from the GitHub repository.

Install Manifold globally for every supported agent without confirmation
prompts:

```bash
npx skills add iamwavecut/Manifold \
  --skill manifold \
  --agent '*' \
  --global \
  --yes
```

The quoted `'*'` prevents shell expansion and tells the CLI to install for all
agents it supports. `--global` selects user scope, and `--yes` skips every
confirmation prompt. Inspect discovery without installing:

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

## Verify

Start a new agent session after installation, then ask it to use `$manifold`
for a status check. From a shell, the bundled client can be verified without
another runtime:

```bash
sh ~/.agents/skills/manifold/scripts/manifold status
```

The exact canonical directory is managed by the `skills` CLI and can vary by
scope and agent. Discover installed paths instead of hard-coding them:

```bash
npx skills list --global --agent '*'
```

## Update or remove

Update the global skill from its recorded source:

```bash
npx skills update manifold --global
```

Remove it globally from every supported agent:

```bash
npx skills remove manifold --agent '*' --global --yes
```
