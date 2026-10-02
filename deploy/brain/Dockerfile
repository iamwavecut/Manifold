# syntax=docker/dockerfile:1.7

FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS source
ARG BRAIN_REF=b19b209fec92069d91df22af61db18b0a810ce82
RUN apk add --no-cache git \
    && git clone --depth 1 --branch v2.2.0 https://github.com/inite-ai/inite-brain-service.git /source \
    && test "$(git -C /source rev-parse HEAD)" = "${BRAIN_REF}"
COPY deploy/brain/openai-base-url.patch /tmp/openai-base-url.patch
COPY deploy/brain/scoped-session-recovery.patch /tmp/scoped-session-recovery.patch
COPY deploy/brain/document-origin-identity.patch /tmp/document-origin-identity.patch
COPY deploy/brain/failed-run-retry.patch /tmp/failed-run-retry.patch
COPY deploy/brain/worker-startup-order.patch /tmp/worker-startup-order.patch
COPY deploy/brain/corroborated-commit-ref.patch /tmp/corroborated-commit-ref.patch
COPY deploy/brain/extraction-completion.patch /tmp/extraction-completion.patch
RUN cd /source \
    && git apply --check /tmp/openai-base-url.patch \
    && git apply /tmp/openai-base-url.patch \
    && git apply --check /tmp/scoped-session-recovery.patch \
    && git apply /tmp/scoped-session-recovery.patch \
    && git apply --check /tmp/document-origin-identity.patch \
    && git apply /tmp/document-origin-identity.patch \
    && git apply --check /tmp/failed-run-retry.patch \
    && git apply /tmp/failed-run-retry.patch \
    && git apply --check /tmp/worker-startup-order.patch \
    && git apply /tmp/worker-startup-order.patch \
    && git apply --check /tmp/corroborated-commit-ref.patch \
    && git apply /tmp/corroborated-commit-ref.patch \
    && git apply --check /tmp/extraction-completion.patch \
    && git apply /tmp/extraction-completion.patch

# These Node 22 digest pins match the v2.2.0 upstream Dockerfile's tested runtime.
FROM node:22-slim@sha256:813a7480f28fdadac1f7f5c824bcdad435b5bc1322a5968bbbdef8d058f9dff4 AS builder
WORKDIR /app
COPY --from=source /source/package.json /source/pnpm-lock.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile
COPY --from=source /source/tsconfig.json /source/nest-cli.json ./
COPY --from=source /source/src ./src
RUN pnpm build

FROM node:22-slim@sha256:813a7480f28fdadac1f7f5c824bcdad435b5bc1322a5968bbbdef8d058f9dff4
RUN apt-get update \
    && apt-get install -y --no-install-recommends wget \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=source /source/package.json /source/pnpm-lock.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile --prod
COPY --from=builder /app/dist ./dist
RUN mkdir -p /app/.cache && chown node:node /app/.cache
ENV NODE_ENV=production \
    TRANSFORMERS_CACHE=/app/.cache \
    HF_HOME=/app/.cache \
    XDG_CACHE_HOME=/app/.cache
EXPOSE 3000
USER node
CMD ["node", "dist/main.js"]
