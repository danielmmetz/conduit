# syntax=docker/dockerfile:1
# Multi-stage build: compiles WASM and the server in parallel, then assembles
# a scratch runtime image with just the CA bundle and the static binary.
ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-bookworm AS base
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

FROM base AS wasm
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" \
        -o /out/main.wasm ./cmd/conduit-wasm \
 && cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" /out/wasm_exec.js

FROM base AS server
COPY . .
COPY --from=wasm /out/main.wasm     ./cmd/conduit-server/web/main.wasm
COPY --from=wasm /out/wasm_exec.js  ./cmd/conduit-server/web/wasm_exec.js
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/conduit-server ./cmd/conduit-server

FROM scratch
COPY --from=server /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --chmod=0555 --chown=65532:65532 --from=server /out/conduit-server /conduit-server

# Distroless-style UID (no /etc/passwd in scratch; numeric USER is fine).
USER 65532:65532

EXPOSE 8080/tcp
# Embedded TURN defaults; non-root cannot bind :3478 without extra caps — use high ports
# (e.g. -turn-listen-udp :5349 -turn-listen-tcp :5349) or --cap-add=NET_BIND_SERVICE.
EXPOSE 3478/tcp 3478/udp

ENTRYPOINT ["/conduit-server"]
