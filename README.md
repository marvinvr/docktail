# 🍸 DockTail

**Unleash your Containers as Tailscale Services**

<p align="center">
  <img src="assets/header.jpeg" alt="DockTail Header" width="100%">
</p>

Automatically expose Docker containers as Tailscale Services using label-based configuration - zero-config service mesh for your dockerized services.

## Features

- [x] Automatically discover and expose Docker containers as Tailscale Services
- [x] Auto-create service definitions via Tailscale API (with OAuth or API key)
- [x] HTTP, HTTPS and TCP protocols
- [x] Tailscale HTTPS with automatic TLS certificates
- [x] Tailscale Funnel support (public internet access)
- [x] Automatic cleanup when containers stop
- [x] Runs entirely in a **stateless Docker container**

## Quick Start

```yaml
services:
  docktail:
    image: ghcr.io/marvinvr/docktail:latest
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /var/run/tailscale/tailscaled.sock:/var/run/tailscale/tailscaled.sock
    environment:
      # Optional but recommended - enables auto-service-creation
      - TAILSCALE_OAUTH_CLIENT_ID=${TAILSCALE_OAUTH_CLIENT_ID}
      - TAILSCALE_OAUTH_CLIENT_SECRET=${TAILSCALE_OAUTH_CLIENT_SECRET}

  myapp:
    image: nginx:latest
    ports:
      - "8080:80"
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=myapp"
      - "docktail.service.port=80"
```

```bash
docker compose up -d
curl http://myapp.your-tailnet.ts.net
```

That's it! See [Configuration](#configuration) for OAuth setup (recommended) or running without credentials.

## Configuration

DockTail works in three modes. Choose based on your needs:

### Recommended: OAuth Credentials

OAuth lets DockTail **auto-create services** in your Tailscale Admin Console. No manual setup required.

**Setup:**

1. Go to [Tailscale Admin Console → Settings → OAuth clients](https://login.tailscale.com/admin/settings/oauth)
2. Create a new OAuth client with scope `all` and your service tags (e.g., `tag:container`)
3. Add to your DockTail environment:

```yaml
environment:
  - TAILSCALE_OAUTH_CLIENT_ID=your-client-id
  - TAILSCALE_OAUTH_CLIENT_SECRET=your-client-secret
```

**Benefits:**
- Services auto-created when containers start
- Never expires (unlike API keys)
- Proper tag-based ACL support

### Alternative: API Key

Also auto-creates services, but expires every 90 days.

**Setup:**

1. Go to [Tailscale Admin Console → Settings → Keys](https://login.tailscale.com/admin/settings/keys)
2. Generate an API key
3. Add to your DockTail environment:

```yaml
environment:
  - TAILSCALE_API_KEY=tskey-api-...
```

### No Credentials (Manual Mode)

DockTail works without any credentials for basic use. Services are advertised locally via the Tailscale CLI, but you must **manually create service definitions** in the Admin Console.

**Manual setup required:**

1. Go to [Tailscale Admin Console → Services](https://login.tailscale.com/admin/services)
2. Create a service for each container you want to expose
3. Configure ACL auto-approvers (see [ACL Configuration](#acl-configuration))

## Installation

### Tailscale on Host

For systems with Tailscale installed on the host:

```yaml
services:
  docktail:
    image: ghcr.io/marvinvr/docktail:latest
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /var/run/tailscale/tailscaled.sock:/var/run/tailscale/tailscaled.sock
    environment:
      # Optional but recommended - enables auto-service-creation
      - TAILSCALE_OAUTH_CLIENT_ID=${TAILSCALE_OAUTH_CLIENT_ID}
      - TAILSCALE_OAUTH_CLIENT_SECRET=${TAILSCALE_OAUTH_CLIENT_SECRET}
```

### Tailscale Sidecar

For systems without Tailscale installed on the host:

```yaml
services:
  tailscale:
    image: tailscale/tailscale:latest
    hostname: docktail-host
    environment:
      - TS_AUTHKEY=${TAILSCALE_AUTH_KEY}
      - TS_STATE_DIR=/var/lib/tailscale
      - TS_SOCKET=/var/run/tailscale/tailscaled.sock
    volumes:
      - tailscale-state:/var/lib/tailscale
      - tailscale-socket:/var/run/tailscale
      - /dev/net/tun:/dev/net/tun
    cap_add:
      - NET_ADMIN
      - SYS_MODULE
    network_mode: host
    restart: unless-stopped

  docktail:
    image: ghcr.io/marvinvr/docktail:latest
    depends_on:
      - tailscale
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - tailscale-socket:/var/run/tailscale
    environment:
      # Optional but recommended - enables auto-service-creation
      - TAILSCALE_OAUTH_CLIENT_ID=${TAILSCALE_OAUTH_CLIENT_ID}
      - TAILSCALE_OAUTH_CLIENT_SECRET=${TAILSCALE_OAUTH_CLIENT_SECRET}

volumes:
  tailscale-state:
  tailscale-socket:
```

Set `TAILSCALE_AUTH_KEY` to authenticate the Tailscale container (generate at [Tailscale Admin → Settings → Keys](https://login.tailscale.com/admin/settings/keys)).

## Labeling Containers

### Port Requirement

**Container ports MUST be published to host.** Tailscale serve proxies to `localhost`.

```yaml
ports:
  - "8080:80"  # HOST:CONTAINER - required!
```

**Exception:** Containers with `network_mode: host` don't need port publishing.

### Service Labels

| Label | Required | Default | Description |
|-------|----------|---------|-------------|
| `docktail.service.enable` | Yes | - | Enable DockTail for container |
| `docktail.service.name` | Yes | - | Service name (e.g., `web`, `api`) |
| `docktail.service.port` | Yes | - | Container port (right side of `ports:`) |
| `docktail.service.protocol` | No | Smart* | Container protocol: `http`, `https`, `https+insecure`, `tcp`, `tls-terminated-tcp` |
| `docktail.service.service-port` | No | Smart** | Port Tailscale listens on |
| `docktail.service.service-protocol` | No | Smart*** | Tailscale protocol: `http`, `https`, `tcp` |
| `docktail.tags` | No | `tag:container` | Comma-separated tags for ACLs |

**Smart Defaults:**
- \* `protocol`: `https` if container port is 443, otherwise `http`
- \** `service-port`: `443` if service-protocol is `https`, otherwise `80`
- \*** `service-protocol`: `https` if service-port is 443, matches `protocol` for TCP, otherwise `http`

### Funnel Labels (Public Internet Access)

Funnel exposes your service to the **public internet**. Independent from service labels.

| Label | Required | Default | Description |
|-------|----------|---------|-------------|
| `docktail.funnel.enable` | Yes | `false` | Enable Tailscale Funnel |
| `docktail.funnel.port` | Yes | - | Container port |
| `docktail.funnel.funnel-port` | No | `443` | Public port (443, 8443, or 10000) |
| `docktail.funnel.protocol` | No | `https` | Protocol: `https`, `tcp`, `tls-terminated-tcp` |

**Notes:**
- Only ONE funnel per port (Tailscale limitation)
- Uses machine hostname, not service name: `https://<machine>.<tailnet>.ts.net`

## Examples

### Web Application

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

### HTTPS with Auto TLS

```yaml
services:
  api:
    image: myapi:latest
    ports:
      - "8080:3000"
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=api"
      - "docktail.service.port=3000"
      - "docktail.service.service-port=443"  # Auto-enables HTTPS
```

Access: `https://api.your-tailnet.ts.net`

### Database (TCP)

```yaml
services:
  postgres:
    image: postgres:16
    ports:
      - "5432:5432"
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=db"
      - "docktail.service.port=5432"
      - "docktail.service.protocol=tcp"
      - "docktail.service.service-port=5432"
```

### Host Networking

```yaml
services:
  app:
    image: myapp:latest
    network_mode: host
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=app"
      - "docktail.service.port=3000"  # No port mapping needed
```

### Public Website with Funnel

```yaml
services:
  website:
    image: nginx:latest
    ports:
      - "8080:80"
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=website"
      - "docktail.service.port=80"
      - "docktail.service.service-port=443"
      - "docktail.funnel.enable=true"
      - "docktail.funnel.port=80"
```

Access:
- Tailnet: `https://website.your-tailnet.ts.net`
- Public: `https://your-machine.your-tailnet.ts.net`

## Reference

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `TAILSCALE_OAUTH_CLIENT_ID` | - | OAuth Client ID (optional, enables auto-service-creation) |
| `TAILSCALE_OAUTH_CLIENT_SECRET` | - | OAuth Client Secret (optional, enables auto-service-creation) |
| `TAILSCALE_API_KEY` | - | API Key (optional alternative to OAuth, expires 90 days) |
| `TAILSCALE_TAILNET` | `-` | Tailnet ID (defaults to key's tailnet) |
| `DEFAULT_SERVICE_TAGS` | `tag:container` | Default tags for services |
| `LOG_LEVEL` | `info` | Logging level (debug, info, warn, error) |
| `RECONCILE_INTERVAL` | `60s` | State reconciliation interval |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon socket |
| `TAILSCALE_SOCKET` | `/var/run/tailscale/tailscaled.sock` | Tailscale daemon socket |

If both OAuth and API key are set, OAuth takes precedence.

### Supported Protocols

**Tailscale-facing (service-protocol):**
- `http` - Layer 7 HTTP
- `https` - Layer 7 HTTPS (auto TLS)
- `tcp` - Layer 4 TCP
- `tls-terminated-tcp` - Layer 4 with TLS termination

**Container-facing (protocol):**
- `http` - HTTP backend
- `https` - HTTPS with valid certificate
- `https+insecure` - HTTPS with self-signed certificate
- `tcp` - TCP backend
- `tls-terminated-tcp` - TCP with TLS termination

### ACL Configuration

For manual mode or fine-grained access control, configure ACLs in your [Tailscale Access Controls](https://login.tailscale.com/admin/acls):

```json
{
  "autoApprovers": {
    "services": {
      "tag:container": ["tag:homelab"]
    }
  }
}
```

This allows machines tagged `tag:homelab` to advertise services tagged `tag:container`. Ensure your Docker host has the appropriate tag: `tailscale up --advertise-tags=tag:homelab`

## How It Works

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

1. **Container Discovery** - Monitors Docker events for container start/stop
2. **Label Parsing** - Extracts service configuration from container labels
3. **Port Detection** - Queries Docker API for published host ports
4. **Config Generation** - Creates Tailscale service config proxying to `localhost:HOST_PORT`
5. **Service Advertisement** - Executes Tailscale CLI to advertise services
6. **Control Plane Sync** - If OAuth/API key configured, creates service definitions via API
7. **Reconciliation** - Periodically syncs state between Docker and Tailscale

**Note:** DockTail does NOT delete service definitions from the API when containers stop (conservative deletion strategy).

## Building from Source

```bash
go build -o docktail .
docker build -t docktail:latest .
./docktail
```

## Links

- [Tailscale Services Documentation](https://tailscale.com/kb/1552/tailscale-services)
- [Tailscale Funnel Documentation](https://tailscale.com/kb/1311/tailscale-funnel)
- [Tailscale Service Configuration Reference](https://tailscale.com/kb/1589/tailscale-services-configuration-file)
- [Docker SDK for Go](https://docs.docker.com/engine/api/sdk/)

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=marvinvr/docktail&type=date&legend=top-left)](https://www.star-history.com/#marvinvr/docktail&type=date&legend=top-left)

## License

AGPL v3

----
By [@marvinvr](https://marvinvr.ch)
