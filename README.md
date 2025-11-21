# 🍸 DockTail

**Unleash your Containers as Tailscale Services**

<p align="center">
  <img src="assets/header.jpeg" alt="DockTail Header" width="100%">
</p>

Automatically expose Docker containers as Tailscale Services using label-based configuration - zero-config service mesh for your dockerized services.

```
 ┌────────────────────────────────────────────────────────┐
 │                     Docker Host                        │
 │                                                        │
 │  ┌──────────────────┐         ┌──────────────────┐     │
 │  │     DockTail     │────────▶│ Tailscale Daemon │     │
 │  │   (Container)    │  CLI    │   (Host Process) │     │
 │  └────────┬─────────┘         └────────┬─────────┘     │
 │           │                            │               │
 │           │ Docker Socket              │ Proxies to    │
 │           │ Monitoring                 │ localhost     │
 │           ▼                            ▼               │
 │  ┌──────────────────┐         ┌──────────────────┐     │
 │  │   App Container  │◀────────│  localhost:9080  │     │
 │  │   Port 80        │  Mapped │  localhost:9081  │     │
 │  │  ports: 9080:80  │◀────────│                  │     │
 │  └──────────────────┘         └──────────────────┘     │
 │                                                        │
 └────────────────────────────────────────────────────────┘
                          │
                          │ Tailscale Network
                          ▼
               ┌─────────────────────┐
               │  Tailnet Clients    │
               │  Access services:   │
               │  web.tailnet.ts.net │
               └─────────────────────┘
```

## Features

- [x] Automatically discover and advertise Docker containers as Tailscale Services
- [x] HTTP, HTTPS and TCP protocols for running services
- [x] Support Tailscale HTTPS (auto TLS certificate)
- [x] Automatically drain Tailscale service configurations on container stop
- [x] Runs entirely in a **stateless Docker container**
- [ ] Add Tailscale Funnel Support
- [ ] More? => Create an Issue :)

> [!WARNING]
> This project is still being developed and it is **not** yet recommended to use for mission critical services.

## Quick Start

### Admin Console Setup

Before installing the DockTail, configure your Tailscale admin console at https://login.tailscale.com/admin/services:

1. **Create service definitions** (Services → Add service):
   - Create a service for each application you want to expose
   - Example: Service name `web`, `api`, `db`, etc.
   - Note: DockTail will automatically configure and advertise these services

2. **(Optional) Configure service tags**:
   - Navigate to Access Controls
   - Add tags for service identification (e.g., `tag:homelab-service`)
   - Tag your Docker host (e.g., `tag:homelab`)

3. **(Recommended) Enable auto-approval**:
   - Navigate to Access Controls and edit your ACL policy
   - Add auto-approvers to skip manual approval for service advertisements:
   ```json
   {
     "autoApprovers": {
       "services": {
         "tag:homelab-service": ["tag:homelab"]
       }
     }
   }
   ```
   - This allows devices tagged `tag:homelab` to automatically advertise services tagged `tag:homelab-service`

See [Tailscale Services documentation](https://tailscale.com/kb/1552/tailscale-services) for detailed setup instructions.

### Installation

#### Option 1: Docker Compose

Create a `docker-compose.yaml`:

```yaml
version: '3.8'

services:
  docktail:
    image: ghcr.io/marvinvr/docktail:latest
    container_name: docktail
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /var/run/tailscale/tailscaled.sock:/var/run/tailscale/tailscaled.sock
```

#### Option 2: Docker Run

```bash
docker run -d \
  --name docktail \
  --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /var/run/tailscale/tailscaled.sock:/var/run/tailscale/tailscaled.sock \
  ghcr.io/marvinvr/docktail:latest
```

### Usage

**🚨 CRITICAL:** Container ports MUST be published to host. Tailscale serve only supports `localhost` proxies.

**Basic example:**
```yaml
services:
  myapp:
    image: nginx:latest
    ports:
      - "8080:80"  # REQUIRED! HOST:CONTAINER format
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=myapp"
      - "docktail.service.port=80"  # CONTAINER port (RIGHT side of "8080:80")
```

Access from any device in your tailnet:
```bash
curl http://myapp.your-tailnet.ts.net
```

**With HTTPS (auto TLS cert from Tailscale):**
```yaml
services:
  myapp:
    image: nginx:latest
    ports:
      - "8080:80"
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=myapp"
      - "docktail.service.port=80"                   # Container port
      - "docktail.service.protocol=http"             # Container speaks HTTP
      - "docktail.service.service-port=443"          # Tailscale listens on 443
      - "docktail.service.service-protocol=https"    # Tailscale serves HTTPS (auto TLS!)
```

**Smart defaults (minimal config):**
```yaml
services:
  myapp:
    image: nginx:latest
    ports:
      - "8080:80"
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=myapp"
      - "docktail.service.port=80"
      - "docktail.service.service-port=443"  # Port 443 → auto-defaults to HTTPS!
      # service-protocol auto-defaults to "https" (based on port 443)
      # protocol auto-defaults to "http" (TLS termination at Tailscale)
```

**Port Mapping Rules:**
- `ports:` = `"HOST:CONTAINER"` (e.g., `"8080:80"` = host 8080 → container 80)
- `docktail.service.port` = CONTAINER port (always the RIGHT side)
- Result: Tailscale → localhost:8080 → Container:80

### Available Labels

| Label | Required | Default | Description |
|-------|----------|---------|-------------|
| `docktail.service.enable` | Yes | - | Enable DockTail for container |
| `docktail.service.name` | Yes | - | Service name (e.g., `web`, `api-v2`) |
| `docktail.service.port` | Yes | - | **CONTAINER** port (RIGHT side of `ports:`) |
| `docktail.service.protocol` | No | Smart*** | Protocol container speaks: `http`, `https`, `tcp`, `tls-terminated-tcp` |
| `docktail.service.service-port` | No | Smart* | Port Tailscale listens on |
| `docktail.service.service-protocol` | No | Smart** | Protocol Tailscale uses: `http`, `https`, `tcp` |

**Smart Defaults:**
- *`service-port`: Defaults to `80`, OR `443` if `service-protocol=https`
- **`service-protocol`: Defaults to `https` if `service-port=443`, otherwise `http`
- ***`protocol`: Defaults to `https` if container `port=443`, otherwise `http`

**Critical:** If `ports: "9080:80"`, then `docktail.service.port=80` (container port, NOT 9080)

### Supported Protocols

- `http`: Layer 7 HTTP forwarding
- `https`: Layer 7 HTTPS forwarding (auto TLS cert)
- `tcp`: Layer 4 TCP forwarding
- `tls-terminated-tcp`: Layer 4 with TLS termination

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_LEVEL` | `info` | Logging level (debug, info, warn, error) |
| `RECONCILE_INTERVAL` | `60s` | State reconciliation interval |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon socket |
| `TAILSCALE_SOCKET` | `/var/run/tailscale/tailscaled.sock` | Tailscale daemon socket |

## How It Works

1. **Container Discovery**: Monitors Docker events API for container lifecycle events (start, stop, die, restart)
2. **Label Parsing**: Extracts Tailscale service configuration from container labels
3. **Port Detection**: Queries Docker API for published host ports
4. **Configuration Generation**: Creates Tailscale service configuration proxying to `localhost:HOST_PORT`
5. **Configuration Application**: Executes Tailscale CLI commands to apply config and advertise services
6. **Stateless Operation**: Periodically reconciles state by querying Docker and Tailscale APIs


## Examples

### Web Application (Simple)

```yaml
services:
  nginx:
    image: nginx:latest
    ports:
      - "8080:80"
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=web"
      - "docktail.service.port=80"
```

### Database (Custom Port & Protocol)

```yaml
services:
  postgres:
    image: postgres:16
    ports:
      - "5432:5432"
    environment:
      POSTGRES_PASSWORD: secret
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=db"
      - "docktail.service.port=5432"
      - "docktail.service.protocol=tcp"
      - "docktail.service.service-port=5432"
```

### API with HTTPS (Auto TLS Certificate)

```yaml
services:
  api:
    image: myapi:latest
    ports:
      - "8080:3000"
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=api"
      - "docktail.service.port=3000"            # Container listens on 3000
      - "docktail.service.service-port=443"     # Tailscale listens on 443 (HTTPS)
      # service-protocol auto-defaults to "https" (based on service-port=443)
      # protocol auto-defaults to "http" (based on container port=3000, not 443)
```

Access with automatic TLS:
```bash
curl https://api.your-tailnet.ts.net  # TLS cert auto-provisioned!
```

## Building from Source

```bash
# Build binary
go build -o docktail .

# Build Docker image
docker build -t docktail:latest .

# Run locally
./docktail
```

## Links

- [Tailscale Services Documentation](https://tailscale.com/kb/1552/tailscale-services)
- [Tailscale Service Configuration Reference](https://tailscale.com/kb/1589/tailscale-services-configuration-file)
- [Docker SDK for Go](https://docs.docker.com/engine/api/sdk/)

## License

AGPL v3

----
By [@marvinvr](https://marvinvr.ch)