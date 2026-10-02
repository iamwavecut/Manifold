# Dependency audit

Audited 2026-10-02 against upstream release pages and registries (previous
audit 2026-09-16, whose selections are the baselines below). The matrix
records stable releases available on that date; it is a dated snapshot.

## Brain and graph storage

| Surface | Baseline | Selected | Compatibility evidence |
| --- | --- | --- | --- |
| INITE Brain | v2.2.0 | v2.3.0, source `0f3d16db0d175794b79b73ef186351aa9f2da3c0` | [Official release](https://github.com/inite-ai/inite-brain-service/releases/tag/v2.3.0); [adapter and patch contracts](brain-patch.md). Adapter routes are unchanged; candidates gain optional `sourceVersion`. Migrations 0130–0160 apply on boot and are one-way. |
| SurrealDB | v3.2.4 | v3.3.0, image digest `sha256:681c6c22c287421b5c7d99e0fde79b6e0d32c36c1ddeaab2762a1661cb04cd20` | [Release notes](https://surrealdb.com/releases/3.3). Storage migrates on first start and 3.2 cannot read the result, so rollback needs the pre-upgrade backup. Fixes advisories that affect v3.2.4. Brain v2.3.0 upstream still pins v3.2.4; Manifold verifies v3.3.0 through its integration run. |

Brain builds on Node 22 LTS (22.23.3, the same major line as its upstream
Dockerfile) with the upstream frozen pnpm lockfile. Its dependency graph is taken as a tested upstream release; Manifold
does not independently rewrite that graph or substitute Node 26 beneath native
bindings. Production activation crosses `DATA_VERSION` 3 and follows the
[coordinated migration procedure](operations.md#data-version-3-migration).

## Runtime and container images

| Surface | Baseline | Selected | Compatibility note and source |
| --- | --- | --- | --- |
| Go language floor (`go.mod`) | 1.26 | 1.27.0 | Go 1.27 is the current stable major. The Go release history lists 1.27.1 as the current patch, with compiler, runtime, cgo, and standard-library fixes. [Go release history](https://go.dev/doc/devel/release) |
| Go build toolchain (CI and Docker) | 1.26.x | 1.27.1 | CI and both Go build stages are pinned to the latest 1.27 patch. Huma 2.39.1 requires Go 1.25 or newer. [Go releases](https://go.dev/dl/), [Huma v2.39.1](https://pkg.go.dev/github.com/danielgtaylor/huma/v2@v2.39.1) |
| Node.js / bundled npm | Node 26.8.2 | Node 26.10.0 | Latest stable 26 release. [Node releases](https://nodejs.org/en/about/previous-releases) |
| Python used by CI checks | 3.13 | 3.14.7 | The Python job only runs repository scripts; it has no third-party Python packages. 3.14.7 is the latest 3.14 maintenance release. [Python 3.14.7](https://www.python.org/downloads/release/python-3147/) |
| Node build image | `node:26.8.2-alpine3.24` | `node:26.10.0-alpine3.24` | Exact official image tag. [Official Node image tags](https://hub.docker.com/_/node/tags?name=26.10.0-alpine3.24) |
| Go build images | `golang:1.26-alpine` | `golang:1.27.1-alpine3.24` | Exact official image tag; multi-architecture manifest verified. [Official Go image tags](https://hub.docker.com/_/golang/tags?name=1.27.1-alpine3.24) |
| Runtime images | `alpine:3.24.1` | `alpine:3.24.2` | Exact official Alpine patch tag. [Official Alpine image tags](https://hub.docker.com/_/alpine/tags?name=3.24.2) |

## Application packages

### Go modules

| Module | Baseline | Selected | Source |
| --- | --- | --- | --- |
| `github.com/danielgtaylor/huma/v2` | 2.39.0 | 2.39.1 | [Go module registry](https://pkg.go.dev/github.com/danielgtaylor/huma/v2@v2.39.1) |
| `github.com/rs/xid` | 1.6.0 | 1.6.0 | [Go module registry](https://pkg.go.dev/github.com/rs/xid@v1.6.0) |
| `golang.org/x/crypto` | 0.52.0 | 0.57.0 | [Go module registry](https://pkg.go.dev/golang.org/x/crypto@v0.57.0) |
| `modernc.org/sqlite` | 1.59.0 | 1.60.1 (with `modernc.org/libc` 1.77.1) | [Go module registry](https://pkg.go.dev/modernc.org/sqlite@v1.60.1) |
| Indirect build/runtime modules | `go-isatty` 0.0.22; `go-strftime` 0.1.9; `x/sys` 0.45.0; `modernc/libc` 1.66.10; `modernc/memory` 1.11.0 | `go-isatty` 0.0.24; `go-strftime` 1.0.0; `x/sys` 0.48.0; `modernc/libc` 1.76.0; `modernc/memory` 1.12.1; `x/tools` 0.50.0 | [Go module proxy](https://proxy.golang.org/) |

The Go graph was upgraded for packages used by the repository and tidied.
`x/exp` is no longer selected. `xid`, `go-humanize`, `google/uuid`,
`remyoudompheng/bigfft`, and `modernc/mathutil` had no newer stable versions in
the selected graph. Huma declares optional adapters for routers Manifold does
not import; those unused branches were left unchanged instead of expanding the
upgrade into unrelated integrations.

### npm packages

| Package | Baseline | Selected | Source |
| --- | --- | --- | --- |
| `mithril` | 2.3.8 | 2.3.8 | [npm registry](https://registry.npmjs.org/mithril/latest) |
| `@types/mithril` | 2.2.7 | 2.2.9 | [npm registry](https://registry.npmjs.org/%40types%2Fmithril/latest) |
| `esbuild` | 0.28.1 | 0.28.2 | [npm registry](https://registry.npmjs.org/esbuild/latest) |
| `typescript` | 6.0.2 | 7.0.2 | [TypeScript 7 announcement](https://devblogs.microsoft.com/typescript/announcing-typescript-7-0/), [npm registry](https://registry.npmjs.org/typescript/latest) |

TypeScript 7 is a native rewrite and has no stable programmatic API yet. This
repository invokes `tsc` as a command-line checker and does not embed the
compiler, so that limitation does not block this upgrade. The project already
sets its target, module mode, module resolution, and strictness explicitly.
The `npm ci`, type-check, and production bundle checks are part of CI.

## OpenViking service

| Surface | Baseline | Selected | Compatibility note and source |
| --- | --- | --- | --- |
| OpenViking image | `v0.4.20` | `v0.4.22@sha256:f52ab2c309e46e47a746ffc5715bb280111588ff27bc7c9ae62ef5b915df9de1` | Latest stable release on the audit date, pinned by manifest digest. [v0.4.22 release](https://github.com/volcengine/OpenViking/releases/tag/v0.4.22), [v0.4.21 release](https://github.com/volcengine/OpenViking/releases/tag/v0.4.21) |

The Manifold adapter uses content write, search, and snapshot operations. The
v0.4.20 source preserves `POST /api/v1/content/write` with `wait` and `timeout`;
it does not return a per-write task identifier or expose a write-status poll
route. With `wait: true`, the request waits for work associated with that write.
A timeout is not proof that the write was rolled back: content is written and
post-processing is started before the queue wait. The adapter therefore uses a
dedicated write deadline, treats a timeout as an uncertain completed write, and
must reconcile before retrying.

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

## GitHub Actions

The workflow already used the current stable major tags for `actions/checkout`
v7, `actions/setup-go` v7, `actions/setup-node` v7, Docker QEMU v4, Docker
Buildx v4, and `docker/build-push-action` v7; those tags remain. Python setup
was the only stale action and moved from `actions/setup-python@v6` to `@v7`.
Workflow runtimes now match the pinned versions above. The compose job also runs
the production deploy-script tests and checks all Brain patches against its
pinned upstream release.

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
