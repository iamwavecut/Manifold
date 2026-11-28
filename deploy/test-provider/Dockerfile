# syntax=docker/dockerfile:1.7

FROM golang:1.27.4-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY test/integration/provider ./test/integration/provider
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/provider ./test/integration/provider

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/provider /usr/local/bin/provider
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/provider"]
