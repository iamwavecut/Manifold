# INITE Brain patch

Manifold builds INITE Brain from the exact `v0.8.1` tag (`557babacfad9ffd856107a49d0cfc20d27016afe`) and applies [`deploy/brain/openai-base-url.patch`](../deploy/brain/openai-base-url.patch).

The patch makes two deliberately small changes:

1. Pass `OPENAI_BASE_URL` to the single shared OpenAI client constructor.
2. Require a non-empty `OPENAI_API_KEY` without assuming every OpenAI-compatible provider uses an `sk-` prefix.

No Brain MCP route is published by the Compose stack. Brain has no host port and is reachable only by Manifold on the internal network. Manifold itself implements no MCP endpoint.

Verify the patch against a clean upstream checkout:

```bash
git clone --branch v0.8.1 --depth 1 https://github.com/inite-ai/inite-brain-service.git /tmp/brain
git -C /tmp/brain apply --check "$PWD/deploy/brain/openai-base-url.patch"
```
