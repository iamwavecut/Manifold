# Plan 003: Release v0.2.0 and deploy every green main SHA

> **Executor instructions**: This plan changes GitHub and production state.
> Complete Plans 001 and 002 first. Deploy only an exact SHA that passed every
> CI job, preserve volumes and `.env`, and roll back automatically on failed
> deep smoke.
>
> **Drift check**: `git diff --stat e03651e..HEAD -- .github/workflows scripts/deploy-production.sh Dockerfile docker-compose.yml README.md docs package.json`

## Status

- **Priority**: P0
- **Effort**: M
- **Risk**: HIGH; this creates production automation and performs a public
  release.
- **Depends on**: Plans 001 and 002
- **Category**: release, deployment, operations
- **Planned at**: commit `e03651e`, 2026-07-22

## Why this matters

The previous work was pushed and CI was green, but no workflow could deploy it.
The server checkout remained at `a21bd0`, the container's build label remained
`3c15ac2`, and clients immediately moved to `e03651e`. Production delivery must
be an explicit exact-SHA state machine with health, protocol, and real retrieval
proof—not an assumption attached to a green multiarch build.

`v0.2.0` is the recommended release because it distinguishes the new
discovery-first/nested-folder protocol and compatibility metadata from every
existing binary that currently claims `0.1.0`.

## Current state

- `.github/workflows/ci.yml:75-85` builds multiarch with `push: false`.
- `.github/workflows/container.yml:3-6` runs only on manual dispatch or `v*`
  tags and only publishes GHCR.
- There is no deploy workflow in repository history.
- There are no Git tags, GitHub releases, production environment, or repository
  deployment secrets.
- `/home/wavecut/Manifold` on `geta.moe` is clean but has a stale, unfetched
  `origin/main` reference. Its `.env` must be preserved and never printed.
- Only Manifold is publicly exposed; deployment must retain the existing
  private/provider network boundaries and named volumes.

## Required GitHub configuration

Create a `production` environment and configure these names without ever
committing or printing values:

- `MANIFOLD_DEPLOY_HOST` — production SSH host;
- `MANIFOLD_DEPLOY_USER` — restricted deployment user;
- `MANIFOLD_DEPLOY_SSH_KEY` — dedicated private key;
- `MANIFOLD_DEPLOY_KNOWN_HOSTS` — pinned host-key line, never dynamic
  `ssh-keyscan` trust;
- environment variable `MANIFOLD_PUBLIC_URL` — public HTTPS base URL.

The deployment key should be limited to this host/account. Do not place the
Manifold API key in GitHub: authenticated smoke should execute on the host and
read the existing protected `.env` without echoing it.

## Commands

| Purpose | Command | Expected result |
| --- | --- | --- |
| Local full gate | `make verify` | exit 0 |
| Local integration | `MANIFOLD_PORT=28080 make integration` | exit 0 and cleanup |
| Compose contract | `docker compose --env-file .env.example config --quiet` | exit 0 |
| CI | `gh run watch <run-id> --exit-status` | all jobs success |
| Release publication | `gh release view v0.2.0` | published release at exact tag SHA |
| Production parity | authenticated `/api/v1/meta` | commit equals release SHA |

## Scope

**In scope**:

- new `.github/workflows/deploy-production.yml`;
- `.github/workflows/container.yml` and CI only where release dependencies need
  wiring;
- new `scripts/deploy-production.sh` and a deep smoke helper;
- `Dockerfile`, `docker-compose.yml`, `.env.example` build identity handling;
- `README.md`, `docs/operations.md`, `CONTRIBUTING.md`, `AGENTS.md`;
- version source/package metadata and release notes;
- GitHub production environment, secrets, tag, release, GHCR publication;
- exact-SHA deployment to `/home/wavecut/Manifold` on `geta.moe`.

**Out of scope**:

- changing production provider/model credentials;
- deleting or recreating volumes;
- exposing dependency ports;
- storing API keys in Actions;
- deploying a dirty remote worktree;
- using `latest` as proof of what is running;
- publishing before Plans 001 and 002 are green.

## Steps

### Step 1: Add an exact-SHA deployment script with rollback

Create `scripts/deploy-production.sh <sha>` for the automation-owned clean
checkout. It must:

1. acquire a host lock so two deploys cannot overlap;
2. verify the canonical path, current branch, clean worktree, `.env` presence
   and mode, Docker/Compose availability, and expected project name;
3. fetch the exact SHA and verify it belongs to `origin/main`;
4. record the previous source SHA and running image IDs;
5. validate Compose and build from the exact fetched source with build version
   `0.2.0` (or the release value) and commit equal to the SHA;
6. fast-forward the canonical checkout exactly to that SHA;
7. run `docker compose up -d --build --wait --wait-timeout 300` without `down`,
   `-v`, or changing `.env`;
8. verify health, readiness, public metadata commit/features, authenticated
   status, unscoped and scoped retrieval, and no degraded dependencies;
9. on failure, restore the previous clean SHA, rebuild/restart the previous
   stack, verify its health, and still return failure to GitHub;
10. print only SHAs, service states, and request IDs—never environment values.

`git reset --hard` is permitted inside rollback only after the script has
proved the worktree was clean and recorded the exact prior SHA. Otherwise stop.

**Verify**: exercise success and forced-smoke-failure rollback against a
disposable local/remote test deployment before production.

### Step 2: Add deployment after successful CI on main

Create `deploy-production.yml` triggered by `workflow_run` for completed `CI`
runs plus manual dispatch. Guard automatic deploy with all of:

- conclusion is `success`;
- source event is `push`;
- head branch is `main`;
- head repository is `iamwavecut/Manifold`;
- environment is `production`;
- concurrency group serializes deploys without racing newer SHAs.

Use `github.event.workflow_run.head_sha` as the only automatic deploy target.
Set up OpenSSH from the pinned known-hosts secret, copy or invoke the checked-in
deploy script, and never re-resolve `main` during the job.

**Verify**: a documentation-only test commit in a disposable branch must not
deploy; a green main push deploys its exact SHA; a failed CI run does not start
the deploy job.

### Step 3: Fix build provenance

Pass version and full commit separately into Docker. Remove the production
dependency on a stale `MANIFOLD_VERSION` line in `.env`; the deploy workflow
owns build identity. Confirm `/api/v1/meta`, container `manifold version`, Git
checkout, workflow SHA, and release tag all agree.

**Verify**: the deployment script fails if any one of those identities differs.

### Step 4: Prepare and publish v0.2.0

After Plans 001 and 002 are merged and CI is green:

1. ensure the working tree is clean and `HEAD == origin/main`;
2. update all version surfaces to `0.2.0` and regenerate skill binaries/OpenAPI;
3. run full verification and integration again;
4. create annotated tag `v0.2.0` on that exact SHA and push the tag;
5. wait for `Publish container` and confirm amd64/arm64 GHCR manifests;
6. create a public GitHub Release titled `Manifold v0.2.0` with generated notes
   plus explicit sections for protocol negotiation, discovery-first memory,
   nested folders/glob tree, Brain auth recovery, deployment behavior, upgrade
   order, and the `npx skills` reinstall command;
7. do not mark the release successful until production parity passes.

No previous tags exist, so release notes should identify this as the first
versioned operational release while still explaining the `0.1.0`/`dev` builds
that preceded it.

### Step 5: Verify the live instance and agent path

After automatic deployment, prove through the public URL:

- `/health`, `/ready`, `/openapi.yaml` return success;
- metadata version is `0.2.0`, commit equals tag SHA, protocol revision is 2,
  and required features are present;
- authenticated status is ready;
- unscoped context/search work;
- scoped search works and returns no degraded dependencies;
- tree glob works;
- one uniquely named test document created with `remember` reaches `ready`, is
  readable by canonical reference, and is deleted afterward;
- OpenViking, Brain, and SurrealDB ports remain unpublished;
- a Manifold-only restart preserves status and canonical data.

Finally install/update the released skill in a fresh agent session and repeat
status plus read-only context. Do not store the release smoke artifact as
durable memory.

## Test plan

- Workflow trigger/condition tests or static assertions.
- Shell syntax and secret-redaction tests for deploy helpers.
- Disposable rollback drill.
- Exact-SHA mismatch negative test.
- Failed-CI no-deploy observation.
- Multiarch manifest inspection.
- Production deep search and `remember` lifecycle smoke with cleanup.
- Post-restart persistence and network exposure checks.

## Done criteria

- [ ] GitHub has a production environment with the required secret names.
- [ ] Every successful main CI run deploys its exact SHA; failed/PR CI does not.
- [ ] Deployment serializes, preserves `.env`/volumes, and has a tested rollback.
- [ ] `v0.2.0` tag, GitHub Release, and amd64/arm64 GHCR image exist at one SHA.
- [ ] Production checkout, container, metadata, workflow, and tag SHA agree.
- [ ] Live context, search, scoped retrieval, tree, and `remember` pass without
      degraded dependencies.
- [ ] Test memory is deleted and no dependency port is exposed.
- [ ] README/operations describe upgrade, rollback, release, and skill update.

## STOP conditions

- Any Plan 001/002 verification is not green.
- The production worktree is dirty or `.env`/volumes cannot be proven intact.
- Deployment secrets or pinned host identity are missing.
- The exact release SHA cannot be reproduced in build metadata.
- Deep retrieval is degraded even though shallow health is green.
- Rollback has not been exercised safely outside production.
- Publishing would require exposing a credential or provider payload.

## Maintenance notes

Treat a green CI run, a published container, a GitHub Release, and a production
deployment as four distinct states. Future completion reports must name each
state and its SHA. Never use multiarch `push: false`, a stale remote
`origin/main`, or shallow `/health` as evidence that production is current.

