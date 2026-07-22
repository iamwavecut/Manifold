# Plan 001: Make the Manifold skill/server protocol skew-safe

> **Executor instructions**: Follow every step and run every verification gate.
> Do not deploy or release in this plan. If a STOP condition occurs, report it
> rather than weakening retrieval or mutation safety.
>
> **Drift check**: `git diff --stat e03651e..HEAD -- skills/manifold internal/api internal/service cmd/manifold Dockerfile docker-compose.yml api docs README.md scripts`
>
> If the request builders or status types no longer match the excerpts below,
> stop and re-plan against the current code.

## Status

- **Priority**: P0
- **Effort**: M
- **Risk**: MEDIUM; request shaping and compatibility gates affect every agent
  retrieval and memory mutation.
- **Depends on**: none
- **Category**: bug, API contract, DX
- **Planned at**: commit `e03651e`, 2026-07-22

## Why this matters

The skill and server are distributed independently. Additive request fields are
not backward compatible with an older Huma server because request schemas use
`additionalProperties: false` and unknown query parameters are rejected. The
current client serializes new optional selectors even when the agent did not
ask for them, turning a normal version skew into a total read-only outage.

The fix must preserve base retrieval against older servers, reject genuinely
unsupported advanced behavior before mutation, and give agents a stable
machine-readable explanation instead of a generic validation failure.

## Current state

- `skills/manifold/cmd/manifold/main.go:256-259` always sends
  `scope_glob` and `source_types` from `search`, including `""` and `[]`.
- `skills/manifold/cmd/manifold/main.go:276-279` does the same for `context`.
- `skills/manifold/cmd/manifold/main.go:230-249` always sends tree defaults as
  query parameters.
- `skills/manifold/cmd/manifold/main.go:364-367` sends
  `source_types:["document"]` during every `remember` discovery pass.
- `skills/manifold/cmd/manifold/main_test.go:50-205` tests the remember state
  machine but never asserts the exact JSON body or legacy schema compatibility.
- `internal/api/api.go:43-59` hard-codes OpenAPI version `0.1.0` and has no build
  or protocol metadata.
- `internal/service/service.go:31-37,81-84` hard-codes status version `0.1.0`,
  independent of the actual binary.
- `cmd/manifold/main.go:25,91` has an ldflag `version`, but it is only logged.
- `scripts/build-skill-binaries.sh` defaults every committed skill binary to
  version `dev`.
- Production proves that base tree, search, and context work when unsupported
  optional values are omitted.

## Target contract

Add a public, non-sensitive `GET /api/v1/meta` response with a concrete schema:

```json
{
  "service": "manifold",
  "version": "0.2.0",
  "commit": "<full build SHA or unknown in dev>",
  "api_major": 1,
  "protocol_revision": 2,
  "features": [
    "retrieval.scope_glob",
    "retrieval.source_types",
    "tree.glob",
    "documents.folder_path",
    "memory.remember.v1"
  ]
}
```

The endpoint must not expose dependency addresses, credentials, counts, or
private state. Mirror the build/protocol fields in authenticated status, but do
not require status capability just to negotiate the public protocol.

Legacy behavior is defined explicitly:

- If `/api/v1/meta` is absent or not valid JSON, treat the server as legacy with
  no advertised advanced features.
- Unscoped `search` and `context` remain available by omitting empty optional
  fields.
- Bare `tree` uses `/api/v1/tree` without query parameters on a legacy server.
- Explicit advanced selectors on a legacy server fail locally as
  `server_incompatible`; they are never silently ignored.
- Every `remember` operation requires `memory.remember.v1`. A legacy server is
  rejected before discovery or mutation; no request may be partially applied.
- A newer server may add response fields: the client continues to ignore
  unknown response members.

## Commands

| Purpose | Command | Expected result |
| --- | --- | --- |
| Skill unit tests | `go test ./skills/manifold/cmd/manifold` | exit 0 |
| API tests | `go test ./internal/api ./internal/service` | exit 0 |
| OpenAPI drift | `make openapi && make openapi-check` | exit 0, clean diff after generated file is staged |
| Full verification | `make verify` | exit 0 |
| Full stack | `MANIFOLD_PORT=28080 make integration` | exit 0 and cleanup succeeds |

## Suggested executor toolkit

- Use the repository's `use-modern-go` skill for Go changes.
- Follow `AGENTS.md` error and generated-surface invariants.
- Use the existing `problem` rendering and exit-code tables rather than parsing
  server prose.

## Scope

**In scope**:

- `skills/manifold/cmd/manifold/main.go`
- `skills/manifold/cmd/manifold/main_test.go`
- `skills/manifold/SKILL.md`
- `skills/manifold/references/error-codes.md`
- `skills/manifold/agents/openai.yaml`
- `skills/manifold/bin/*/manifold` (regenerated)
- `scripts/build-skill-binaries.sh`
- `scripts/validate-skill.py`
- `internal/api/api.go`
- `internal/api/system.go`
- `internal/api/api_test.go`
- `internal/service/service.go`
- `internal/service/service_test.go`
- `cmd/manifold/main.go`
- `Dockerfile`, `docker-compose.yml`, `.env.example`
- `api/openapi.yaml`
- `README.md`, `docs/skill.md`, `docs/errors.md`, `docs/operations.md`, `AGENTS.md`
- `scripts/integration.sh`, `test/integration/integration_test.go`
- a small version/build-info source file if needed

**Out of scope**:

- weakening Huma's unknown-field rejection;
- auto-retrying mutations;
- parsing `detail` or violation messages to infer compatibility;
- aliases for removed API behavior;
- production deployment or tagging;
- Brain/SurrealDB behavior, which belongs to Plan 002.

## Steps

### Step 1: Introduce one source of truth for release and protocol identity

Create a small typed build/protocol model shared by server construction and
status output. Set the development defaults explicitly (`version=dev`,
`commit=unknown`) and make release/build ldflags override them. Do not leave
`service.Status.Version` or Huma's Info version hard-coded separately.

Use a stable constant list for protocol revision 2 features. Sort it once in
source so status and OpenAPI examples are deterministic.

**Verify**: add a unit test that constructs a server with known build data and
asserts `/api/v1/meta`, `/api/v1/status`, and OpenAPI Info report the same
version/commit and feature list.

### Step 2: Add the public metadata endpoint

Register `GET /api/v1/meta` with an explicit response struct and no auth. Keep
health and readiness unchanged. Ensure the response cannot inherit component
health, counts, URLs, or configuration.

**Verify**: API tests prove unauthenticated `200`, exact typed keys, no secret or
dependency fields, and a generated OpenAPI schema for the endpoint.

### Step 3: Omit zero-value optional request fields

Replace the literal search/context maps with small request-builder functions.
Only add `scope_glob` when non-empty and `source_types` when at least one type
was requested. Preserve base fields supported by the old server.

For tree, distinguish default from explicitly requested behavior. A legacy
server gets a bare `/api/v1/tree` only for the unfiltered default. A server that
advertises `tree.glob` receives the current selector parameters.

**Verify**: use strict `httptest` handlers with `DisallowUnknownFields` and
unknown-query rejection to model the production pre-`e03651e` contract. Assert
that unscoped context/search and bare tree succeed and that their exact request
bodies/URL contain no new optional names.

### Step 4: Negotiate advanced features without unsafe fallback

Add a client metadata probe cached for the single CLI process. Invalid/missing
metadata means legacy, not unreachable. Network/auth failures on actual
commands retain their current semantic handling.

Before sending an explicitly scoped retrieval, filtered tree, or any
`remember`, compare required features. Return a local semantic problem:

- code: `server_incompatible`;
- stable new exit code: 23;
- detail: command and missing feature names, without server internals;
- remediation: ask the operator to deploy a compatible Manifold release, run
  `status`, then retry; for read-only retrieval only, suggest an unscoped base
  search when that preserves the agent's intent.

Do not silently drop an explicitly requested scope/type filter. Do not permit
`remember` against a server without `memory.remember.v1`.

**Verify**: tests cover legacy base reads, legacy explicit-feature rejection,
legacy remember rejection with zero mutation calls, new-server advanced reads,
and new-server remember continuation.

### Step 5: Make versions real in binaries and containers

Make committed skill binaries report `0.2.0` rather than `dev`; keep rebuilds
reproducible. Pass release version and exact commit separately into the server
binary. Remove the stale pattern where `.env` can claim a commit unrelated to
the checked-out source; deployment will supply build identity explicitly.

Regenerate all four skill binaries and OpenAPI.

**Verify**:

- `sh skills/manifold/scripts/manifold --version` includes `0.2.0` and protocol
  revision 2 in a documented stable format;
- a locally built server reports the same version and supplied test commit;
- `git diff --exit-code -- skills/manifold/bin` is clean after the documented
  deterministic rebuild command.

### Step 6: Document compatibility behavior for agents and operators

Update the skill and docs so an agent understands `server_incompatible`, may
fall back only to a semantically acceptable unscoped read, and never writes
through an unverified legacy server. Add the code to the skill reference and
validation script. Add compatibility/version information to operations docs.

**Verify**: `make skill-check` passes and targeted text checks find the new code,
exit status, and no instruction to parse free-form error text.

### Step 7: Add a two-version contract fixture

Keep a minimal legacy test server/schema fixture in Go tests or integration
test support. It must remain intentionally frozen and cover the production
contract seen on 2026-07-22. Run the current bundled client against both legacy
and current fixtures in CI.

The fixture must prove:

1. legacy base context/search/tree work;
2. advanced legacy reads return `server_incompatible` rather than raw
   `validation_failed`;
3. legacy remember makes zero mutations;
4. current selectors and remember still work.

**Verify**: `make verify` and the isolated integration suite pass.

## Test plan

- Exact request serialization tests for empty and non-empty selectors.
- Metadata endpoint auth/data-minimization tests.
- Feature negotiation table tests for every CLI command that gained options in
  `e03651e`.
- Negative test that an explicit scope is never dropped.
- Negative test that no compatibility path retries a mutation.
- OpenAPI drift and semantic error documentation tests.
- Reproducibility checks for all runtime-free binaries.

## Done criteria

- [ ] The installed client can run unscoped context, search, and bare tree
      against the frozen legacy fixture.
- [ ] Advanced legacy commands fail with `server_incompatible`/exit 23 and list
      required features.
- [ ] Legacy `remember` performs zero mutation calls.
- [ ] `/api/v1/meta` and status identify version, exact commit, protocol
      revision, and features consistently.
- [ ] No server or skill version remains hard-coded independently.
- [ ] `make verify` passes.
- [ ] `MANIFOLD_PORT=28080 make integration` passes and leaves no containers or
      volumes.
- [ ] Only in-scope files plus generated artifacts changed.

## STOP conditions

- The old server cannot execute a base retrieval without fields that are absent
  from its OpenAPI contract.
- Feature negotiation would require a key capability not required by the
  intended command.
- Supporting a legacy mutation would weaken discovery, ETag, idempotency, or
  atomic folder guarantees.
- The proposed version source makes committed binaries non-reproducible.

## Maintenance notes

Every future optional request field must have an omission test against the
frozen previous protocol fixture. New behavior that cannot be represented by
omitting a field must receive a feature name and a local compatibility error
before the skill begins depending on it.

