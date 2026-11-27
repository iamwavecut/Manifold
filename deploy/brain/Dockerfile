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
COPY scripts/brain-apply-patches.sh /tmp/manifold/scripts/
COPY deploy/brain/series deploy/brain/*.patch /tmp/manifold/deploy/brain/
RUN /tmp/manifold/scripts/brain-apply-patches.sh /source

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
