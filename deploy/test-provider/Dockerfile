# syntax=docker/dockerfile:1.7

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY test/integration/provider ./test/integration/provider
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/provider ./test/integration/provider

FROM alpine:3.22
RUN adduser -S -D provider
COPY --from=build /out/provider /usr/local/bin/provider
USER provider
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/provider"]
