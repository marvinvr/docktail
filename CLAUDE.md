# DockTail

**Unleash your Containers as Tailscale Services**

Automatically expose Docker containers as Tailscale Services using label-based configuration - zero-config service mesh for your dockerized services.

## Quick Start - Port Publishing Requirements

**🚨 CRITICAL:** Container ports MUST be published to the host. Tailscale serve only supports `localhost` proxies.

```yaml
services:
  myapp:
    image: nginx
    ports:
      - "9080:80"  # ← REQUIRED! Format is HOST:CONTAINER
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=myapp"
      - "docktail.service.port=80"           # ← CONTAINER port (RIGHT side of "9080:80")
      - "docktail.service.service-port=443"  # Optional: Port on Tailscale (default: 80)
      - "docktail.service.protocol=https"    # Optional: Protocol (default: http)
```

**Port Label Guide:**
- **`ports:`** = `"HOST:CONTAINER"` (Docker format: `"9080:80"` means host 9080 → container 80)
- **`docktail.service.port`** = CONTAINER port (always the RIGHT side of the mapping)
- **Result:** Tailscale → `localhost:9080` → Container:80

## What is Tailscale Services?

[Tailscale Services](https://tailscale.com/kb/1552/tailscale-services) (currently in beta) decouples internal resources from the devices hosting them by creating named services with stable DNS names. Instead of connecting to specific devices, users connect to services using MagicDNS names while Tailscale automatically routes traffic to available hosts.

**Key benefits:**
- **Stable addressing**: `http://myapp.tailnet-name.ts.net` stays the same even when moving between hosts
- **High availability**: Multiple hosts can advertise the same service
- **Access control**: Leverage Tailscale's ACLs and identity-based authentication
- **No public exposure**: Services only accessible within your tailnet

**Documentation:**
- [Tailscale Services Overview](https://tailscale.com/kb/1552/tailscale-services)
- [Service Configuration File Syntax](https://tailscale.com/kb/1589/tailscale-services-configuration-file)

## The Problem

Manually configuring Tailscale Services for Docker containers requires:
1. Creating service definitions in the admin console
2. Writing JSON configuration files
3. Running `tailscale serve set-config` commands
4. Running `tailscale serve advertise` commands
5. Cleaning up when containers stop
6. Updating configs when containers restart with new IPs

This is tedious, error-prone, and doesn't scale well when managing many services.

## The Solution

**DockTail** watches your Docker daemon and automatically manages Tailscale Services based on container labels - similar to how Traefik discovers services.

**Just add labels to your containers and publish their ports:**
```yaml
services:
  nginx:
    image: nginx:latest
    ports:
      - "8080:80"  # REQUIRED: HOST:CONTAINER format - Publish container port to host
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=web"
      - "docktail.service.port=80"           # CONTAINER port (RIGHT side of "8080:80")
      - "docktail.service.service-port=443"  # Port to expose on Tailscale (default: 80)
      - "docktail.service.protocol=http"     # Protocol (default: http)
```

**IMPORTANT PORT MAPPING RULES:**
- **ports:** `"HOST:CONTAINER"` - Docker port format (e.g., `"9080:80"` maps host 9080 → container 80)
- **docktail.service.port:** Always the **CONTAINER** port (the RIGHT side of the ports mapping)
- DockTail automatically detects which HOST port it's published to

**DockTail handles the rest:**
- Detects container starts/stops via Docker events
- Detects published host ports
- Generates Tailscale service configurations (proxying to localhost)
- Applies configs and advertises services via Tailscale CLI
- Gracefully drains and cleans up on container stop
- Updates configs when containers restart or ports change

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                     Docker Host                          │
│                                                          │
│  ┌──────────────────┐         ┌──────────────────┐     │
│  │     DockTail     │────────▶│ Tailscale Daemon │     │
│  │   (Container)    │  CLI    │   (Host Process) │     │
│  └────────┬─────────┘         └──────────────────┘     │
│           │                                              │
│           │ Docker Socket                                │
│           │ Monitoring                                   │
│           ▼                                              │
│  ┌──────────────────┐         ┌──────────────────┐     │
│  │   App Container  │         │  App Container   │     │
│  │  172.17.0.5:80   │         │  172.17.0.6:3000 │     │
│  │  labels: {...}   │         │  labels: {...}   │     │
│  └──────────────────┘         └──────────────────┘     │
│                                                          │
└─────────────────────────────────────────────────────────┘
                         │
                         │ Tailscale Network
                         ▼
              ┌────────────────────┐
              │  Tailnet Clients   │
              │  Access services:  │
              │  web.tailnet.ts.net│
              └────────────────────┘
```

## How It Works

### 1. Container Discovery
Monitors Docker events API for container lifecycle events (start, stop, die, restart).

### 2. Label Parsing
Extracts Tailscale service configuration from container labels:
- `docktail.service.enable`: Enable DockTail for this container
- `docktail.service.name`: Service name (becomes `svc:name`)
- `docktail.service.port`: Internal container port
- `docktail.service.service-port`: External port to expose on Tailscale (default: 80)
- `docktail.service.protocol`: Protocol (default: http) - options: `http`, `https`, `tcp`, `tls-terminated-tcp`

### 3. Port Binding Detection
Queries Docker API for container's published port mappings to the host.

**Important:** Tailscale serve only supports proxying to `localhost` or `127.0.0.1`. Container ports MUST be published to the host.

### 4. Configuration Generation
Creates Tailscale service configuration JSON proxying to localhost:
```json
{
  "version": "0.0.1",
  "services": {
    "svc:web": {
      "endpoints": {
        "tcp:443": "http://localhost:8080"
      }
    }
  }
}
```

### 5. Configuration Application
Executes Tailscale CLI commands:
```bash
tailscale serve set-config --all /tmp/config.json
tailscale serve advertise svc:web
```

### 6. Stateless Operation
On startup and at regular intervals, DockTail:
1. Queries Docker for all running containers with `docktail.service.enable=true`
2. Reads current Tailscale service configuration via `tailscale serve get-config --all`
3. Reconciles differences and applies necessary changes
4. No persistent state storage required - truth comes from Docker API

## Prerequisites

**On the Docker host:**
1. Tailscale installed and authenticated
2. Host must have a tag-based identity (not user-based)
3. Docker daemon running with API socket exposed
4. (Recommended) Auto-approval ACL policies configured

**Auto-approval example** (`/admin/acls`):
```json
{
  "autoApprovers": {
    "services": {
      "tag:homelab-service": ["tag:homelab"]
    }
  }
}
```

This allows devices tagged `tag:homelab` to automatically advertise services tagged `tag:homelab-service`.

## Usage

### Docker Compose Deployment

```yaml
version: '3.8'

services:
  docktail:
    image: ghcr.io/yourusername/docktail:latest
    container_name: docktail
    restart: unless-stopped
    volumes:
      # Docker socket for container monitoring
      - /var/run/docker.sock:/var/run/docker.sock:ro
      # Tailscale socket for CLI communication
      - /var/run/tailscale/tailscaled.sock:/var/run/tailscale/tailscaled.sock
    environment:
      - LOG_LEVEL=info
      - RECONCILE_INTERVAL=60s
    # Optional: network mode host to avoid networking issues
    # network_mode: host

  # Example application
  nginx:
    image: nginx:latest
    ports:
      - "8080:80"  # HOST:CONTAINER - Publish container port 80 to host port 8080
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=web"
      - "docktail.service.port=80"           # CONTAINER port (RIGHT side: "8080:80")
      - "docktail.service.service-port=443"  # Port on Tailscale (default: 80)
      - "docktail.service.protocol=https"    # Protocol (default: http)

  # Another example - database
  postgres:
    image: postgres:16
    ports:
      - "5432:5432"  # HOST:CONTAINER - Same port on both sides
    environment:
      POSTGRES_PASSWORD: secret
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=db"
      - "docktail.service.port=5432"          # CONTAINER port (RIGHT side: "5432:5432")
      - "docktail.service.service-port=5432"  # Port on Tailscale (default: 80)
      - "docktail.service.protocol=tcp"       # Protocol (default: http)
```

### Running the Containers

```bash
# Start everything
docker compose up -d

# Check DockTail logs
docker logs -f docktail

# Verify services are advertised
tailscale serve status --json

# Access from any device in your tailnet
curl https://web.your-tailnet.ts.net
psql postgresql://user@db.your-tailnet.ts.net:5432/database
```

### Label Reference

| Label | Required | Default | Description | Example |
|-------|----------|---------|-------------|---------|
| `docktail.service.enable` | Yes | - | Enable DockTail for container | `true` |
| `docktail.service.name` | Yes | - | Service name (alphanumeric, hyphens) | `web`, `api-v2` |
| `docktail.service.port` | Yes | - | **CONTAINER** port (RIGHT side of `ports:`) | `80`, `3000` |
| `docktail.service.service-port` | No | `80` | Port exposed on Tailscale | `443`, `8080` |
| `docktail.service.protocol` | No | `http` | Protocol type | `http`, `https`, `tcp` |

**CRITICAL REQUIREMENTS:**
1. Container port in `docktail.service.port` **MUST** be published via `ports:` in docker-compose.yaml
2. `docktail.service.port` is ALWAYS the **CONTAINER** port (RIGHT side of `"HOST:CONTAINER"`)
3. Example: If `ports: "9080:80"`, then `docktail.service.port=80` (not 9080)
4. DockTail auto-detects the HOST port and proxies `localhost:9080` → Tailscale

**Supported protocols:**
- `http`: Layer 7 HTTP forwarding
- `https`: Layer 7 HTTPS forwarding (auto TLS cert)
- `tcp`: Layer 4 TCP forwarding
- `tls-terminated-tcp`: Layer 4 with TLS termination

## Implementation Details

### Technology Stack
- **Language**: Go 1.21+
- **Docker SDK**: github.com/docker/docker/client
- **Logging**: zerolog or logrus
- **State Management**: Stateless - queries Docker API on-demand

### Key Components

**Event Listener**
```go
// Watches Docker events stream
eventsChan, errChan := dockerClient.Events(ctx, types.EventsOptions{
    Filters: filters.NewArgs(
        filters.Arg("type", "container"),
    ),
})
```

**Service Manager**
```go
// Handles service lifecycle (stateless)
type ServiceManager struct {
    dockerClient   *docker.Client
    tsConfig       *TailscaleConfig
}

// Reconciles by querying current state
func (sm *ServiceManager) Reconcile() error {
    // Query Docker for all containers with docktail.service.enable=true
    // Query Tailscale for current config
    // Apply differences
}
```

**Configuration Builder**
```go
// Generates Tailscale service JSON
func buildServiceConfig(containers []*Container) *TailscaleServiceConfig {
    config := &TailscaleServiceConfig{
        Version:  "0.0.1",
        Services: make(map[string]ServiceDefinition),
    }
    // ... build logic from container labels
}
```

### Error Handling

- **Docker daemon disconnects**: Reconnect with exponential backoff
- **Container IP not available**: Skip container, retry on next reconciliation
- **Tailscale CLI failures**: Log error, retry on next reconciliation
- **Invalid labels**: Log warning, skip container

### State Reconciliation

Periodic reconciliation loop (every 60s by default):
1. Query all running containers with `docktail.service.enable=true`
2. Query current Tailscale service configuration
3. Calculate diff (containers added/removed/changed)
4. Apply only necessary changes (drain → clear → configure → advertise)

On Docker events (start/stop/die/restart):
1. Trigger immediate reconciliation
2. No separate event-based state tracking needed

## Advanced Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_LEVEL` | `info` | Logging level (debug, info, warn, error) |
| `RECONCILE_INTERVAL` | `60s` | State reconciliation interval |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon socket |
| `TAILSCALE_SOCKET` | `/var/run/tailscale/tailscaled.sock` | Tailscale daemon socket |

### Port Publishing Examples

**Example 1: Different host and container ports**
```yaml
services:
  app:
    image: myapp:latest
    ports:
      - "8080:3000"  # HOST:CONTAINER - Host port 8080 → Container port 3000
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=app"
      - "docktail.service.port=3000"         # CONTAINER port (RIGHT side: "8080:3000")
      - "docktail.service.service-port=443"  # Tailscale exposes on port 443 (default: 80)
      - "docktail.service.protocol=https"    # Protocol (default: http)
# Result: Tailscale (443) → localhost:8080 → Container (3000)
```

**Example 2: Same port on host and container**
```yaml
services:
  db:
    image: postgres:16
    ports:
      - "5432:5432"  # HOST:CONTAINER - Same port 5432 on both sides
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=database"
      - "docktail.service.port=5432"          # CONTAINER port (RIGHT side: "5432:5432")
      - "docktail.service.service-port=5432"  # Tailscale exposes on port 5432 (default: 80)
      - "docktail.service.protocol=tcp"       # Protocol (default: http)
# Result: Tailscale (5432) → localhost:5432 → Container (5432)
```

### High Availability

Multiple containers can advertise the same service name:
```yaml
services:
  web-01:
    image: nginx
    labels:
      - "docktail.service.name=web"  # Same name

  web-02:
    image: nginx
    labels:
      - "docktail.service.name=web"  # Same name
```

Tailscale automatically load balances across available hosts.

## Limitations

- **Port publishing required**: Container ports MUST be published to the host (Tailscale serve only supports `localhost` proxies)
- **TCP only**: Tailscale Services currently only support TCP protocol
- **Tag-based identity**: Host machine must use Tailscale tags, not user authentication
- **No hairpinning**: Containers cannot access their own Tailscale service endpoint
- **Port conflicts**: Published ports must not conflict with other services on the host

## Links

- [Tailscale Services Documentation](https://tailscale.com/kb/1552/tailscale-services)
- [Tailscale Service Configuration Reference](https://tailscale.com/kb/1589/tailscale-services-configuration-file)
- [Tailscale ACL Documentation](https://tailscale.com/kb/1337/policy-syntax)
- [Docker SDK for Go](https://docs.docker.com/engine/api/sdk/)