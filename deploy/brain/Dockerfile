# syntax=docker/dockerfile:1.7

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS source
ARG BRAIN_REF=0f3d16db0d175794b79b73ef186351aa9f2da3c0
RUN apk add --no-cache git \
    && git clone --depth 1 --branch v2.3.0 https://github.com/inite-ai/inite-brain-service.git /source \
    && test "$(git -C /source rev-parse HEAD)" = "${BRAIN_REF}"
COPY deploy/brain/openai-base-url.patch /tmp/openai-base-url.patch
COPY deploy/brain/document-origin-identity.patch /tmp/document-origin-identity.patch
COPY deploy/brain/worker-startup-order.patch /tmp/worker-startup-order.patch
COPY deploy/brain/corroborated-commit-ref.patch /tmp/corroborated-commit-ref.patch
COPY deploy/brain/failed-run-retry.patch /tmp/failed-run-retry.patch
COPY deploy/brain/extraction-completion.patch /tmp/extraction-completion.patch
COPY deploy/brain/fatal-signal-default.patch /tmp/fatal-signal-default.patch
COPY deploy/brain/openai-chat-extra-body.patch /tmp/openai-chat-extra-body.patch
RUN cd /source \
    && git apply --check /tmp/openai-base-url.patch \
    && git apply /tmp/openai-base-url.patch \
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
    && git apply /tmp/openai-chat-extra-body.patch

# Node 22 LTS (22.23.3); upstream v2.3.0 builds on the same major line.
FROM node:22.23.3-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c AS builder
WORKDIR /app
COPY --from=source /source/package.json /source/pnpm-lock.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile
COPY --from=source /source/tsconfig.json /source/nest-cli.json ./
COPY --from=source /source/src ./src
RUN pnpm build

FROM node:22.23.3-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c
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
