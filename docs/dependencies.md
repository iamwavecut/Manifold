# Dependency audit

Audited 2026-11-28 for v0.3.0 (previous audit 2026-10-14, whose selections
are the baselines below). The matrix records the stable releases available on
that date. It is a dated snapshot.

v0.3.0 removes three services and their dependency graphs: OpenViking
v0.5.0, INITE Brain v2.11.0 (with its Node 24 and pnpm graph) and SurrealDB
v3.3.5. The embedded index uses SQLite FTS5 and the sqlite-vec extension that
`modernc.org/sqlite` ships as the `modernc.org/sqlite/vec` package, so it adds
no module.

## Runtime and container images

| Surface | Baseline | Selected | Compatibility note and source |
| --- | --- | --- | --- |
| Go language floor (`go.mod`) | 1.27.0 | 1.27.0 | Unchanged. |
| Go build toolchain (CI and Docker) | 1.27.2 | 1.27.4 | Latest stable Go release on the audit date. [Go downloads JSON](https://go.dev/dl/?mode=json) |
| Node.js / bundled npm | Node 26.11.1 | Node 26.13.1 | Latest 26.x release on the audit date. [Node release index](https://nodejs.org/dist/index.json) |
| Python used by CI checks | 3.14.8 | 3.14.8 | Not re-audited; the Python job only runs repository scripts. |
| Node build image | `node:26.11.1-alpine3.24` | `node:26.13.1-alpine3.24` | Exact official image tag. No `alpine3.25` variant exists yet. |
| Go build images | `golang:1.27.2-alpine3.24` | `golang:1.27.4-alpine3.24` | Exact official image tag. No `alpine3.25` variant exists yet. |
| Manifold runtime image | `alpine:3.24.2` with `ca-certificates`, `tzdata`, `wget` | `gcr.io/distroless/static-debian13:nonroot` | The binary is static (`CGO_ENABLED=0`). The base has CA certificates and time zones but no shell or package manager; the healthcheck is `manifold healthcheck`. The process runs as UID 100 and GID 101, the owner of existing Alpine-created volumes. |
| Test provider runtime image | `alpine:3.24.2` | `gcr.io/distroless/static-debian13:nonroot` | Same base; `provider healthcheck` and `provider control` replace `wget`. |

## Application packages

### Go modules

| Module | Baseline | Selected | Source |
| --- | --- | --- | --- |
| `github.com/danielgtaylor/huma/v2` | 2.39.1 | 2.41.0 | [Go module registry](https://pkg.go.dev/github.com/danielgtaylor/huma/v2@v2.41.0) |
| `github.com/rs/xid` | 1.6.0 | 1.6.0 | [Go module registry](https://pkg.go.dev/github.com/rs/xid@v1.6.0) |
| `golang.org/x/crypto` | 0.58.0 | 0.59.0 | [Go module registry](https://pkg.go.dev/golang.org/x/crypto@v0.59.0) |
| `modernc.org/sqlite` | 1.60.1 (with `modernc.org/libc` 1.78.0) | 1.63.0 (with `modernc.org/libc` 1.80.1) | [Go module registry](https://pkg.go.dev/modernc.org/sqlite@v1.63.0). Provides FTS5 and `modernc.org/sqlite/vec`. |
| Indirect modules | `x/sys` 0.49.0 | `x/sys` 0.50.0; the others unchanged | [Go module proxy](https://proxy.golang.org/) |

`go list -m -u` reported no newer version of a direct module after the update.

### npm packages

| Package | Baseline | Selected | Source |
| --- | --- | --- | --- |
| `mithril` | 2.3.8 | 2.3.8 | [npm registry](https://registry.npmjs.org/mithril/latest) |
| `@types/mithril` | 2.2.9 | 2.2.9 | [npm registry](https://registry.npmjs.org/%40types%2Fmithril/latest) |
| `esbuild` | 0.28.2 | 0.28.2 | [npm registry](https://registry.npmjs.org/esbuild/latest) |
| `typescript` | 7.0.2 | 7.0.2 | [npm registry](https://registry.npmjs.org/typescript/latest) |

`npm outdated` reported no newer version on the audit date.

## GitHub Actions

The workflows use the current stable major tags for `actions/checkout` v7,
`actions/setup-go` v7, `actions/setup-node` v7, `actions/setup-python` v7,
Docker QEMU v4, Docker Buildx v4, and `docker/build-push-action` v7. The
compose job no longer fetches or patches Brain, and the production workflow
builds one image.

## Verification

The repository CI verifies Go tests, race tests, vet, TypeScript checking,
esbuild output, OpenAPI, skill binary builds, Compose configuration, production
deployment-script tests, integration behavior, and multi-architecture image
builds. Docker host tool versions are host state, not pinned repository
dependencies.
