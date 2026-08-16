## Reference

Use this section when checking exact configuration names, defaults, and supported protocols.

### Environment Variables

| Variable | Default | Description |
| --- | --- | --- |
| `TAILSCALE_OAUTH_CLIENT_ID` | - | OAuth client ID. Enables automatic service creation when paired with the secret. |
| `TAILSCALE_OAUTH_CLIENT_SECRET` | - | OAuth client secret. Enables automatic service creation when paired with the client ID. |
| `TAILSCALE_API_KEY` | - | API key alternative to OAuth. |
| `TAILSCALE_TAILNET` | `-` | Tailnet ID. Defaults to the credential's tailnet. |
| `DEFAULT_SERVICE_TAGS` | `tag:container` | Default tags for services whose containers set no `docktail.tags` label. Tags are reconciled on every cycle; manual tag edits in the admin console are overwritten. |
| `IGNORE_SERVICE_NAMES` | - | Comma-separated service names DockTail must not drain, clear, or delete during reconciliation or shutdown cleanup. |
| `DELETE_UNUSED_SERVICES` | `false` | When `true`, DockTail deletes tailnet Service definitions that no host advertises anymore. Requires API credentials. See [Cleanup Behavior](#cleanup-behavior). |
| `SKIP_SHUTDOWN_CLEANUP` | `false` | When `true`, DockTail leaves its services and Funnels advertised on shutdown instead of draining and clearing them. This can keep ports exposed on the tailnet beyond what your current labels define; see [Cleanup Behavior](#cleanup-behavior). |
| `LOG_LEVEL` | `info` | Logging level: `debug`, `info`, `warn`, or `error`. |
| `RECONCILE_INTERVAL` | `60s` | State reconciliation interval. |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon socket. Rootless Docker typically uses `unix:///run/user/<uid>/docker.sock`. |
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

#### DockTail Cloud (optional)

These variables enable optional DockTail Cloud reporting. They are opt-in: the agent is completely inert unless `DOCKTAIL_CLOUD_KEY` is set. See [DockTail Cloud](#docktail-cloud).

| Variable | Default | Description |
| --- | --- | --- |
| `DOCKTAIL_CLOUD_KEY` | - | Workspace key (`dtc_...`) from the cloud dashboard. Enables reporting. Inert when unset. |
| `DOCKTAIL_LOG_LEVEL` | `info` | Log level for the cloud module: `debug`, `info`, `warn`, or `error`. |
| `DOCKTAIL_CHECK_INTERVAL` | `30s` | How often local-vantage checks run (5s–5m). |

Local-development overrides: `DOCKTAIL_CLOUD_URL` replaces the built-in ingest
endpoint. `ws://` is allowed for loopback endpoints; non-loopback plaintext
requires `DOCKTAIL_CLOUD_ALLOW_INSECURE=true` and must never be used in
production. Non-loopback production endpoints must use `wss://`.

### Supported Protocols

Tailscale-facing `docktail.service.service-protocol` values:

| Value | Description |
| --- | --- |
| `http` | Layer 7 HTTP. |
| `https` | Layer 7 HTTPS with automatic TLS. |
| `tcp` | Layer 4 TCP. |
| `tls-terminated-tcp` | Layer 4 TCP; Tailscale terminates incoming TLS and forwards decrypted TCP to the backend. |

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

DockTail cleans up the services and Funnels it advertises locally when it shuts down (draining then clearing them). This is the safe default: when DockTail is not running, nothing it configured stays reachable, so the advertised surface can never drift from what your labels describe.

`SKIP_SHUTDOWN_CLEANUP=true` disables this cleanup. DockTail then leaves its services and Funnels advertised when it exits instead of tearing them down. It affects only graceful shutdown; a hard crash never runs cleanup either way.

> **Warning:** Enabling this keeps ports exposed on the tailnet while DockTail is down, potentially beyond what your current labels define. The serve and Funnel configuration lives in `tailscaled`, not in DockTail, so anything DockTail last advertised keeps serving on the host until DockTail comes back. If you stop a container, remove it, or delete its DockTail labels while DockTail is not running, that service (and any Funnel) stays reachable for the entire downtime and is only reconciled away once DockTail restarts. The default cleanup exists precisely to prevent this stale exposure. Only enable `SKIP_SHUTDOWN_CLEANUP` if keeping services reachable across DockTail restarts is worth giving up that guarantee, and treat the advertised surface as detached from your labels until DockTail is running again.

On restart, DockTail re-adopts the still-advertised services and removes only those whose containers are no longer running. Ignored services (`IGNORE_SERVICE_NAMES`) are never cleaned up regardless of this setting.

By default DockTail does **not** delete Tailscale Service definitions from the Control Plane when containers stop; this is a conservative strategy that avoids removing definitions unexpectedly.

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
