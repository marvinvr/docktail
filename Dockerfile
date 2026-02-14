# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /build

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags '-w -s' -o docktail .

# Tailscale binary stage — ensures CLI version matches the sidecar daemon exactly
FROM tailscale/tailscale:latest AS tailscale

# Runtime stage
FROM alpine:latest

RUN apk add --no-cache ca-certificates iptables ip6tables

# Copy tailscale CLI from official image to guarantee version consistency with sidecar
COPY --from=tailscale /usr/local/bin/tailscale /usr/local/bin/tailscale

WORKDIR /app

# Copy binary from build stage
COPY --from=builder /build/docktail .

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD tailscale --socket=${TAILSCALE_SOCKET:-/var/run/tailscale/tailscaled.sock} serve status || exit 1

ENTRYPOINT ["/bin/sh", "-c", "sleep 1 && exec /app/docktail"]
