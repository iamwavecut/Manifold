# Dependency audit

Audited 2026-09-16 against upstream release pages and registries. The matrix
records stable releases available on that date; it is a dated snapshot.

## Brain and graph storage

| Surface | Baseline | Selected | Compatibility evidence |
| --- | --- | --- | --- |
| INITE Brain | v0.8.1 | v2.2.0, source `b19b209fec92069d91df22af61db18b0a810ce82` | [Official release](https://github.com/inite-ai/inite-brain-service/releases/tag/v2.2.0); [adapter and patch contracts](brain-patch.md) |
| SurrealDB | v3.1.5 | v3.2.4, image digest `sha256:51baed8709f57f67dcf04b30e3177db846803fa9342dae2be58c6fa5f8d59843` | [Official release](https://github.com/surrealdb/surrealdb/releases/tag/v3.2.4); [same-volume migration verification](migration-verification.md) |

Brain retains its release's Node 22 image digest and upstream frozen pnpm
lockfile. Its dependency graph is taken as a tested upstream release; Manifold
does not independently rewrite that graph or substitute Node 26 beneath native
bindings. Production activation crosses `DATA_VERSION` and follows the
[coordinated migration procedure](operations.md#brain-v2--data-version-2-migration).

## Runtime and container images

| Surface | Baseline | Selected | Compatibility note and source |
| --- | --- | --- | --- |
| Go language floor (`go.mod`) | 1.26 | 1.27.0 | Go 1.27 is the current stable major. The Go release history lists 1.27.1 as the current patch, with compiler, runtime, cgo, and standard-library fixes. [Go release history](https://go.dev/doc/devel/release) |
| Go build toolchain (CI and Docker) | 1.26.x | 1.27.1 | CI and both Go build stages are pinned to the latest 1.27 patch. Huma 2.39.1 requires Go 1.25 or newer. [Go releases](https://go.dev/dl/), [Huma v2.39.1](https://pkg.go.dev/github.com/danielgtaylor/huma/v2@v2.39.1) |
| Node.js / bundled npm | Node 26 floating; npm unpinned | Node 26.8.2 / npm 11.19.1 | Node 26.8.2 is the latest stable 26 release at audit time and includes npm 11.19.1. Node 26 was already the repository's major, so this is a patch pin. [Node 26.8.2 release](https://nodejs.org/en/blog/release/v26.8.2) |
| Python used by CI checks | 3.13 | 3.14.7 | The Python job only runs repository scripts; it has no third-party Python packages. 3.14.7 is the latest 3.14 maintenance release. [Python 3.14.7](https://www.python.org/downloads/release/python-3147/) |
| Node build image | `node:26-alpine` | `node:26.8.2-alpine3.24` | Exact official image tag; multi-architecture manifest verified. [Official Node image tags](https://hub.docker.com/_/node/tags?name=26.8.2-alpine3.24) |
| Go build images | `golang:1.26-alpine` | `golang:1.27.1-alpine3.24` | Exact official image tag; multi-architecture manifest verified. [Official Go image tags](https://hub.docker.com/_/golang/tags?name=1.27.1-alpine3.24) |
| Runtime images | `alpine:3.22` | `alpine:3.24.1` | Exact official Alpine patch tag; multi-architecture manifest verified. [Official Alpine image tags](https://hub.docker.com/_/alpine/tags?name=3.24.1) |

## Application packages

### Go modules

| Module | Baseline | Selected | Source |
| --- | --- | --- | --- |
| `github.com/danielgtaylor/huma/v2` | 2.39.0 | 2.39.1 | [Go module registry](https://pkg.go.dev/github.com/danielgtaylor/huma/v2@v2.39.1) |
| `github.com/rs/xid` | 1.6.0 | 1.6.0 | [Go module registry](https://pkg.go.dev/github.com/rs/xid@v1.6.0) |
| `golang.org/x/crypto` | 0.52.0 | 0.57.0 | [Go module registry](https://pkg.go.dev/golang.org/x/crypto@v0.57.0) |
| `modernc.org/sqlite` | 1.39.1 | 1.59.0 | [Go module registry](https://pkg.go.dev/modernc.org/sqlite@v1.59.0) |
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
| OpenViking image | `v0.4.10` | `v0.4.20@sha256:b9827753d035f4157b5b318865907fd18f738924ad6398c86feb1182f209825b` | Latest stable release on the audit date, pinned by manifest digest. [v0.4.20 release](https://github.com/volcengine/OpenViking/releases/tag/v0.4.20), [GHCR image](https://github.com/volcengine/OpenViking/pkgs/container/openviking) |

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
