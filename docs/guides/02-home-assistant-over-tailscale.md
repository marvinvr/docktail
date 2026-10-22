<!--
description: Expose Home Assistant over Tailscale with DockTail: Docker Compose labels, host networking, the use_x_forwarded_for and trusted_proxies fix for 400 errors.
nav: Home Assistant
title: Expose Home Assistant over Tailscale with DockTail (Docker)
-->

## Expose Home Assistant Over Tailscale

This guide puts a Home Assistant container on your tailnet as a Tailscale Service, at `https://homeassistant.your-tailnet.ts.net`, for the browser and the companion app. It covers the one setting Home Assistant needs behind any reverse proxy, which is what Tailscale Serve is.

### Before You Start

- DockTail runs on the Docker host, as in the [Quick Start](../01-quick-start.md).
- The host is tagged and allowed to advertise services; see [ACL Configuration](../03-tailscale-admin.md#acl-configuration).
- DockTail has [OAuth credentials](../03-tailscale-admin.md#oauth-credentials), so it creates the `homeassistant` service definition itself.
- HTTPS is enabled for your tailnet (Tailscale admin console, DNS page).

### The Compose File

This is Home Assistant's own Compose example for a container install, with the DockTail labels added:

```yaml
services:
  homeassistant:
    image: ghcr.io/home-assistant/home-assistant:stable
    container_name: homeassistant
    restart: unless-stopped
    stop_grace_period: 60s
    privileged: true
    network_mode: host
    volumes:
      - ./config:/config
      - /etc/localtime:/etc/localtime:ro
      - /run/dbus:/run/dbus:ro
    environment:
      - TZ=Europe/Zurich
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=homeassistant"
      - "docktail.service.port=8123"
      - "docktail.service.service-port=443"
```

Home Assistant runs with `network_mode: host`, as its installation guide shows, so device discovery on your LAN keeps working. DockTail serves host-network containers on `127.0.0.1`, so the service points at `127.0.0.1:8123`. `service-port=443` makes the tailnet side HTTPS with an automatic certificate; Home Assistant itself stays on plain HTTP.

### Trust The Proxy

Tailscale Serve adds an `X-Forwarded-For` header with the tailnet client's address. Home Assistant rejects such requests unless it is told to trust the proxy: the page answers **400: Bad Request**, and the Home Assistant log says:

```text
A request from a reverse proxy was received from 127.0.0.1, but your HTTP integration is not set-up for reverse proxies
```

Add this to `configuration.yaml`, then restart Home Assistant:

```yaml
http:
  use_x_forwarded_for: true
  trusted_proxies:
    - 127.0.0.1
```

The two keys go together; Home Assistant refuses one without the other. Newer releases also offer the same settings in the UI as **Trust X-Forwarded-For** and **Trusted proxies**.

`127.0.0.1` is right for the setup above, where Home Assistant uses host networking. If you run it on a Docker bridge network instead, `tailscaled` connects from that network's gateway, and the log line names that address. Put the address, or its network such as `172.16.0.0/12`, in `trusted_proxies`. If the log says `Received X-Forwarded-For header from an untrusted proxy`, the address in `trusted_proxies` does not match.

### Start And Approve

```bash
docker compose up -d
```

The first time, the service may need approval in the Services tab of the Tailscale admin console; see [Approve Services](../03-tailscale-admin.md#approve-services). Then open `https://homeassistant.your-tailnet.ts.net`.

### Companion App

In the Home Assistant companion app, use `https://homeassistant.your-tailnet.ts.net` as the server address, on a phone that runs Tailscale. Tailscale Serve passes WebSocket upgrades through, which the frontend and the app rely on. If you set Home Assistant's own URLs (Settings, System, Network), use the same address so links and notifications point at the tailnet name.

### On The LAN

Nothing changes on your local network: with host networking, Home Assistant still listens on port 8123 of the host, so `http://<host-ip>:8123` keeps working next to the tailnet address.

### Troubleshooting

- **400: Bad Request.** The proxy is not trusted yet; see [Trust The Proxy](#trust-the-proxy).
- **The service never shows up.** `docker compose logs docktail` names the cause, most often an untagged host or a missing auto-approver.
- **It works in the browser but the app cannot connect.** Check that the phone is connected to Tailscale and that the server address uses `https://` without a port.
