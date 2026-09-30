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
| `LOG_LEVEL` | `info` | Logging level for all output, including the DockTail Cloud module: `debug`, `info`, `warn`, or `error`. Any other value means `info`. |
| `RECONCILE_INTERVAL` | `60s` | State reconciliation interval. |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon socket. Rootless Docker typically uses `unix:///run/user/<uid>/docker.sock`. |
| `TAILSCALE_SOCKET` | `/var/run/tailscale/tailscaled.sock` | The `tailscaled` socket DockTail checks: the startup missing-socket hint, the [socket-loss check](#tailscale-socket-loss), and the image's health check. DockTail does not pass it to the bundled `tailscale` CLI, which does the serve and Funnel work at its own default of `/var/run/tailscale/tailscaled.sock`, so mount the daemon's socket directory at `/var/run/tailscale` either way. |
| `EXIT_ON_SOCKET_LOSS` | `true` | When `true`, DockTail exits if the Tailscale socket stays unreachable past the grace period, so the container's restart policy can re-establish the mount. See [Tailscale Socket Loss](#tailscale-socket-loss). |
| `SOCKET_LOSS_GRACE_PERIOD` | `90s` | How long the Tailscale socket may stay unreachable before DockTail exits. Must be longer than a normal `tailscaled` restart. |

If both OAuth and API key credentials are configured, DockTail uses OAuth.

Sensitive credential variables can also be loaded from files. This is useful with Docker secrets, Swarm secrets, and other secret mounts. Direct environment variables take precedence; when the direct variable is unset, DockTail checks `FILE__VARIABLE_NAME` first and `VARIABLE_NAME_FILE` second. The file content is used as the value with trailing newlines removed.

Supported file-backed credential variables:

| Direct variable | File variable alternatives |
| --- | --- |
| `TAILSCALE_OAUTH_CLIENT_ID` | `FILE__TAILSCALE_OAUTH_CLIENT_ID`, `TAILSCALE_OAUTH_CLIENT_ID_FILE` |
| `TAILSCALE_OAUTH_CLIENT_SECRET` | `FILE__TAILSCALE_OAUTH_CLIENT_SECRET`, `TAILSCALE_OAUTH_CLIENT_SECRET_FILE` |
| `TAILSCALE_API_KEY` | `FILE__TAILSCALE_API_KEY`, `TAILSCALE_API_KEY_FILE` |

`IGNORE_SERVICE_NAMES` accepts bare names like `grafana` and fully qualified names like `svc:grafana`.

Durations (`RECONCILE_INTERVAL`, `SOCKET_LOSS_GRACE_PERIOD`) use Go syntax such as `30s`, `5m` or `1h30m`; booleans accept `true`/`false`, `1`/`0`, and `t`/`f`. An unparseable value in one of the variables above logs a warning and falls back to the default.

#### Docker Connection And Log Output

DockTail builds its Docker client from the standard Docker environment variables, so they work as they do for the `docker` CLI. Besides `DOCKER_HOST` above, which also takes `tcp://host:port` (for example a [read-only socket proxy](02-security.md#read-only-socket-proxy)):

| Variable | Default | Description |
| --- | --- | --- |
| `DOCKER_API_VERSION` | negotiated | Pins the Docker API version instead of negotiating it with the daemon. |
| `DOCKER_CERT_PATH` | - | Directory with `ca.pem`, `cert.pem` and `key.pem`. Setting it makes DockTail talk TLS to a `tcp://` daemon. |
| `DOCKER_TLS_VERIFY` | - | Used with `DOCKER_CERT_PATH`: any non-empty value verifies the daemon's certificate; unset or empty skips verification. |

Log lines are colored only when stdout is a terminal; setting `NO_COLOR` (to any value) or `TERM=dumb` turns color off there too.

`TAILSCALE_AUTH_KEY` in the examples is read by the `tailscale/tailscale` sidecar container (as `TS_AUTHKEY`), not by DockTail.

#### DockTail Cloud (optional)

These variables enable optional DockTail Cloud reporting. They are opt-in: the agent is completely inert unless `DOCKTAIL_CLOUD_KEY` is set. See [DockTail Cloud](#docktail-cloud).

| Variable | Default | Description |
| --- | --- | --- |
| `DOCKTAIL_CLOUD_KEY` | - | Workspace key (`dtc_...`) from the cloud dashboard. Enables reporting. Inert when unset. |
| `DOCKTAIL_LOG_LEVEL` | `info` | Read, but currently has no effect: the cloud module logs at the level `LOG_LEVEL` sets. |
| `DOCKTAIL_CHECK_INTERVAL` | `30s` | How often local-vantage checks run (5s–5m). A value outside that range, or one that does not parse, keeps the cloud module from starting; DockTail itself keeps running. |
| `DOCKTAIL_HOST_ROOT` | `/host` | Where the host's root filesystem is bind-mounted, for whole-host disk usage. Only used when that path exists; see [Disk Usage](06-cloud.md#disk-usage). |

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

`docktail.service.path` selects the HTTP(S) Serve mount point and defaults to `/`. It must start with `/` and is rejected for TCP protocols.

`docktail.service.proxy-protocol` (`1` or `2`) is valid only with `tcp` or `tls-terminated-tcp`. It is off by default because the backend must understand the PROXY header. HTTP/HTTPS values are rejected.

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
3. Asks the Control Plane which hosts are registered for the Service. If **at least one** host is registered, DockTail keeps it.
4. Deletes the Service only when **no** host is registered for it.

Because the decision is based on the tailnet-wide host list, this is safe to enable on multiple DockTail instances at once: a Service hosted by any other host or instance always reports at least one host and is never deleted. DockTail also skips deletion whenever an API call fails, so it never deletes under uncertainty.

> **Note:** That host list is a configuration and approval registry, not a liveness signal. A host stays listed while it is configured to host the Service, and remains listed for some time after it stops advertising it. Cleanup can therefore lag behind reality — it may keep a definition longer than expected, but it will not delete one that is still in use.

This cleanup runs only during reconciliation, not during shutdown, so restarting DockTail does not delete and recreate the Services of still-running containers.

> **Note:** When enabled, DockTail may also delete Service definitions it did not create if they have no advertising hosts (for example, a Service you defined in the admin console but never advertised). Add such names to `IGNORE_SERVICE_NAMES` to protect them.

### Tailscale Socket Loss

DockTail talks to `tailscaled` over a Unix socket that it bind-mounts from the
host or shares with a sidecar. That mount is resolved once, when the container
starts.

This matters whenever `tailscaled` restarts. On a host install under systemd, the
unit declares `RuntimeDirectory=tailscale` and leaves `RuntimeDirectoryPreserve`
at its default of `no`, so systemd **removes `/run/tailscale` when the daemon
stops and creates a new directory when it starts**. A container that mounted the
old directory stays attached to it after it is unlinked, and the new socket never
becomes visible inside the container. Sharing the socket through a host path with
a sidecar that gets recreated has the same effect. Retrying cannot help: the
socket is not late, it is in a directory this container can no longer see.

An upgrade is the most common trigger, because upgrading the package restarts the
daemon — but it is not the only one. Measured on Debian 12 (systemd 252,
tailscale 1.102.2), an ordinary `systemctl restart tailscaled` replaces the
directory just as an upgrade does, and so does any stop/start pair:

| | directory | DockTail's mount |
| --- | --- | --- |
| before | inode 264, socket present | inode 264, socket present |
| after `systemctl restart tailscaled` | inode 412, socket present | inode 264, empty |
| after the container restarts | inode 412, socket present | inode 412, socket present |

Because of this, mounting the directory rather than the socket file — which is
what the setup guide recommends, and which does survive the socket file being
recreated — is not on its own enough to survive a daemon restart on a
systemd host.

Without intervention DockTail would keep running in that state — process alive,
every Tailscale call failing, and every Service it manages drifting until it goes
offline, with no recovery until someone restarts the container by hand.

So DockTail probes the socket, and if it stays unreachable for
`SOCKET_LOSS_GRACE_PERIOD` (default `90s`) it logs the reason and exits, letting
the container's restart policy re-create the container and with it the mount.

- **Use a restart policy.** `restart: unless-stopped` (or `always`) is what turns
  the exit into a recovery. Without one, DockTail stops instead of restarting.
- The grace period must stay comfortably longer than a normal `tailscaled`
  restart, which takes a second or two. Brief outages are ignored and never
  cause an exit.
- The check arms only after the socket has been reachable at least once, so
  starting DockTail before `tailscaled` waits rather than exits.
- Set `EXIT_ON_SOCKET_LOSS=false` to disable it and keep the old behaviour of
  retrying forever.

Prefer a named volume over a host path when you run `tailscaled` as a sidecar: a
volume keeps one directory for its lifetime, so recreating the sidecar cannot
detach DockTail's mount in the first place.

### Useful Links

- Tailscale Services documentation: `https://tailscale.com/kb/1552/tailscale-services`
- Tailscale Funnel documentation: `https://tailscale.com/kb/1311/tailscale-funnel`
- Tailscale service configuration reference: `https://tailscale.com/kb/1589/tailscale-services-configuration-file`
- Docker SDK for Go: `https://docs.docker.com/engine/api/sdk/`
