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
| `DIAGNOSTICS` | `false` | When `true`, DockTail records the node's Tailscale service hosting state to a file for troubleshooting. See [Diagnostics](#diagnostics). |
| `DIAGNOSTICS_FILE` | `/diagnostics/docktail-diagnostics.jsonl` | Where diagnostics records are appended. Mount a volume at this path to keep them. Set to an empty value to record to the log only. |
| `DIAGNOSTICS_INTERVAL` | `10s` | How often diagnostics samples the hosting state. |
| `DIAGNOSTICS_HEARTBEAT` | `10m` | How often a record is written even when nothing changed, so a quiet period is distinguishable from a stopped agent. |
| `RECONCILE_INTERVAL` | `60s` | State reconciliation interval. |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon socket. |
| `TAILSCALE_SOCKET` | `/var/run/tailscale/tailscaled.sock` | Tailscale daemon socket. |
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

### Tailscale Socket Loss

DockTail talks to `tailscaled` over a Unix socket that it bind-mounts from the
host or shares with a sidecar. That mount is resolved once, when the container
starts.

This matters when `tailscaled` restarts. On a host install under systemd, the
unit declares `RuntimeDirectory=tailscale` and leaves `RuntimeDirectoryPreserve`
at its default of `no`, so systemd **deletes `/run/tailscale` when the daemon
stops and creates a new directory when it starts**. A container that mounted the
old directory stays attached to it after it is unlinked, and the new socket never
becomes visible inside the container. Sharing the socket through a host path with
a sidecar that gets recreated has the same effect. Retrying cannot help: the
socket is not late, it is in a directory this container can no longer see.

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

### Diagnostics

Diagnostics is an opt-in troubleshooting mode for the case where a Service shows
as offline in the admin console while its container is healthy and DockTail's
own logs look clean. It is completely inert unless `DIAGNOSTICS=true`.

A node hosts a Tailscale Service only when two independent pieces of local state
agree:

1. the **serve config** carries handlers for it, and
2. **`prefs.AdvertiseServices`** lists it.

`tailscale serve status` — the command DockTail's reconciliation is based on —
shows only the first. A service that has serve config but is not advertised
therefore looks completely healthy to DockTail while being unreachable to the
rest of the tailnet. Diagnostics records both halves so that state is visible.

Enable it by setting `DIAGNOSTICS=true` and mounting a volume for the output:

```yaml
services:
  docktail:
    image: ghcr.io/marvinvr/docktail:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /var/run/tailscale:/var/run/tailscale
      - ./docktail-diagnostics:/diagnostics
    environment:
      - DIAGNOSTICS=true
```

Each record is one JSON line, written when the state changes and on the
heartbeat interval:

| Field | Meaning |
| --- | --- |
| `reason` | `start`, `change`, `heartbeat`, `stop`, or `error`. |
| `advertise_services` | The services this node currently advertises (`prefs.AdvertiseServices`). |
| `services[]` | Per service: ports, backend destination, and the `configured` / `advertised` pair. |
| `vip_fingerprint` | The exact tuple Tailscale hashes to decide whether to notify the Control Plane. The backend destination is not part of it, so re-pointing a service at a new container IP never changes it. |
| `daemon_health` | Health warnings reported by `tailscaled` itself. |
| `anomalies` | Services whose two halves disagree, for example configured but not advertised. |

Anomalies are also logged as warnings, once when they appear and once when they
clear, so they are visible without reading the file.

The file grows only when something changes, so it is safe to leave running for
days. It contains service names, ports, backend container IPs and node IDs; it
contains no credentials.

### Useful Links

- Tailscale Services documentation: `https://tailscale.com/kb/1552/tailscale-services`
- Tailscale Funnel documentation: `https://tailscale.com/kb/1311/tailscale-funnel`
- Tailscale service configuration reference: `https://tailscale.com/kb/1589/tailscale-services-configuration-file`
- Docker SDK for Go: `https://docs.docker.com/engine/api/sdk/`
