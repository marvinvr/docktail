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
- [x] Multiple services from a single container
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
      - /var/run/tailscale:/var/run/tailscale
    environment:
      # Optional but recommended - enables auto-service-creation
      - TAILSCALE_OAUTH_CLIENT_ID=${TAILSCALE_OAUTH_CLIENT_ID}
      - TAILSCALE_OAUTH_CLIENT_SECRET=${TAILSCALE_OAUTH_CLIENT_SECRET}

  myapp:
    image: nginx:latest
    # No ports needed! DockTail proxies directly to container IP
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
2. Create a new OAuth client with the following permissions, scoped to your server tag (`tag:server` in most examples):
   - **General → Services**: Write
   - **Devices → Core**: Write
   - **Keys -> Auth Keys**: Write (When using the 'sidecar' method)
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
      - /var/run/tailscale:/var/run/tailscale
    environment:
      # Optional but recommended - enables auto-service-creation
      - TAILSCALE_OAUTH_CLIENT_ID=${TAILSCALE_OAUTH_CLIENT_ID}
      - TAILSCALE_OAUTH_CLIENT_SECRET=${TAILSCALE_OAUTH_CLIENT_SECRET}
```

> **Note:** We mount the `/var/run/tailscale` directory rather than the socket file directly. When `tailscaled` restarts, it recreates the socket with a new inode — a file bind mount would go stale, but a directory mount stays in sync automatically.

**Host tag requirement:** The host machine running Tailscale must advertise a tag that matches your ACL auto-approvers (see [Tailscale Admin Setup](#tailscale-admin-setup)). For example:

```bash
sudo tailscale up --advertise-tags=tag:server --reset
```

> **Warning:** The `--reset` flag briefly drops the Tailscale connection. If you are connected via SSH over Tailscale, your session will be interrupted momentarily. The connection will restore automatically once Tailscale reconnects.

When using the sidecar setup below, the sidecar container handles its own tags via `TS_AUTHKEY`, so this step is not needed.

### Tailscale Sidecar

For systems without Tailscale installed on the host:

```yaml
services:
  tailscale:
    image: tailscale/tailscale:latest
    hostname: docktail-host
    environment:
      - TS_AUTHKEY=${TAILSCALE_AUTH_KEY}
      - TS_EXTRA_ARGS=--advertise-tags=tag:server
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

Set `TAILSCALE_AUTH_KEY` to authenticate the Tailscale container (generate at [Tailscale Admin → Settings → Keys](https://login.tailscale.com/admin/settings/keys)). The sidecar must advertise `tag:server` so it can satisfy the ACL auto-approver example shown below.

## Tailscale Admin Setup

After deploying DockTail, you need to configure a few things in the [Tailscale Admin Console](https://login.tailscale.com/admin) for services to work.

### 1. Configure Access Controls (ACLs)

Services require two things in your ACL policy: **tag definitions** in `tagOwners` and **auto-approvers** to allow the host machine to advertise them.

Go to [Access Controls](https://login.tailscale.com/admin/acls) and add both blocks:

```json
{
  "tagOwners": {
    "tag:server": ["autogroup:admin"],
    "tag:container": ["tag:server"]
  },
  "autoApprovers": {
    "services": {
      "tag:container": ["tag:server"]
    }
  }
}
```

> **Note:** If you manage your ACLs via GitOps (e.g., [tailscale/gitops-acl-action](https://github.com/marketplace/actions/sync-tailscale-acls)), both tags **must** be defined in `tagOwners` — otherwise the sync will fail because Tailscale rejects references to undefined tags.

**How the tags work:**

- `tag:server` — assigned to the host machine (or sidecar auth key) that runs DockTail. This identifies *who* is allowed to advertise services.
- `tag:container` — the default tag DockTail assigns to the Tailscale services it creates. This identifies *what* is being advertised.

Adjust the tags to match your setup — the right side of `autoApprovers` must match the tag on your host machine, and the left side must match the `docktail.tags` label on your containers (defaults to `tag:container`).

### 2. Approve the Service

The first time a new service is advertised, it must be manually approved in the Tailscale Admin Console:

1. Go to the [Services tab](https://login.tailscale.com/admin/services)
2. Find the newly advertised service
3. Approve it to allow traffic

After the initial approval, the service will continue to work automatically on subsequent container restarts. If you are using OAuth or API key mode, service definitions are auto-created, but the first approval may still be required depending on your ACL configuration.

## Labeling Containers

Your application containers (nginx, postgres, APIs, etc.) live alongside DockTail in the same Docker Compose file (or on the same Docker host). DockTail **does not run your services** — it watches for containers with `docktail.*` labels and automatically advertises them as Tailscale services. Each labeled container becomes its own independent Tailscale service.

### Direct Container IP Proxying (Default)

By default, DockTail proxies directly to container IPs on the Docker bridge network. **No port publishing required!**

```yaml
services:
  myapp:
    image: nginx:latest
    # No ports needed!
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=myapp"
      - "docktail.service.port=80"
```

DockTail automatically detects the container's IP address and configures Tailscale to proxy directly to it. When containers restart and get new IPs, DockTail automatically updates the configuration.

**Opt-out:** Set `docktail.service.direct=false` to use published port bindings instead (legacy behavior).

### Service Labels

| Label | Required | Default | Description |
|-------|----------|---------|-------------|
| `docktail.service.enable` | Yes | - | Enable DockTail for container |
| `docktail.service.name` | Yes | - | Service name (e.g., `web`, `api`) |
| `docktail.service.port` | Yes | - | Container port to proxy to |
| `docktail.service.direct` | No | `true` | Proxy directly to container IP (no port publishing needed) |
| `docktail.service.network` | No | `bridge` | Docker network to use for container IP |
| `docktail.service.protocol` | No | Smart* | Container protocol: `http`, `https`, `https+insecure`, `tcp`, `tls-terminated-tcp` |
| `docktail.service.service-port` | No | Smart** | Port Tailscale listens on |
| `docktail.service.service-protocol` | No | Smart*** | Tailscale protocol: `http`, `https`, `tcp` |
| `docktail.tags` | No | `tag:container` | Comma-separated tags for ACLs |

**Smart Defaults:**
- \* `protocol`: `https` if container port is 443, otherwise `http`
- \** `service-port`: `443` if service-protocol is `https`, otherwise `80`
- \*** `service-protocol`: `https` if service-port is 443, matches `protocol` for TCP, otherwise `http`

### Funnel Labels (Public Internet Access)

Funnel exposes your service to the **public internet**. It can be used together with a DockTail service or on its own for funnel-only containers.

| Label | Required | Default | Description |
|-------|----------|---------|-------------|
| `docktail.funnel.enable` | Yes | `false` | Enable Tailscale Funnel |
| `docktail.funnel.port` | Yes | - | Container port |
| `docktail.funnel.funnel-port` | No | `443` | Public port (443, 8443, or 10000) |
| `docktail.funnel.protocol` | No | `https` | Protocol: `https`, `tcp`, `tls-terminated-tcp` |

**Notes:**
- Only ONE funnel per port (Tailscale limitation)
- Uses machine hostname, not service name: `https://<machine>.<tailnet>.ts.net`
- Funnel-only containers can omit `docktail.service.enable` and the other `docktail.service.*` labels entirely
- `docktail.service.direct` and `docktail.service.network` still control how DockTail reaches the container backend for funnel traffic

## Examples

### Web Application

```yaml
services:
  nginx:
    image: nginx:latest
    # No ports needed - DockTail proxies directly to container IP
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
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=api"
      - "docktail.service.port=3000"
      - "docktail.service.service-port=443"  # Auto-enables HTTPS
```

Access: `https://api.your-tailnet.ts.net`

> **Note:** If switching an existing service from http to https, you must first delete it from the Tailscale advertised service panel.

### Database (TCP)

```yaml
services:
  postgres:
    image: postgres:16
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=db"
      - "docktail.service.port=5432"
      - "docktail.service.protocol=tcp"
      - "docktail.service.service-port=5432"
```

### Multiple Services from One Container

A single container can expose multiple separate Tailscale services using numbered labels (`docktail.service.N.*`). Each indexed entry requires its own `name` and `port`, and becomes an independent service. Tags and network are inherited from the primary config.

This is useful for VPN gateway containers (e.g., gluetun) where multiple apps route through a single container but each needs its own Tailscale service.

```yaml
services:
  gluetun:
    image: gluetun:latest
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=qbittorrent"
      - "docktail.service.port=8000"
      - "docktail.service.1.name=bitmagnet"
      - "docktail.service.1.port=8001"
```

Per-index overridable labels: `name` (required), `port` (required), `service-port`, `protocol`, and `service-protocol`.

### Custom Docker Network

```yaml
services:
  app:
    image: myapp:latest
    networks:
      - backend
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=app"
      - "docktail.service.port=3000"
      - "docktail.service.network=backend"  # Specify which network to use

networks:
  backend:
```

### Legacy Mode (Published Ports)

```yaml
services:
  app:
    image: myapp:latest
    ports:
      - "8080:3000"  # Required when direct=false
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=app"
      - "docktail.service.port=3000"
      - "docktail.service.direct=false"  # Use published port instead of container IP
```

### Public Website with Funnel

```yaml
services:
  website:
    image: nginx:latest
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

### Funnel-Only Public Proxy

```yaml
services:
  immich-public-proxy:
    image: ghcr.io/immich-app/immich-public-proxy:latest
    labels:
      - "docktail.funnel.enable=true"
      - "docktail.funnel.port=3000"
      - "docktail.funnel.funnel-port=8443"
```

Access:
- Public only: `https://your-machine.your-tailnet.ts.net:8443`

## Reference

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `TAILSCALE_OAUTH_CLIENT_ID` | - | OAuth Client ID (optional, enables auto-service-creation) |
| `TAILSCALE_OAUTH_CLIENT_SECRET` | - | OAuth Client Secret (optional, enables auto-service-creation) |
| `TAILSCALE_API_KEY` | - | API Key (optional alternative to OAuth, expires 90 days) |
| `TAILSCALE_TAILNET` | `-` | Tailnet ID (defaults to key's tailnet) |
| `DEFAULT_SERVICE_TAGS` | `tag:container` | Default tags for services |
| `IGNORE_SERVICE_NAMES` | - | Comma-separated service names DockTail must not drain or clear during reconciliation or shutdown cleanup |
| `LOG_LEVEL` | `info` | Logging level (debug, info, warn, error) |
| `RECONCILE_INTERVAL` | `60s` | State reconciliation interval |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon socket |
| `TAILSCALE_SOCKET` | `/var/run/tailscale/tailscaled.sock` | Tailscale daemon socket |

If both OAuth and API key are set, OAuth takes precedence.

`IGNORE_SERVICE_NAMES` accepts either bare names like `grafana` or fully qualified names like `svc:grafana`.

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

See [Tailscale Admin Setup](#tailscale-admin-setup) for the required ACL auto-approver configuration and service approval steps.

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
 │           │ Monitoring                 │ container IP  │
 │           ▼                            ▼               │
 │  ┌──────────────────┐         ┌──────────────────┐     │
 │  │   App Container  │◀────────│  172.17.0.3:80   │     │
 │  │   Port 80        │         │  (bridge network)│     │
 │  │  No ports needed │◀────────│                  │     │
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
3. **IP Detection** - Gets container IP from Docker network settings (default: bridge)
4. **Config Generation** - Creates Tailscale service config proxying to container IP
5. **Service Advertisement** - Executes Tailscale CLI to advertise services
6. **Control Plane Sync** - If OAuth/API key configured, creates service definitions via API
7. **Reconciliation** - Periodically syncs state; auto-updates when container IPs change

**Notes:**
- DockTail does NOT delete service definitions from the API when containers stop (conservative deletion strategy)
- Container IP changes on restart are handled automatically during reconciliation

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
