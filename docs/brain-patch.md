# INITE Brain patch

Manifold builds INITE Brain from the exact `v0.8.1` tag (`557babacfad9ffd856107a49d0cfc20d27016afe`) and applies two auditable patches:

- [`deploy/brain/openai-base-url.patch`](../deploy/brain/openai-base-url.patch)
- [`deploy/brain/scoped-session-recovery.patch`](../deploy/brain/scoped-session-recovery.patch)

The provider patch makes two deliberately small changes:

1. Pass `OPENAI_BASE_URL` to the single shared OpenAI client constructor.
2. Require a non-empty `OPENAI_API_KEY` without assuming every OpenAI-compatible provider uses an `sk-` prefix.

The session-recovery patch closes a long-running availability gap in the pinned
Brain release. Root SurrealDB connections already re-authenticate after a
websocket reconnect, but scoped `brain_caller` connections did not. A SurrealDB
restart could therefore leave search connected anonymously until Brain itself
was restarted. The patch caches only the in-process scoped credentials,
performs a bounded signin whenever a scoped connection is acquired, rebuilds a
dead connection, and restores its namespace/database selection. First-boot
migration remains root-authenticated before the scoped session is required.

No Brain MCP route is published by the Compose stack. Brain has no host port and is reachable only by Manifold on the internal network. Manifold itself implements no MCP endpoint.

Verify the patch against a clean upstream checkout:

```bash
git clone --branch v0.8.1 --depth 1 https://github.com/inite-ai/inite-brain-service.git /tmp/brain
git -C /tmp/brain apply --check "$PWD/deploy/brain/openai-base-url.patch"
git -C /tmp/brain apply "$PWD/deploy/brain/openai-base-url.patch"
git -C /tmp/brain apply --check "$PWD/deploy/brain/scoped-session-recovery.patch"
```
