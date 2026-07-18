# Manifold repository instructions

## Product boundary

- Manifold is a self-hosted REST and OpenAPI service. Do not add, expose, document, or advertise MCP.
- `internal/store` owns public IDs, workflow state, jobs, revisions, idempotency, and upstream mappings.
- OpenViking owns canonical folder/document content and snapshot objects.
- INITE Brain owns derived entities, facts, relations, provenance, and conflicts.
- Upstream identifiers are opaque internal values. Never expose them in API, UI, logs, or skill output.

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
- OpenViking and Brain need outbound provider access but no published ports. Keep data traffic on the internal `private` network and model API egress on the un-published `provider` network.

## Skill distribution

- `skills/manifold` is the source of truth for the external agent skill.
- Distribute and update it with the standard `npx skills` CLI. Do not add a repository-specific installer or document manual copies into agent directories.
- Installation may require Node.js/npm; the installed skill must remain runtime-free through its bundled native binaries.
- Keep endpoint and bearer credentials outside the skill. Target an instance only through `MANIFOLD_URL` and `MANIFOLD_API_KEY`.

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

Run `make integration` for changes that cross the API, durable worker, OpenViking, Brain, SurrealDB, or restart persistence. It uses only the repository's deterministic OpenAI-compatible test provider and destroys only its own `manifold-integration` Compose volumes.

For changes to the pinned Brain integration, also verify:

```bash
git -C /path/to/inite-brain-service-v0.8.1 apply --check deploy/brain/openai-base-url.patch
```

Report skipped checks explicitly. Never claim a real-provider integration passed when only deterministic fakes ran.
