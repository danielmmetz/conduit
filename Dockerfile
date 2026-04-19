# syntax=docker/dockerfile:1
# Multi-stage build: generates WASM assets, then compiles a static conduit-server.
# Runtime is scratch + only the CA bundle (for HTTPS/WSS) and the binary.
ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ENV CGO_ENABLED=0
RUN go generate ./cmd/conduit-server
RUN go build -trimpath -ldflags="-s -w" -o /out/conduit-server ./cmd/conduit-server

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --chmod=0555 --chown=65532:65532 --from=build /out/conduit-server /conduit-server

# Distroless-style UID (no /etc/passwd in scratch; numeric USER is fine).
USER 65532:65532

EXPOSE 8080/tcp
# Embedded TURN defaults; non-root cannot bind :3478 without extra caps — use high ports
# (e.g. -turn-listen-udp :5349 -turn-listen-tcp :5349) or --cap-add=NET_BIND_SERVICE.
EXPOSE 3478/tcp 3478/udp

ENTRYPOINT ["/conduit-server"]
