## Reference

Use this section when checking exact configuration names, defaults, and supported protocols.

### Environment Variables

| Variable | Default | Description |
| --- | --- | --- |
| `TAILSCALE_OAUTH_CLIENT_ID` | - | OAuth client ID. Enables automatic service creation when paired with the secret. |
| `TAILSCALE_OAUTH_CLIENT_SECRET` | - | OAuth client secret. Enables automatic service creation when paired with the client ID. |
| `TAILSCALE_API_KEY` | - | API key alternative to OAuth. |
| `TAILSCALE_TAILNET` | `-` | Tailnet ID. Defaults to the credential's tailnet. |
| `DEFAULT_SERVICE_TAGS` | `tag:container` | Default tags assigned to services. |
| `IGNORE_SERVICE_NAMES` | - | Comma-separated service names DockTail must not drain, clear, or delete during reconciliation or shutdown cleanup. |
| `DELETE_UNUSED_SERVICES` | `false` | When `true`, DockTail deletes tailnet Service definitions that no host advertises anymore. Requires API credentials. See [Cleanup Behavior](#cleanup-behavior). |
| `SKIP_SHUTDOWN_CLEANUP` | `false` | When `true`, DockTail leaves its services and Funnels advertised on shutdown instead of draining and clearing them, so they stay reachable while DockTail is down. See [Cleanup Behavior](#cleanup-behavior). |
| `LOG_LEVEL` | `info` | Logging level: `debug`, `info`, `warn`, or `error`. |
| `RECONCILE_INTERVAL` | `60s` | State reconciliation interval. |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon socket. |
| `TAILSCALE_SOCKET` | `/var/run/tailscale/tailscaled.sock` | Tailscale daemon socket. |

If both OAuth and API key credentials are configured, DockTail uses OAuth.

Sensitive credential variables can also be loaded from files. This is useful with Docker secrets, Swarm secrets, and other secret mounts. Direct environment variables take precedence; when the direct variable is unset, DockTail checks `FILE__VARIABLE_NAME` first and `VARIABLE_NAME_FILE` second. The file content is used as the value with trailing newlines removed.

Supported file-backed credential variables:

| Direct variable | File variable alternatives |
| --- | --- |
| `TAILSCALE_OAUTH_CLIENT_ID` | `FILE__TAILSCALE_OAUTH_CLIENT_ID`, `TAILSCALE_OAUTH_CLIENT_ID_FILE` |
| `TAILSCALE_OAUTH_CLIENT_SECRET` | `FILE__TAILSCALE_OAUTH_CLIENT_SECRET`, `TAILSCALE_OAUTH_CLIENT_SECRET_FILE` |
| `TAILSCALE_API_KEY` | `FILE__TAILSCALE_API_KEY`, `TAILSCALE_API_KEY_FILE` |

`IGNORE_SERVICE_NAMES` accepts bare names like `grafana` and fully qualified names like `svc:grafana`.

### Supported Protocols

Tailscale-facing `docktail.service.service-protocol` values:

| Value | Description |
| --- | --- |
| `http` | Layer 7 HTTP. |
| `https` | Layer 7 HTTPS with automatic TLS. |
| `tcp` | Layer 4 TCP. |
| `tls-terminated-tcp` | Layer 4 TCP with TLS termination. |

Container-facing `docktail.service.protocol` values:

| Value | Description |
| --- | --- |
| `http` | HTTP backend. |
| `https` | HTTPS backend with a valid certificate. |
| `https+insecure` | HTTPS backend with a self-signed certificate. |
| `tcp` | TCP backend. |
| `tls-terminated-tcp` | TCP backend with TLS termination. |

Funnel `docktail.funnel.protocol` values:

| Value | Description |
| --- | --- |
| `http` | HTTP Funnel. |
| `https` | HTTPS Funnel. |
| `tcp` | TCP Funnel. |
| `tls-terminated-tcp` | TLS-terminated TCP Funnel. |

`docktail.funnel.path` is supported only with HTTP(S) Funnel protocols. It defaults to `/` and must start with `/`.

### Cleanup Behavior

DockTail cleans up the services and Funnels it advertises locally when it shuts down (draining then clearing them).

Set `SKIP_SHUTDOWN_CLEANUP=true` to skip this. DockTail then leaves its services and Funnels advertised when it exits, so they stay reachable while DockTail is down. This affects only graceful shutdown; a hard crash never runs cleanup either way. On restart, DockTail re-adopts the still-advertised services and removes only those whose containers are no longer running. Ignored services (`IGNORE_SERVICE_NAMES`) are unaffected because they are never cleaned up regardless of this setting.

By default it does **not** delete Tailscale Service definitions from the Control Plane when containers stop; this is a conservative strategy that avoids removing definitions unexpectedly.

#### Deleting unused Service definitions

Set `DELETE_UNUSED_SERVICES=true` to let DockTail remove Service definitions that are no longer advertised by any host. This requires API credentials (OAuth or API key). It is disabled by default.

During each reconciliation, for every Service definition in the tailnet DockTail:

1. Keeps the Service if DockTail currently advertises it (it is backed by a running container).
2. Keeps the Service if its name is listed in `IGNORE_SERVICE_NAMES`.
3. Asks the Control Plane which hosts advertise the Service. If **at least one** host advertises it, DockTail keeps it.
4. Deletes the Service only when **no** host advertises it.

Because the decision is based on the tailnet-wide advertiser count, this is safe to enable on multiple DockTail instances at once: a Service advertised by any other host or instance always reports at least one host and is never deleted. DockTail also skips deletion whenever an API call fails, so it never deletes under uncertainty.

This cleanup runs only during reconciliation, not during shutdown, so restarting DockTail does not delete and recreate the Services of still-running containers.

> **Note:** When enabled, DockTail may also delete Service definitions it did not create if they have no advertising hosts (for example, a Service you defined in the admin console but never advertised). Add such names to `IGNORE_SERVICE_NAMES` to protect them.

### Useful Links

- Tailscale Services documentation: `https://tailscale.com/kb/1552/tailscale-services`
- Tailscale Funnel documentation: `https://tailscale.com/kb/1311/tailscale-funnel`
- Tailscale service configuration reference: `https://tailscale.com/kb/1589/tailscale-services-configuration-file`
- Docker SDK for Go: `https://docs.docker.com/engine/api/sdk/`
