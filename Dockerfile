# Build stage. Runs on the build machine's own platform and cross-compiles for
# the target (CGO is off), so a multi-arch build only emulates the small
# runtime stage instead of the whole Go toolchain.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

WORKDIR /build

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application. TARGETVARIANT is "v7" for linux/arm/v7; GOARM wants
# the bare number.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
    go build -a -installsuffix cgo \
    -ldflags "-w -s -X github.com/marvinvr/docktail/version.Version=${VERSION}" \
    -o docktail .

# Tailscale binary stage — ensures CLI version matches the sidecar daemon exactly
FROM tailscale/tailscale:latest AS tailscale

# Runtime stage. DockTail runs no tailscaled of its own; it only drives the
# host's or sidecar's daemon through the tailscale CLI over the mounted socket,
# so no packet-filtering tools are needed here.
FROM alpine:latest

RUN apk add --no-cache ca-certificates

# Copy tailscale CLI from official image to guarantee version consistency with sidecar
COPY --from=tailscale /usr/local/bin/tailscale /usr/local/bin/tailscale

WORKDIR /app

# Copy binary from build stage
COPY --from=builder /build/docktail .

# Reads the status file the running process keeps current: healthy while the
# reconcile loop keeps succeeding (see docs/07-reference.md#health-check).
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/app/docktail", "health"]

# Exec form: DockTail is PID 1 and receives SIGTERM directly. It waits for a
# tailscaled that is still starting on its own (see main.go socketStartupWait).
ENTRYPOINT ["/app/docktail"]
