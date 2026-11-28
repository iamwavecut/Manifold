# Manifold repository instructions

## Product boundary

- Manifold is a self-hosted REST and OpenAPI service. Agents connect through the bundled Manifold CLI and HTTP API, not gRPC or MCP. An absent MCP tool is not evidence of an unavailable service. Do not add, expose, document, or advertise MCP.
- `internal/store` owns canonical documents and revisions, public IDs, workflow state, jobs, idempotency, and index mappings (`manifold.db`).
- `internal/engine` owns the derived index (`index.db`): document chunks, extracted entities, facts and relations, full-text and vector search. It must stay rebuildable: a missing `index.db` is seeded from `manifold.db` at start without model calls for revisions already extracted.
- `internal/llm` is the only client of the OpenAI-compatible model provider. Errors carry a refusal class and never the response body or the key.
- Index identifiers are opaque internal values. Never expose them in API, UI, logs, or skill output.

## Identity invariants

- Use semantic ASCII slugs for folders, documents, entities, sources, and API-key records.
- Use `github.com/rs/xid` for facts, relations, conflicts, jobs, requests, and rename plans.
- Use `rN` for revisions and `document@rN#chunk-N` for chunks.
- Never introduce UUIDs into the Manifold model.
- Display names never rename IDs. Only a reviewed rename plan may change a slug.

## Rename invariants

- A preview is bound to the SQLite state version and must fail with `stale_rename_plan` after any relevant state change.
- Validate the entire batch before apply. Swaps and cycles must remain supported.
- Keep historical revisions immutable. Rewrite only active UTF-8 content and create a new revision.
- Block binary references with `unrewritable_reference` and an exact resource/location.
- Do not create aliases or redirects. Preserve an audit record only.
- Keep the active SQLite phase transactional. On any local failure, old IDs and references must remain active.

## Error contract

- Every public failure is `application/problem+json` using `internal/problem.Error`.
- Every response includes stable `code`, XID `request_id`, retry safety, remediation, and `documentation_url`.
- Validation reports all independent fields with JSON Pointers.
- Never return raw upstream payloads, SQL, stack traces, secrets, private content, or internal network addresses.
- Add every new public code to `semanticErrorDocs`, `docs/errors.md`, OpenAPI examples, skill exit handling where relevant, and negative tests.
- Async jobs persist the same semantic error object returned by synchronous surfaces.

## API and security

- Mutations require `Idempotency-Key`; replay records are scoped to the authenticated API-key ID.
- Concurrent changes use `ETag` and `If-Match`.
- Enforce the narrowest capability in `internal/auth`.
- API-key secrets, session cookies, CSRF tokens, and idempotency keys may be cryptographically random. Never use XIDs as secrets.
- UI sessions stay HttpOnly/SameSite and unsafe session requests require CSRF.
- Manifold runs as one container with no database server or sidecar. It needs outbound access to the model provider and publishes only the API port.
- Canonicalize all retrieval hits through SQLite before returning them. Never expose index locators or unmapped index IDs.
- Document creation by `folder_path` must create missing nested folders and the document in one SQLite transaction. Keep `folder_id` and `folder_path` mutually exclusive.
- Path selectors use slash-aware `*`, `?`, and whole-segment `**`; validate them before querying.

## Skill distribution

- `skills/manifold` is the source of truth for the external agent skill.
- Distribute and update it with the standard `npx skills` CLI. Do not add a repository-specific installer or document manual copies into agent directories.
- Public installation instructions must use only `--skill manifold --global`; leave target selection and confirmation to the standard CLI.
- Installation may require Node.js/npm; the installed skill must remain runtime-free through its bundled native binaries.
- Keep endpoint and bearer credentials outside the skill. Target an instance only through `MANIFOLD_URL` and `MANIFOLD_API_KEY`.
- Keep `remember` discovery-first: read candidates before update/new decisions, require an explicit reason to create beside related memory, never blind-retry ETag conflicts, and wait for terminal job state.

## Generated surfaces

- Huma registrations are the API source of truth.
- Regenerate `api/openapi.yaml` with `make openapi`.
- `make openapi-check` must be clean before commit.
- Build `web/dist/app.js` with `npm run build`; it is embedded in the Go binary.

## Verification

Run the narrowest relevant test first, then:

```bash
make verify
docker compose --env-file .env.example config --quiet
docker buildx build --platform linux/amd64,linux/arm64 --target build .
```

Run `make integration` for changes that cross the API, durable worker, embedded index, model provider, seeding, or restart persistence. It uses only the repository's deterministic OpenAI-compatible test provider and destroys only its own `manifold-integration` Compose volumes.

Report skipped checks explicitly. Never claim a real-provider integration passed when only deterministic fakes ran.
