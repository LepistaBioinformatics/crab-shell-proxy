# syntax=docker/dockerfile:1
# ---------------------------------------------------------------------------
# crab-shell-proxy — per-user picoclaw orchestrator (see
# ../.specs/features/crab-shell-proxy). Build with network access so Go modules
# resolve (this stack builds with `--network=host`, matching mycelium-gateway).
# ---------------------------------------------------------------------------

FROM golang:1.23-bookworm AS build
WORKDIR /src

# No committed go.sum (no host Go toolchain in this project); resolve + record
# module hashes at build time. -mod=mod lets go.sum be written as needed.
ENV GOFLAGS=-mod=mod
ENV CGO_ENABLED=0

COPY go.mod ./
COPY . .

# The build IS the test gate (tasks T02): vet + the full test suite must pass
# or the image does not get built.
RUN go mod tidy \
 && go vet ./... \
 && go test ./... \
 && go build -trimpath -o /out/crab-shell-proxy ./cmd/crab-shell-proxy

# ---------------------------------------------------------------------------
# Runtime — runs as ROOT (uid 0) on purpose: it must reach the Docker socket
# (root:docker 0660), read root-owned 0600 picoclaw template files, and write
# per-user data dirs that root-running picoclaw then reads. A nonroot/distroless
# image fails all three at runtime. debian-slim (not distroless) so a shell is
# available for debugging in this self-host stack.
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim AS runtime
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates wget \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/crab-shell-proxy /usr/local/bin/crab-shell-proxy
COPY config.yaml /etc/crab-shell-proxy/config.yaml
ENV CRAB_CONFIG=/etc/crab-shell-proxy/config.yaml
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/crab-shell-proxy"]
