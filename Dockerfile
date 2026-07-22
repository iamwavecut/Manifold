# syntax=docker/dockerfile:1.7

FROM node:26-alpine AS web
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY tsconfig.json ./
COPY web ./web
RUN npm run check && npm run build

FROM golang:1.26-alpine AS build
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
    && go build -trimpath -ldflags="-s -w -X main.version=${version} -X main.commit=${COMMIT}" -o /out/manifold ./cmd/manifold

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -S manifold \
    && adduser -S -G manifold -h /var/lib/manifold manifold \
    && mkdir -p /var/lib/manifold \
    && chown manifold:manifold /var/lib/manifold
COPY --from=build /out/manifold /usr/local/bin/manifold
USER manifold
EXPOSE 8080
VOLUME ["/var/lib/manifold"]
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=5 \
    CMD wget -qO- http://127.0.0.1:8080/health >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/manifold"]
