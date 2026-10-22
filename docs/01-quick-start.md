<!--
description: Expose a Docker container as a Tailscale Service in minutes: tag the host, add DockTail to Docker Compose and label the app container.
title: Quick Start: Expose a Docker Container on Tailscale | DockTail
-->

## Quick Start

### Before you start

- [ ] The Docker host is on your tailnet and tagged `tag:server` (`sudo tailscale up --advertise-tags=tag:server --reset`). On macOS, Windows and many NAS devices, use the [Tailscale sidecar](02-installation.md#tailscale-sidecar) instead.
- [ ] Your tailnet policy lists `tag:server` and `tag:container` (the default service tag) in `tagOwners`, and `autoApprovers.services` lets `tag:server` advertise `tag:container` services. See [ACL Configuration](03-tailscale-admin.md#acl-configuration).
- [ ] Recommended: a Tailscale OAuth client for `tag:server` with **Services: Write** and **Devices Core: Write** (plus **Keys Auth Keys: Write** for the sidecar). No broader scope, such as `all`, is needed. Without credentials, you create each service in the admin console yourself. See [OAuth Credentials](03-tailscale-admin.md#oauth-credentials).

<details>
<summary>Policy snippet (tagOwners + autoApprovers)</summary>

```json
{
  "tagOwners": {
    "tag:server": ["autogroup:admin"],
    "tag:container": ["tag:server"]
  },
  "autoApprovers": {
    "services": {
      "tag:container": ["tag:server"]
    }
  }
}
```

</details>

### Run it

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
