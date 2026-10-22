<!--
description: Migrate from TSDProxy to DockTail: map tsdproxy.* labels to docktail.* labels, set up Tailscale tags and OAuth, and move one container at a time.
nav: Migrate from TSDProxy
title: Migrate from TSDProxy to DockTail: Label Mapping and Steps
-->

## Migrate From TSDProxy To DockTail

[TSDProxy](https://github.com/almeidapaulopt/tsdproxy) and DockTail both put Docker containers on a tailnet from labels. They differ in how. TSDProxy v2 gives each proxied container its own Tailscale machine (an embedded `tsnet` node), so every app is a separate device in your admin console. DockTail runs next to the host's Tailscale and publishes each app as a [Tailscale Service](https://tailscale.com/kb/1552/tailscale-services) of that one host, so apps do not take device slots and each keeps a stable `https://<name>.your-tailnet.ts.net` address.

This guide maps TSDProxy v2 labels (checked against v2.3.4) to DockTail labels and walks through a switch, one container at a time.

### Is DockTail The Right Move?

Stay on TSDProxy, at least for some apps, if you rely on something DockTail does not do:

- TSDProxy's web dashboard, health checks, webhooks or Prometheus metrics. DockTail has no UI of its own; monitoring is the separate, paid [DockTail Cloud](../06-cloud.md).
- UDP or port ranges. DockTail serves HTTP, HTTPS, TCP and TLS-terminated TCP.
- No Tailscale on the host. DockTail needs a tagged `tailscaled`, either on the host or as a [sidecar container](../02-installation.md#tailscale-sidecar).
- Per-device features for each app, such as its own machine in the admin console.

The two can share a host while you move: TSDProxy only reads `tsdproxy.*` labels and DockTail only reads `docktail.*` labels. See the [DockTail vs TSDProxy](https://docktail.org/vs/tsdproxy/) comparison for the full picture.

### Step 1: Set Up The Tailscale Side

TSDProxy logs each node in with an auth key, an OAuth client or by hand from its dashboard. DockTail instead needs:

1. The Docker host (or a Tailscale sidecar) on your tailnet with a tag such as `tag:server`.
2. An ACL with `tag:server` and `tag:container` in `tagOwners` and an `autoApprovers.services` rule; see [ACL Configuration](../03-tailscale-admin.md#acl-configuration).
3. An OAuth client, so DockTail creates the service definitions; see [OAuth Credentials](../03-tailscale-admin.md#oauth-credentials).
4. For any app you had on Funnel, the `funnel` node attribute for `tag:server`; see [Funnel ACL](../03-tailscale-admin.md#funnel-acl).

### Step 2: Run DockTail

Add DockTail as in the [Quick Start](../01-quick-start.md), or the [sidecar setup](../02-installation.md#tailscale-sidecar) if the host does not run Tailscale. Leave TSDProxy running for now.

### Step 3: Translate The Labels

A typical TSDProxy v2 container:

```yaml
services:
  myapp:
    image: nginx:latest
    labels:
      - "tsdproxy.enable=true"
      - "tsdproxy.name=myapp"
      - "tsdproxy.port.1=443/https:80/http"
```

The same app with DockTail:

```yaml
services:
  myapp:
    image: nginx:latest
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=myapp"
      - "docktail.service.port=80"
      - "docktail.service.service-port=443"
```

`service-port=443` makes the tailnet side HTTPS with an automatic certificate, and the backend stays HTTP. Neither tool needs a `ports:` section: DockTail proxies straight to the container IP.

Label by label:

| TSDProxy v2 | DockTail |
| --- | --- |
| `tsdproxy.enable=true` | `docktail.service.enable=true` |
| `tsdproxy.name=myapp` (defaults to the container name) | `docktail.service.name=myapp` (required) |
| `tsdproxy.port.1=443/https:80/http` | `docktail.service.port=80` and `docktail.service.service-port=443` |
| `tsdproxy.port.1=80/http:80/http` | `docktail.service.port=80` (HTTP on port 80 is the default) |
| `tsdproxy.port.2=22/tcp:2222/tcp` | `docktail.service.1.name=myapp`, `docktail.service.1.port=2222`, `docktail.service.1.protocol=tcp`, `docktail.service.1.service-port=22` |
| `no_tlsvalidate` port option, for a backend with a self-signed certificate | `docktail.service.protocol=https+insecure` |
| `tailscale_funnel` port option | `docktail.funnel.enable=true` and `docktail.funnel.port=<container port>` |
| `tsdproxy.tags` | `docktail.tags` (needs API credentials) |
| `tsdproxy.autodetect` | Not needed: DockTail uses the container IP on its Docker network; `docktail.service.network` picks the network |
| `tsdproxy.container_port`, `tsdproxy.scheme`, `tsdproxy.funnel` (deprecated v1 labels) | `docktail.service.port`, `docktail.service.protocol`, `docktail.funnel.enable` |
| `tsdproxy.ephemeral`, `tsdproxy.authkey`, `tsdproxy.authkeyfile`, `tsdproxy.runwebclient`, `tsdproxy.tsnet_verbose` | No equivalent: there is no per-app Tailscale node to configure |
| `tsdproxy.dash.*`, `tsdproxy.health_check_*` | No equivalent in DockTail itself; [DockTail Cloud labels](../04-labels.md#docktail-cloud-labels) cover checks if you use Cloud |

Extra ports become numbered services of the same container; see [Multiple Services From One Container](../04-labels.md#multiple-services-from-one-container). Using the same `name` on a different `service-port` puts both ports on one address, like the SSH example in [Examples](../05-examples.md#https-with-tcp-ssh).

Two behaviours change:

- **Funnel URLs.** With TSDProxy a Funnel answers on the app's own machine name. With DockTail it answers on the DockTail host's machine name, `https://your-machine.your-tailnet.ts.net`, because Funnel runs on that node. Several apps can share it through different `docktail.funnel.path` values or Funnel ports `443`, `8443` and `10000`; see [Funnel Labels](../04-labels.md#funnel-labels).
- **Access rules.** ACL rules that name the old TSDProxy machines, by hostname or by tag, do not apply to Services. Write the rules for the services instead, by their tags (`tag:container` by default); see Tailscale's [Services documentation](https://tailscale.com/kb/1552/tailscale-services).

### Step 4: Switch One App

For each app:

1. Replace its `tsdproxy.*` labels with the `docktail.*` labels from Step 3.
2. In the Tailscale admin console, remove the app's old TSDProxy machine, so the old device no longer holds the name you want for the service.
3. Recreate the container: `docker compose up -d myapp`.
4. Approve the service in the Services tab if your ACL does not auto-approve it; see [Approve Services](../03-tailscale-admin.md#approve-services).
5. Open `https://myapp.your-tailnet.ts.net` and check it.

When the last app has moved, stop TSDProxy and delete its data volume.

### Troubleshooting

- **`service hosts must be tagged nodes` in the DockTail logs.** The host or sidecar is not tagged; see [Tailscale On Host](../02-installation.md#tailscale-on-host).
- **The service advertises but does not connect.** On rootless Docker the host often cannot reach container IPs; see [Rootless Docker](../02-installation.md#rootless-docker).
- **`Service endpoint conflict` in the logs.** Two containers claim the same service name and port, for example after copying labels. DockTail leaves that endpoint as it is until only one claims it; see [Service Name Conflicts Between Containers](../04-labels.md#service-name-conflicts-between-containers).
