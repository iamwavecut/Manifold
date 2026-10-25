# syntax=docker/dockerfile:1.7

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS source
# Upstream main after v2.3.0 (39 commits, the last on 2026-09-26): the Brain
# repository has published no release since v2.3.0.
ARG BRAIN_REF=a567f6960c031b916549699fec7b89423c650d1e
RUN apk add --no-cache git \
    && git init -q /source \
    && git -C /source remote add origin https://github.com/inite-ai/inite-brain-service.git \
    && git -C /source fetch -q --depth 1 origin "${BRAIN_REF}" \
    && git -C /source checkout -q FETCH_HEAD \
    && test "$(git -C /source rev-parse HEAD)" = "${BRAIN_REF}"
COPY deploy/brain/openai-api-key-format.patch /tmp/openai-api-key-format.patch
COPY deploy/brain/document-origin-identity.patch /tmp/document-origin-identity.patch
COPY deploy/brain/worker-startup-order.patch /tmp/worker-startup-order.patch
COPY deploy/brain/corroborated-commit-ref.patch /tmp/corroborated-commit-ref.patch
COPY deploy/brain/failed-run-retry.patch /tmp/failed-run-retry.patch
COPY deploy/brain/extraction-completion.patch /tmp/extraction-completion.patch
COPY deploy/brain/fatal-signal-default.patch /tmp/fatal-signal-default.patch
COPY deploy/brain/openai-chat-extra-body.patch /tmp/openai-chat-extra-body.patch
COPY deploy/brain/entity-fact-read-index-hints.patch /tmp/entity-fact-read-index-hints.patch
COPY deploy/brain/resolve-fact-entity-index.patch /tmp/resolve-fact-entity-index.patch
COPY deploy/brain/selective-entity-fact-indexes.patch /tmp/selective-entity-fact-indexes.patch
COPY deploy/brain/derived-stale-cascade.patch /tmp/derived-stale-cascade.patch
COPY deploy/brain/edge-retraction.patch /tmp/edge-retraction.patch
COPY deploy/brain/revive-superseded-index.patch /tmp/revive-superseded-index.patch
COPY deploy/brain/knn-empty-result-fallback.patch /tmp/knn-empty-result-fallback.patch
COPY deploy/brain/hnsw-provision-early-tenants.patch /tmp/hnsw-provision-early-tenants.patch
COPY deploy/brain/search-throttle-limit.patch /tmp/search-throttle-limit.patch
RUN cd /source \
    && git apply --check /tmp/openai-api-key-format.patch \
    && git apply /tmp/openai-api-key-format.patch \
    && git apply --check /tmp/document-origin-identity.patch \
    && git apply /tmp/document-origin-identity.patch \
    && git apply --check /tmp/worker-startup-order.patch \
    && git apply /tmp/worker-startup-order.patch \
    && git apply --check /tmp/corroborated-commit-ref.patch \
    && git apply /tmp/corroborated-commit-ref.patch \
    && git apply --check /tmp/failed-run-retry.patch \
    && git apply /tmp/failed-run-retry.patch \
    && git apply --check /tmp/extraction-completion.patch \
    && git apply /tmp/extraction-completion.patch \
    && git apply --check /tmp/fatal-signal-default.patch \
    && git apply /tmp/fatal-signal-default.patch \
    && git apply --check /tmp/openai-chat-extra-body.patch \
    && git apply /tmp/openai-chat-extra-body.patch \
    && git apply --check /tmp/entity-fact-read-index-hints.patch \
    && git apply /tmp/entity-fact-read-index-hints.patch \
    && git apply --check /tmp/resolve-fact-entity-index.patch \
    && git apply /tmp/resolve-fact-entity-index.patch \
    && git apply --check /tmp/selective-entity-fact-indexes.patch \
    && git apply /tmp/selective-entity-fact-indexes.patch \
    && git apply --check /tmp/derived-stale-cascade.patch \
    && git apply /tmp/derived-stale-cascade.patch \
    && git apply --check /tmp/edge-retraction.patch \
    && git apply /tmp/edge-retraction.patch \
    && git apply --check /tmp/revive-superseded-index.patch \
    && git apply /tmp/revive-superseded-index.patch \
    && git apply --check /tmp/knn-empty-result-fallback.patch \
    && git apply /tmp/knn-empty-result-fallback.patch \
    && git apply --check /tmp/hnsw-provision-early-tenants.patch \
    && git apply /tmp/hnsw-provision-early-tenants.patch \
    && git apply --check /tmp/search-throttle-limit.patch \
    && git apply /tmp/search-throttle-limit.patch

# Node 22 LTS (22.23.3); upstream main builds on the same major line.
FROM node:22.23.3-slim@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392 AS builder
WORKDIR /app
COPY --from=source /source/package.json /source/pnpm-lock.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile
COPY --from=source /source/tsconfig.json /source/nest-cli.json ./
COPY --from=source /source/src ./src
RUN pnpm build

FROM node:22.23.3-slim@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392
RUN apt-get update \
    && apt-get install -y --no-install-recommends wget \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=source /source/package.json /source/pnpm-lock.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile --prod
COPY --from=builder /app/dist ./dist
COPY --chmod=0755 deploy/brain/healthcheck.sh /usr/local/bin/brain-healthcheck
RUN mkdir -p /app/.cache && chown node:node /app/.cache
ENV NODE_ENV=production \
    TRANSFORMERS_CACHE=/app/.cache \
    HF_HOME=/app/.cache \
    XDG_CACHE_HOME=/app/.cache
EXPOSE 3000
USER node
CMD ["node", "dist/main.js"]
