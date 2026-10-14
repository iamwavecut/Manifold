# Dependency audit

Audited 2026-10-14 against upstream release pages and registries (previous
audit 2026-10-02, whose selections are the baselines below). The matrix
records stable releases available on that date, except for INITE Brain, which
has published no release since v2.3.0 and is pinned to a `main` commit. It is
a dated snapshot.

## Brain and graph storage

| Surface | Baseline | Selected | Compatibility evidence |
| --- | --- | --- | --- |
| INITE Brain | v2.3.0, source `0f3d16db0d175794b79b73ef186351aa9f2da3c0` | `main` at `a567f6960c031b916549699fec7b89423c650d1e` (v2.3.0 plus 39 commits, the last on 2026-09-26) | [Commit log](https://github.com/inite-ai/inite-brain-service/compare/v2.3.0...a567f6960c031b916549699fec7b89423c650d1e); no later release exists. [Adapter and patch contracts](brain-patch.md). Adapter routes are unchanged. Migrations 0161-0167 come from upstream and 0168-0169 from Manifold's performance patches; all are one-way. Upstream's batched entity judge (#705) shortens document commits. |
| SurrealDB | v3.3.0, image digest `sha256:681c6c22c287421b5c7d99e0fde79b6e0d32c36c1ddeaab2762a1661cb04cd20` | v3.3.0, same digest | [Release notes](https://surrealdb.com/releases/3.3). Still the newest 3.3 release (v3.2.5 patches the older line). Its planner scans whole tables for `userId IS NONE` and for array `CONTAINS` on unindexed elements; see the performance patches in [brain-patch.md](brain-patch.md). |

Brain builds on Node 22 LTS (22.23.3, the same major line as its upstream
Dockerfile) with the upstream frozen pnpm lockfile. Its dependency graph is taken as a tested upstream release; Manifold
does not independently rewrite that graph or substitute Node 26 beneath native
bindings. Production activation crosses `DATA_VERSION` 4 and follows the
[coordinated migration procedure](operations.md#data-version-4-migration).

## Runtime and container images

| Surface | Baseline | Selected | Compatibility note and source |
| --- | --- | --- | --- |
| Go language floor (`go.mod`) | 1.27.0 | 1.27.0 | Go 1.27 is the current stable major. The Go release history lists 1.27.2 as the current patch, with compiler, runtime, cgo, and standard-library fixes. [Go release history](https://go.dev/doc/devel/release) |
| Go build toolchain (CI and Docker) | 1.27.1 | 1.27.2 | CI and both Go build stages are pinned to the latest 1.27 patch. Huma 2.39.1 requires Go 1.25 or newer. [Go releases](https://go.dev/dl/), [Huma v2.39.1](https://pkg.go.dev/github.com/danielgtaylor/huma/v2@v2.39.1) |
| Node.js / bundled npm | Node 26.10.0 | Node 26.11.1 | Latest stable 26 release. [Node releases](https://nodejs.org/en/about/previous-releases) |
| Python used by CI checks | 3.14.7 | 3.14.8 | The Python job only runs repository scripts; it has no third-party Python packages. 3.14.8 is the latest 3.14 maintenance release. [Python 3.14.8](https://www.python.org/downloads/release/python-3148/) |
| Node build image | `node:26.10.0-alpine3.24` | `node:26.11.1-alpine3.24` | Exact official image tag. [Official Node image tags](https://hub.docker.com/_/node/tags?name=26.11.1-alpine3.24) |
| Go build images | `golang:1.27.1-alpine3.24` | `golang:1.27.2-alpine3.24` | Exact official image tag; multi-architecture manifest verified. [Official Go image tags](https://hub.docker.com/_/golang/tags?name=1.27.2-alpine3.24) |
| Runtime images | `alpine:3.24.2` | `alpine:3.24.2` | Exact official Alpine patch tag. [Official Alpine image tags](https://hub.docker.com/_/alpine/tags?name=3.24.2) |

## Application packages

### Go modules

| Module | Baseline | Selected | Source |
| --- | --- | --- | --- |
| `github.com/danielgtaylor/huma/v2` | 2.39.1 | 2.39.1 | [Go module registry](https://pkg.go.dev/github.com/danielgtaylor/huma/v2@v2.39.1) |
| `github.com/rs/xid` | 1.6.0 | 1.6.0 | [Go module registry](https://pkg.go.dev/github.com/rs/xid@v1.6.0) |
| `golang.org/x/crypto` | 0.57.0 | 0.58.0 | [Go module registry](https://pkg.go.dev/golang.org/x/crypto@v0.58.0) |
| `modernc.org/sqlite` | 1.60.1 (with `modernc.org/libc` 1.77.1) | 1.60.1 (with `modernc.org/libc` 1.78.0) | [Go module registry](https://pkg.go.dev/modernc.org/sqlite@v1.60.1) |
| Indirect build/runtime modules | `go-humanize` 1.0.1; `go-strftime` 1.0.0; `x/sys` 0.48.0 | `go-humanize` 1.1.0; `go-strftime` 1.1.0; `x/sys` 0.49.0; `go-isatty` 0.0.24, `modernc/memory` 1.12.1 and the other indirect modules unchanged | [Go module proxy](https://proxy.golang.org/) |

Only modules that the repository's packages import were upgraded and tidied
(`go list -m -u` over the package graph). Huma declares optional adapters for
routers Manifold does not import (Gin, Fiber, Echo and others); their newer
releases were left unchanged instead of expanding the update into unrelated
integrations.

### npm packages

| Package | Baseline | Selected | Source |
| --- | --- | --- | --- |
| `mithril` | 2.3.8 | 2.3.8 | [npm registry](https://registry.npmjs.org/mithril/latest) |
| `@types/mithril` | 2.2.9 | 2.2.9 | [npm registry](https://registry.npmjs.org/%40types%2Fmithril/latest) |
| `esbuild` | 0.28.2 | 0.28.2 | [npm registry](https://registry.npmjs.org/esbuild/latest) |
| `typescript` | 7.0.2 | 7.0.2 | [TypeScript 7 announcement](https://devblogs.microsoft.com/typescript/announcing-typescript-7-0/), [npm registry](https://registry.npmjs.org/typescript/latest) |

All four packages were already at the newest published version on the audit
date (`npm view`). TypeScript 7 is a native rewrite and has no stable programmatic API yet. This
repository invokes `tsc` as a command-line checker and does not embed the
compiler, so that limitation does not block this upgrade. The project already
sets its target, module mode, module resolution, and strictness explicitly.
The `npm ci`, type-check, and production bundle checks are part of CI.

## OpenViking service

| Surface | Baseline | Selected | Compatibility note and source |
| --- | --- | --- | --- |
| OpenViking image | `v0.4.22@sha256:f52ab2c309e46e47a746ffc5715bb280111588ff27bc7c9ae62ef5b915df9de1` | `v0.5.0@sha256:c60a83cd79cfce80db5266acb8bdd0908eee8abe8ff559a5f786aa504c251ccf` | Latest stable release on the audit date, pinned by manifest digest. [v0.5.0 release](https://github.com/volcengine/OpenViking/releases/tag/v0.5.0), [v0.4.23 release](https://github.com/volcengine/OpenViking/releases/tag/v0.4.23) |

The Manifold adapter uses content write, search, and snapshot operations. The
v0.4.20 source preserves `POST /api/v1/content/write` with `wait` and `timeout`;
it does not return a per-write task identifier or expose a write-status poll
route. With `wait: true`, the request waits for work associated with that write.
A timeout is not proof that the write was rolled back: content is written and
post-processing is started before the queue wait. In v0.4.22 that wait covers
every queue item registered for the request, including the semantic refresh it
enqueues for each ancestor folder up to the root, and an expired `timeout`
returns HTTP 504 `DEADLINE_EXCEEDED`
([content_write.py](https://raw.githubusercontent.com/volcengine/OpenViking/v0.4.22/openviking/storage/content_write.py)).
The adapter therefore uses a dedicated write deadline and, after a 504 or a
client deadline, reads the content back with `GET /api/v1/content/read?raw=true`.
A matching read-back completes the write with indexing still pending; anything
else keeps the original retryable error. `processing_mode: "vectors_only"` would
skip the summaries, but it vectorizes the file with an empty summary, and the
v0.4.22 retriever starts from L0/L1 records. Whether new documents stay findable
in that mode is unverified, so Manifold does not use it. Unreleased
OpenViking `main` removes the write `timeout` field (#6048); revisit this adapter
before upgrading past v0.4.22.

The v0.4.20 write response can be HTTP 200 while reporting
`semantic_status: "failed"` or `vector_status: "failed"` when queue work has
errors; it can also report `queued` or `deferred` when processing was not
completed. For a waited write, Manifold treats only `complete` or `skipped` as
success. The service forces a semantic refresh for waited writes, which bypasses
the normal freshness deferral decision. The contract is visible in
[content_write.py](https://raw.githubusercontent.com/volcengine/OpenViking/v0.4.20/openviking/storage/content_write.py)
and the [freshness policy](https://raw.githubusercontent.com/volcengine/OpenViking/v0.4.20/openviking/storage/queuefs/semantic_ops/freshness_policy.py).

Search compatibility is preserved for Manifold's current request: v0.4.20
removes the older `agent_id` and `agent_uri` fields, which Manifold does not
send, and validates `target_uri` as a Viking URI. Snapshot commit results still
provide `result.commit_oid`; v0.4.20 adds `result`, `changed`, and `ignored`
metadata. The documented OID remains a 40-hex SHA-1. Existing stored snapshot
OIDs are external pointers and must be preserved exactly across migration and
verified by round-trip reads, not regenerated. See the v0.4.10 and
[v0.4.20 search request models](https://raw.githubusercontent.com/volcengine/OpenViking/v0.4.20/openviking/server/routers/search.py)
and the [snapshot API contract](https://github.com/volcengine/OpenViking/blob/v0.4.20/docs/en/api/11-snapshot.md).

Other v0.4.20 changes are outside Manifold's current calls but matter to future
integrations: Compile moved from `/bot/v1/compile` to `/api/v1/compile` plus
task polling, Compile CLI wait/timeout flags were removed, and the default
memory extraction output changed from JSON to a restricted Python format.
Manifold does not call Compile or invoke the OpenViking CLI; deployments that
depend on the old extraction format should explicitly retain JSON in OpenViking
configuration. The release's [upgrade notes](https://github.com/volcengine/OpenViking/releases/tag/v0.4.20)
list these changes.

The v0.4.21 and v0.4.22 changes keep every route Manifold calls. `mkdir` and
`content/write` accept an optional `acl`, `write` accepts `tag_mode`, and
search scores are normalized to [0, 1]; Manifold fuses by rank, so the
normalization does not change its ordering. Unknown configuration fields now
log a warning instead of blocking startup, so review the startup log after an
upgrade. The image's default working directory became `/app/.openviking`;
Manifold's configuration uses absolute paths.

The v0.4.23 and v0.5.0 changes keep every route Manifold calls. v0.4.23 replaces
per-directory recursion with one global recall and one rerank, so scores and
ordering for the same query differ from v0.4.22; Manifold fuses by rank and
canonicalizes every hit through SQLite. `/health` now answers an expired or
invalid API key with an authentication error, while requests without one still
succeed. v0.5.0 turns session Working Memory off by default (Manifold writes
resources, not sessions), serializes local embedding calls, and adds the
Gateway as a separate process that Manifold does not run. Neither release
speeds up content writes. Unreleased OpenViking `main` removes the
wait-specific `timeout` request field (#6048); Manifold's content write still
sends it, so review that change before moving past v0.5.0.

## GitHub Actions

The workflow already used the current stable major tags for `actions/checkout`
v7, `actions/setup-go` v7, `actions/setup-node` v7, Docker QEMU v4, Docker
Buildx v4, and `docker/build-push-action` v7; those tags remain. Python setup
moved from `actions/setup-python@v6` to `@v7` in the previous audit; no action
needed a change in this one (`actions/setup-node` is at 7.1.0).
Workflow runtimes now match the pinned versions above. The compose job also runs
the production deploy-script tests and checks all Brain patches against its
pinned upstream commit.

Sources: [checkout releases](https://github.com/actions/checkout/releases),
[setup-go releases](https://github.com/actions/setup-go/releases),
[setup-node releases](https://github.com/actions/setup-node/releases),
[setup-python v7.0.0](https://github.com/actions/setup-python/releases/tag/v7.0.0),
[QEMU action releases](https://github.com/docker/setup-qemu-action/releases),
[Buildx action releases](https://github.com/docker/setup-buildx-action/releases),
and [build-push action releases](https://github.com/docker/build-push-action/releases).

## Verification

The repository CI verifies Go tests, race tests, vet, TypeScript checking,
esbuild output, OpenAPI, skill binary builds, Compose configuration, production
deployment-script tests, Brain patch application, integration behavior, and
multi-architecture image builds. Dependency installation and local test results
are recorded with the change review; Docker host tool versions are host state,
not pinned repository dependencies.
