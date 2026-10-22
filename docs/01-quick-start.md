<!--
description: Expose a Docker container as a Tailscale Service in minutes: tag the host, add DockTail to Docker Compose and label the app container.
title: Quick Start: Expose a Docker Container on Tailscale | DockTail
-->

## Quick Start

Before you start, the Docker host needs:

1. **Tailscale, tagged.** The host is on your tailnet and advertises a tag such as `tag:server` (`sudo tailscale up --advertise-tags=tag:server --reset`). On macOS, Windows and many NAS devices, run Tailscale as a [sidecar](02-installation.md#tailscale-sidecar) instead.
2. **An ACL that lets it advertise services.** `tag:server` and `tag:container` in `tagOwners`, and an `autoApprovers.services` rule; see [ACL Configuration](03-tailscale-admin.md#acl-configuration).
3. **Tailscale OAuth credentials** (recommended), so DockTail creates the service definitions itself; see [OAuth Credentials](03-tailscale-admin.md#oauth-credentials). Without them, create each service in the Tailscale Admin Console by hand.

Then add DockTail to your Docker Compose file alongside the service you want to expose:

```yaml
services:
  docktail:
    image: ghcr.io/marvinvr/docktail:latest
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /var/run/tailscale:/var/run/tailscale
    environment:
      # Optional but recommended. Enables automatic service creation.
      - TAILSCALE_OAUTH_CLIENT_ID=${TAILSCALE_OAUTH_CLIENT_ID}
      - TAILSCALE_OAUTH_CLIENT_SECRET=${TAILSCALE_OAUTH_CLIENT_SECRET}

  myapp:
    image: nginx:latest
    # No ports needed. DockTail proxies directly to the container IP.
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=myapp"
      - "docktail.service.port=80"
```

Start the stack:

```bash
docker compose up -d
```

Then open the service from your tailnet:

```bash
curl http://myapp.your-tailnet.ts.net
```

The first time a service is advertised, it may need approval in the Tailscale Admin Console's Services tab; see [Approve Services](03-tailscale-admin.md#approve-services). If it does not show up, `docker compose logs docktail` names the cause, and [Installation](02-installation.md#installation) and [Tailscale Admin Setup](03-tailscale-admin.md#tailscale-admin-setup) cover each step in detail. On macOS and Windows the host's Tailscale daemon cannot be shared with containers, so use the [Tailscale Sidecar](02-installation.md#tailscale-sidecar) setup instead. Rootless Docker on Linux needs an extra operator step; see [Rootless Docker](02-installation.md#rootless-docker). For Podman, Synology, Unraid, TrueNAS, Swarm and Kubernetes, see the [Platform Guides](02-platforms.md#platform-guides).
