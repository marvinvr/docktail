## Labels

DockTail watches containers with `docktail.*` labels. Each labeled container can become a private Tailscale service, a public Funnel, or both. DockTail does not run your application containers; it only observes them and configures Tailscale.

### Direct Container IP Proxying

By default, DockTail proxies directly to container IPs on the Docker network. No Docker port publishing is required.

```yaml
services:
  myapp:
    image: nginx:latest
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=myapp"
      - "docktail.service.port=80"
```

Set `docktail.service.direct=false` to use published host ports instead. Use this for legacy setups, or when host `tailscaled` cannot reach container IPs (common with [rootless Docker](#rootless-docker)).

### Service Labels

| Label | Required | Default | Description |
| --- | --- | --- | --- |
| `docktail.service.enable` | Yes | - | Enable a private Tailscale service for the container. |
| `docktail.service.name` | Yes | - | Service name, such as `web` or `api`. |
| `docktail.service.port` | Yes | - | Backend container port to proxy to. |
| `docktail.service.description` | No | - | Human-readable description shown for the service in the Tailscale admin panel. Requires API credentials (synced to the Service definition's `comment`). |
| `docktail.service.direct` | No | `true` | Proxy directly to container IP instead of requiring a published host port. Set `false` when host `tailscaled` cannot reach container IPs (rootless Docker). |
| `docktail.service.network` | No | `bridge` or first available | Docker network used for direct container IP detection. |
| `docktail.service.protocol` | No | Smart | Backend protocol. |
| `docktail.service.service-port` | No | Smart | Port Tailscale listens on. |
| `docktail.service.service-protocol` | No | Smart | Tailscale-facing protocol. |
| `docktail.tags` | No | `tag:container` | Comma-separated service tags. Labels are the source of truth; see the note below. |

Tags are synced to the tailnet Service definition through the Tailscale API and require API credentials. Labels are authoritative: DockTail reconciles tags on every cycle, so tags edited by hand in the Tailscale admin console are reverted to the label-declared set on the next reconcile. Containers without a `docktail.tags` label use `DEFAULT_SERVICE_TAGS`.

Smart defaults:

- `docktail.service.protocol` defaults to `https` when the backend port is `443`; otherwise it defaults to `http`.
- `docktail.service.service-port` defaults to `443` when `service-protocol` is `https`; otherwise it defaults to `80`.
- `docktail.service.service-protocol` defaults to `https` when the service port is `443`, to `tcp` when the backend protocol is TCP, and otherwise to `http`.

### Multiple Services From One Container

A single container can expose multiple separate Tailscale services using numbered labels:

```yaml
services:
  gluetun:
    image: qmcgaw/gluetun:latest
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=qbittorrent"
      - "docktail.service.port=8000"
      - "docktail.service.1.name=bitmagnet"
      - "docktail.service.1.port=8001"
```

Each indexed service requires its own `name` and `port`. Per-index overridable labels are `name`, `port`, `service-port`, `protocol`, `service-protocol`, and `description`. Tags and network settings are inherited from the primary service config.

### Funnel Labels

Funnel exposes a service to the public internet. It can be used together with a private DockTail service or on its own for funnel-only containers.

| Label | Required | Default | Description |
| --- | --- | --- | --- |
| `docktail.funnel.enable` | Yes | `false` | Enable Tailscale Funnel. |
| `docktail.funnel.port` | Yes | - | Backend container port for Funnel traffic. |
| `docktail.funnel.funnel-port` | No | `443` | Public Funnel port. HTTPS/HTTP Funnel supports `443`, `8443`, or `10000`. |
| `docktail.funnel.protocol` | No | `https` | Funnel protocol: `http`, `https`, `tcp`, or `tls-terminated-tcp`. |
| `docktail.funnel.path` | No | `/` | HTTP(S) Funnel path. Must start with `/`. |

Funnel notes:

- HTTP(S) Funnels can share a public port when each Funnel uses a different `docktail.funnel.path`.
- TCP and TLS-terminated TCP Funnels support only one active Funnel per public port on a node.
- `docktail.funnel.path` is only valid for HTTP(S) Funnels.
- Funnel URLs use the machine hostname, not the Tailscale service name.
- Funnel-only containers can omit `docktail.service.enable` and other `docktail.service.*` labels.
- `docktail.service.direct` and `docktail.service.network` still control how DockTail reaches the backend for Funnel traffic.
