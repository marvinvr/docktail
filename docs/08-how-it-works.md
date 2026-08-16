## How It Works

DockTail is a reconciliation loop between Docker and Tailscale.

```text
Docker container labels
        |
        v
DockTail watches Docker events
        |
        v
DockTail parses service and Funnel config
        |
        v
DockTail resolves the backend IP and port
        |
        v
Tailscale CLI advertises services and Funnels
        |
        v
Tailnet clients access container services
```

### Reconciliation Flow

1. DockTail monitors Docker events for container starts and stops.
2. It extracts service configuration from container labels.
3. It resolves the backend destination from Docker network settings or published ports.
4. It generates Tailscale service configuration pointing to that backend.
5. It executes the Tailscale CLI to advertise services and Funnels.
6. If OAuth or API key credentials are configured, it creates service definitions through the Tailscale API and keeps their tags and descriptions in sync with the labels; manual edits to either are overwritten.
7. If `DELETE_UNUSED_SERVICES` is enabled, it deletes tailnet service definitions that no host advertises anymore.
8. It periodically reconciles state so container IP changes are handled automatically.

### Networking Model

Direct mode is the default. DockTail reaches containers through their Docker network IPs, so application containers do not need published host ports.

When `docktail.service.direct=false`, DockTail uses Docker published port bindings instead. In that mode, the target port must be published to the host.

TCP forwards hide the tailnet client address by default: the backend sees tailscaled. Set `docktail.service.proxy-protocol` to `1` or `2` so Tailscale prepends a PROXY protocol header. The backend must accept that header; HTTP/HTTPS services cannot use it.

Containers using `network_mode: host` are served on `127.0.0.1` (IPv4, to avoid dual-stack `localhost`→`::1` refusals). The local health check probes them from wherever the agent runs: directly via `127.0.0.1` when the agent shares the host's network namespace, or via the agent's docker-network gateway (the host's bridge address) when the agent runs in its own container. Containers using `network_mode: none` cannot use direct mode.
