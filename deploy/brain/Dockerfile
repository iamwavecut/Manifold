# syntax=docker/dockerfile:1.7

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS source
# Upstream release v2.11.0.
ARG BRAIN_REF=6ba986054a546c71c93c39e0e118e0f3dddc983f
RUN apk add --no-cache git \
    && git init -q /source \
    && git -C /source remote add origin https://github.com/inite-ai/inite-brain-service.git \
    && git -C /source fetch -q --depth 1 origin "${BRAIN_REF}" \
    && git -C /source checkout -q FETCH_HEAD \
    && test "$(git -C /source rev-parse HEAD)" = "${BRAIN_REF}"
COPY scripts/brain-apply-patches.sh /tmp/manifold/scripts/
COPY deploy/brain/series deploy/brain/*.patch /tmp/manifold/deploy/brain/
RUN /tmp/manifold/scripts/brain-apply-patches.sh /source

# Node 24, the image upstream v2.11.0 builds on.
FROM node:24-slim@sha256:55fb5f5a527ea2c5c4a95f963e48ef90aa38c7b346054fdc8ee9957a2b55d205 AS builder
WORKDIR /app
COPY --from=source /source/package.json /source/pnpm-lock.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile
COPY --from=source /source/tsconfig.json /source/nest-cli.json ./
COPY --from=source /source/src ./src
RUN pnpm build

FROM node:24-slim@sha256:55fb5f5a527ea2c5c4a95f963e48ef90aa38c7b346054fdc8ee9957a2b55d205
RUN apt-get update \
    && apt-get install -y --no-install-recommends wget \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=source /source/package.json /source/pnpm-lock.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile --prod
COPY --from=builder /app/dist ./dist
COPY --from=source /source/packs ./packs
# The flag defaults of the release Manifold ran before (scripts/brain-flags.py);
# the environment still wins over this file.
COPY deploy/brain/flags/defaults.env ./defaults.env
COPY --chmod=0755 deploy/brain/healthcheck.sh /usr/local/bin/brain-healthcheck
RUN mkdir -p /app/.cache && chown node:node /app/.cache
ENV NODE_ENV=production \
    TRANSFORMERS_CACHE=/app/.cache \
    HF_HOME=/app/.cache \
    XDG_CACHE_HOME=/app/.cache
EXPOSE 3000
USER node
CMD ["node", "--env-file=/app/defaults.env", "dist/main.js"]
