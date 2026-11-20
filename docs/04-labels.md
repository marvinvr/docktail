<!--
description: Every DockTail Docker label: service name, ports, protocols, paths, PROXY protocol, HTTP-to-HTTPS redirects, multiple services per container, Funnel and Cloud labels.
title: DockTail Docker Labels: Tailscale Services and Funnel
-->

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

Set `docktail.service.direct=false` to use published host ports instead. Use this for legacy setups, or when host `tailscaled` cannot reach container IPs (common with [rootless Docker](02-installation.md#rootless-docker)).

### Service Labels

| Label | Required | Default | Description |
| --- | --- | --- | --- |
| `docktail.service.enable` | Yes | - | Enable a private Tailscale service for the container. |
| `docktail.service.name` | Yes | - | Service name, such as `web` or `api`. |
| `docktail.service.port` | Yes | - | Backend container port to proxy to. |
| `docktail.service.description` | No | Image `title:version` or `title`, else image tag | Human-readable description shown for the service in the Tailscale admin panel. Requires API credentials (synced to the Service definition's `comment`). When set, it overwrites the comment on every reconcile. If unset, DockTail falls back to the image's `org.opencontainers.image.title` and `org.opencontainers.image.version` labels, else the image reference (for example `nginx:1.27`), but only to fill an empty comment or to update one that names another version of the same image (`nginx:1.27` → `nginx:1.31`); a comment you wrote in the admin panel is kept. Set it empty to turn the fallback off and always leave the comment alone. |
| `docktail.service.direct` | No | `true` | Proxy directly to container IP instead of requiring a published host port. Set `false` when host `tailscaled` cannot reach container IPs (rootless Docker). |
| `docktail.service.network` | No | `bridge` or first available | Docker network used for direct container IP detection. |
| `docktail.service.protocol` | No | Smart | Backend protocol. |
| `docktail.service.service-port` | No | Smart | Port Tailscale listens on. |
| `docktail.service.service-protocol` | No | Smart | Tailscale-facing protocol. |
| `docktail.service.path` | No | `/` | URL path for HTTP(S) Serve traffic. Must start with `/`; not valid for TCP services. |
| `docktail.service.proxy-protocol` | No | off | PROXY protocol version (`1` or `2`) prepended on TCP forwards so the backend sees the tailnet client address. Only valid with `service-protocol` `tcp` or `tls-terminated-tcp`. |
| `docktail.service.http-redirect` | No | `true` for `service-protocol` `https`, else `false` | Answer `http://` on port 80 of the service with a redirect to its HTTPS endpoint. Set `false` to turn it off; see [HTTP To HTTPS Redirect](#http-to-https-redirect). |
| `docktail.tags` | No | `tag:container` | Comma-separated service tags. Labels are the source of truth; see the note below. |

Tags are synced to the tailnet Service definition through the Tailscale API and require API credentials. Labels are authoritative: DockTail reconciles tags on every cycle, so tags edited by hand in the Tailscale admin console are reverted to the label-declared set on the next reconcile. Containers without a `docktail.tags` label use `DEFAULT_SERVICE_TAGS`.

Smart defaults:

- `docktail.service.protocol` defaults to `https` when the backend port is `443`; otherwise it defaults to `http`.
- `docktail.service.service-port` defaults to `443` when `service-protocol` is `https`; otherwise it defaults to `80`.
- `docktail.service.service-protocol` defaults to `https` when the service port is `443`, to `tcp` when the backend protocol is TCP, and otherwise to `http`.
- `docktail.service.path` defaults to `/` and is passed to `tailscale serve --set-path`. Changing or removing the label reconciles the old handler before advertising the new path.
- `docktail.service.proxy-protocol` is opt-in and off by default. The backend must understand PROXY protocol; sending the header to something that does not (Postgres, Redis, raw TCP apps) will break the connection. Set it to `2` unless you specifically need version `1`. The label is rejected on HTTP/HTTPS services because Tailscale only supports PROXY protocol for TCP forwarding.

To expose an HTTP(S) service below a custom path:

```yaml
labels:
  - "docktail.service.enable=true"
  - "docktail.service.name=api"
  - "docktail.service.port=8000"
  - "docktail.service.path=/api"
```

### HTTP To HTTPS Redirect

Every HTTPS service also answers plain `http://` on port 80 with a redirect to its HTTPS address, so a browser sent to `http://myapp` does not hang. Nothing needs to be configured:

```yaml
labels:
  - "docktail.service.enable=true"
  - "docktail.service.name=myapp"
  - "docktail.service.port=3000"
  - "docktail.service.service-port=443"
```

`http://myapp/settings?tab=2`, `http://myapp.your-tailnet.ts.net/settings?tab=2` and the service's tailnet IP all redirect with `308 Permanent Redirect` to `https://myapp.your-tailnet.ts.net/settings?tab=2`. The path and query are kept. The target always uses the full MagicDNS name, because the certificate only covers that name (a short name like `https://myapp/` would fail the TLS check). A `service-port` other than `443` stays in the target, for example `https://myapp.your-tailnet.ts.net:8443/`.

To turn it off, set `docktail.service.http-redirect=false` (or `docktail.service.N.http-redirect=false` for an indexed service). Turning it off removes only the port 80 listener; HTTPS keeps serving.

How it behaves:

- **Port 80 already in use.** If any container declares port `80` for the same service name, that endpoint is kept and the redirect is skipped, with one info log. This never causes a [conflict](#service-name-conflicts-between-containers). When several HTTPS ports or containers share a service name, they share one redirect. It points to port `443` if one of them uses it, otherwise to the lowest port.
- **Service definition.** The Service definition must allow `tcp:80`. With [API credentials](03-tailscale-admin.md#oauth-credentials), DockTail adds `tcp:80` to new definitions and to existing ones that lack it. It never removes ports. Without credentials, DockTail logs one warning per service: add `tcp:80` to the Service's ports in the [Tailscale admin console](https://login.tailscale.com/admin/services), or plain HTTP gets no answer.
- **Requirements.** The host's `tailscaled` must be 1.92 or newer (the first release that can serve redirects), and MagicDNS must be on. Otherwise DockTail logs one warning and HTTPS works as before, without the redirect.
- **Invalid labels.** `http-redirect=true` is rejected on a service that is not `https`, or that uses `service-port` `80`.

The Tailscale CLI cannot configure redirects yet, so DockTail writes this one listener straight to `tailscaled`'s Serve configuration over the mounted Tailscale socket. This needs no permissions beyond what DockTail already uses.

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

Each indexed service requires its own `name` and `port`. Per-index overridable labels are `name`, `port`, `service-port`, `protocol`, `service-protocol`, `path`, `proxy-protocol`, `http-redirect`, and `description`; an indexed service without its own `description` gets the same image default, not the primary's description. Tags and network settings are inherited from the primary service config.

### Service Name Conflicts Between Containers

Several containers may share a service name as long as each uses a different `service-port`. A given service name and `service-port` pair can only be backed by one container per DockTail host. If two containers declare the same pair, for example after copy-pasting labels:

```yaml
services:
  app:
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=web"
      - "docktail.service.port=3000"
  app-copy:
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=web" # conflicts with app on svc:web:80
      - "docktail.service.port=3000"
```

DockTail does not pick a winner. It logs `Service endpoint conflict` with both container names, leaves that endpoint exactly as it is currently served (a container that already serves it keeps it; if nobody does, it stays unserved), pauses tag and description syncing and the removal of stale ports for that service name, and reports the reconciliation as failed until only one container claims the pair. All other services keep reconciling normally.

This is a guard against misconfiguration, not a tenant isolation boundary: DockTail trusts every container that can carry `docktail.*` labels, and once the original container stops, the remaining one becomes the sole claimant and is served.

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

### DockTail Cloud Labels

With [DockTail Cloud](06-cloud.md#docktail-cloud) enabled, `docktail.cloud.*` labels declare how Cloud monitors a container, next to the rest of its configuration. They change nothing about what DockTail serves on your tailnet, and are ignored when Cloud is not enabled.

| Label | Values | Description |
| --- | --- | --- |
| `docktail.cloud.ignore` | `true` / `false` | `true` keeps the container out of Cloud monitoring: it is not reported as a service, is never checked, captures no logs, and cannot be watched. It still appears in the host's container inventory, marked as ignored by label, and its Docker events (start, stop, exit) still show in the activity log like any other container's. |
| `docktail.cloud.logs` | `off` | Never capture incident log excerpts for this container. Capture can only be switched *on* in the Cloud dashboard. |
| `docktail.cloud.check.kind` | `tcp` / `http` | Kind of local check. Without the label, the dashboard's setting applies, else `tcp`. |
| `docktail.cloud.check.path` | `/path` | Path the HTTP check requests. Must start with `/`. Implies `check.kind=http`. |
| `docktail.cloud.check.expect-status` | `100`–`599` | The only HTTP status that counts as up. Without it, any status below 500 is up. Implies `check.kind=http`. |

```yaml
labels:
  - "docktail.service.enable=true"
  - "docktail.service.name=api"
  - "docktail.service.port=8000"
  - "docktail.cloud.check.path=/healthz"
  - "docktail.cloud.logs=off"
```

A label wins over the dashboard for the setting it names: while `docktail.cloud.logs=off` is set, the dashboard shows that service's log capture as set by label and dashboard changes to it do not apply. Settings without a label keep their dashboard value.

The check always targets the port DockTail already probes for the service; labels choose only how it is checked, never where. Cloud labels apply to every service a container publishes, including [numbered services](#multiple-services-from-one-container) — except that a service whose backend protocol is `tcp` or `tls-terminated-tcp` keeps its TCP check whatever the HTTP check labels say. An invalid value (for example `docktail.cloud.check.kind=udp`) or an unknown `docktail.cloud.*` label is logged as a warning and ignored. `check.path` and `check.expect-status` are ignored when `check.kind=tcp`.

Adding `docktail.cloud.ignore=true` to a container Cloud already monitors reads as that service being removed from the catalog; removing the label brings it back.
