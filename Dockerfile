# syntax=docker/dockerfile:1.7

FROM node:26.13.1-alpine3.24 AS web
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY tsconfig.json ./
COPY web ./web
RUN npm run check && npm run build

FROM golang:1.27.4-alpine3.24 AS build
ARG VERSION
ARG COMMIT=unknown
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist/app.js /src/web/dist/app.js
RUN CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH}" \
    version="${VERSION:-$(tr -d '[:space:]' < VERSION)}" \
    && go build -trimpath -ldflags="-s -w -X main.version=${version} -X main.commit=${COMMIT}" -o /out/manifold ./cmd/manifold \
    && mkdir -p /out/data

# Static binary on a shell-less base. UID 100 and GID 101 match the earlier
# Alpine image, so existing data volumes stay writable.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/manifold /usr/local/bin/manifold
COPY --from=build --chown=100:101 /out/data /var/lib/manifold
USER 100:101
ENV MANIFOLD_DB_PATH=/var/lib/manifold/manifold.db
EXPOSE 8080
VOLUME ["/var/lib/manifold"]
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=5 \
    CMD ["/usr/local/bin/manifold", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/manifold"]
