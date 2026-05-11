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
| `IGNORE_SERVICE_NAMES` | - | Comma-separated service names DockTail must not drain or clear during reconciliation or shutdown cleanup. |
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

### Cleanup Behavior

DockTail cleans up the services it advertises locally when it shuts down. It does not delete Tailscale service definitions from the Admin Console API when containers stop; this is a conservative deletion strategy to avoid removing definitions unexpectedly.

### Useful Links

- Tailscale Services documentation: `https://tailscale.com/kb/1552/tailscale-services`
- Tailscale Funnel documentation: `https://tailscale.com/kb/1311/tailscale-funnel`
- Tailscale service configuration reference: `https://tailscale.com/kb/1589/tailscale-services-configuration-file`
- Docker SDK for Go: `https://docs.docker.com/engine/api/sdk/`
