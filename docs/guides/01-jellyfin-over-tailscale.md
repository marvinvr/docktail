<!--
description: Expose Jellyfin over Tailscale with DockTail: one Docker Compose file, four labels, HTTPS at jellyfin.your-tailnet.ts.net, and no published ports.
nav: Jellyfin
title: Expose Jellyfin over Tailscale with DockTail (Docker Compose)
-->

## Expose Jellyfin Over Tailscale

This guide puts [Jellyfin](https://jellyfin.org) on your tailnet as its own Tailscale Service, at `https://jellyfin.your-tailnet.ts.net`. Every device signed in to your tailnet can stream from it, nothing is published on your router, and Jellyfin does not need a Docker port mapping.

### Before You Start

- DockTail runs on the Docker host, as in the [Quick Start](../01-quick-start.md).
- The host is tagged and allowed to advertise services; see [ACL Configuration](../03-tailscale-admin.md#acl-configuration).
- DockTail has [OAuth credentials](../03-tailscale-admin.md#oauth-credentials), so it creates the `jellyfin` service definition itself. Without them, create the service in the Tailscale admin console first.
- HTTPS is enabled for your tailnet (Tailscale admin console, DNS page), so Tailscale can issue the certificate.

### The Compose File

```yaml
services:
  jellyfin:
    image: jellyfin/jellyfin:latest
    restart: unless-stopped
    volumes:
      - ./config:/config
      - ./cache:/cache
      - /path/to/media:/media:ro
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=jellyfin"
      - "docktail.service.port=8096"
      - "docktail.service.service-port=443"
```

What the labels do:

- `docktail.service.port=8096` is Jellyfin's HTTP port inside the container. DockTail proxies straight to the container IP, so there is no `ports:` section.
- `docktail.service.service-port=443` makes the tailnet side HTTPS with an automatic certificate. The backend stays plain HTTP, because the backend protocol only defaults to HTTPS when the container port is `443`.

Start it:

```bash
docker compose up -d
```

The first time, the service may need approval in the Services tab of the Tailscale admin console; see [Approve Services](../03-tailscale-admin.md#approve-services). Then open `https://jellyfin.your-tailnet.ts.net` and run the Jellyfin setup wizard.

### Jellyfin Apps

In the Jellyfin apps (Android, iOS, Android TV, Swiftfin, Jellyfin Media Player), add the server as `https://jellyfin.your-tailnet.ts.net`. The device needs Tailscale running. Tailscale Serve passes WebSocket upgrades through, which Jellyfin uses for remote control and live updates.

### Client IPs In Jellyfin

Tailscale Serve sends the tailnet client's address in `X-Forwarded-For`. Jellyfin ignores that header unless the request comes from a known proxy, so by default its dashboard and logs show the address `tailscaled` connects from: with Tailscale on the host, the gateway of the Docker network Jellyfin is on.

To see real client addresses, look up that gateway and add it under **Dashboard > Networking > Known proxies**, then restart Jellyfin:

```bash
docker network inspect <project>_default --format '{{range .IPAM.Config}}{{.Gateway}}{{end}}'
```

This is optional. Streaming works either way.

### Host Networking For DLNA

Jellyfin's documentation says host networking is required for DLNA. DockTail works with it: a container with `network_mode: host` is served on `127.0.0.1`, so the same labels apply.

```yaml
services:
  jellyfin:
    image: jellyfin/jellyfin:latest
    restart: unless-stopped
    network_mode: host
    volumes:
      - ./config:/config
      - ./cache:/cache
      - /path/to/media:/media:ro
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=jellyfin"
      - "docktail.service.port=8096"
      - "docktail.service.service-port=443"
```

With host networking, Jellyfin also listens on port 8096 of every host interface, so it stays reachable on your LAN as before. In that case the known proxy to add is `127.0.0.1`.

### Optional: Share Publicly With Funnel

To let someone without Tailscale reach Jellyfin, add a [Funnel](../04-labels.md#funnel-labels). The host needs the `funnel` node attribute first; see [Funnel ACL](../03-tailscale-admin.md#funnel-acl).

```yaml
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=jellyfin"
      - "docktail.service.port=8096"
      - "docktail.service.service-port=443"
      - "docktail.funnel.enable=true"
      - "docktail.funnel.port=8096"
```

The public URL is the machine's name, `https://your-machine.your-tailnet.ts.net`, not `jellyfin.your-tailnet.ts.net`. Anyone on the internet then reaches the Jellyfin login page, so use strong passwords and keep Jellyfin updated.

### Troubleshooting

- **The service never shows up.** `docker compose logs docktail` names the cause, most often an untagged host or a missing auto-approver.
- **It shows up but does not connect.** On rootless Docker the host often cannot reach container IPs; see [Rootless Docker](../02-installation.md#rootless-docker).
- **Jellyfin is on a network DockTail does not pick.** If the container is attached to several networks, name the one to use with `docktail.service.network`; see [Service Labels](../04-labels.md#service-labels).
