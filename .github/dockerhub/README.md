# 🍸 DockTail

**Label-based Tailscale Services for Docker containers.**

[![Latest release](https://img.shields.io/github/v/release/marvinvr/docktail?label=release)](https://github.com/marvinvr/docktail/releases/latest) [![License: AGPL-3.0](https://img.shields.io/github/license/marvinvr/docktail)](https://github.com/marvinvr/docktail/blob/main/LICENSE) [![GitHub](https://img.shields.io/github/stars/marvinvr/docktail?logo=github&label=GitHub)](https://github.com/marvinvr/docktail) [![Docs](https://img.shields.io/badge/docs-docktail.org-blue)](https://docktail.org/docs/)

![Add docktail labels to a Compose service, run docker compose up -d, and open http://myapp.your-tailnet.ts.net](https://raw.githubusercontent.com/marvinvr/docktail/main/assets/demo.gif)

[![Featured on Tailscale Community](https://raw.githubusercontent.com/marvinvr/docktail/main/assets/tailscale-community-badge.svg)](https://tailscale.com/community/community-projects/docktail)

DockTail watches Docker containers, reads `docktail.*` labels, and exposes matching containers as native [Tailscale Services](https://tailscale.com/kb/1552/tailscale-services). App containers do not need published Docker ports; DockTail proxies directly to their Docker network IPs, and your apps do not each use up a Tailscale device slot.

## Image

The primary registry is GitHub Container Registry. Every release is mirrored here on Docker Hub with the same tags and digests, so use whichever you prefer:

| Registry | Image |
|---|---|
| GitHub (default in all docs) | `ghcr.io/marvinvr/docktail` |
| Docker Hub | `marvinvr/docktail` |

- Platforms: `linux/amd64`, `linux/arm64`, `linux/arm/v7`
- Tags: `latest`, the major (`1`), the minor (`1.8`) and the exact version (`1.8.5`) of each release; prereleases get a channel tag instead of `latest`

## Features

- Automatic container discovery through Docker labels
- Automatic Tailscale Service creation with OAuth or API key credentials
- HTTP, HTTPS, TCP and TLS-terminated TCP, with automatic Tailscale HTTPS certificates
- Optional PROXY protocol on TCP services so backends see the tailnet client IP
- Tailscale Funnel for public internet access
- Several Tailscale Services from one container
- Reconciliation when containers restart or their IPs change
- Stateless container, nothing to back up

## Before you start

- The Docker host is on your tailnet and tagged `tag:server`. On macOS, Windows and many NAS devices, run the [Tailscale sidecar](https://docktail.org/docs/installation/#tailscale-sidecar) instead.
- Your tailnet policy lists `tag:server` and `tag:container` in `tagOwners`, and `autoApprovers.services` lets `tag:server` advertise `tag:container` services.
- Recommended: a Tailscale OAuth client for `tag:server` with **Services: Write** and **Devices Core: Write**. Without credentials, you create each service in the admin console yourself. See [OAuth credentials](https://docktail.org/docs/tailscale-admin/#oauth-credentials).

## Quick Start

```yaml
services:
  docktail:
    image: ghcr.io/marvinvr/docktail:latest   # or marvinvr/docktail:latest
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

```bash
docker compose up -d
curl http://myapp.your-tailnet.ts.net
```

Expose an app with Tailscale HTTPS:

```yaml
labels:
  - "docktail.service.enable=true"
  - "docktail.service.name=api"
  - "docktail.service.port=3000"
  - "docktail.service.service-port=443"
```

Host setup, the sidecar, OAuth scopes, ACLs, every [label](https://docktail.org/docs/labels/), Funnel, hardening and [platform guides](https://docktail.org/docs/platforms/) (Podman, Synology, Unraid, TrueNAS, Swarm, Kubernetes) are in the [documentation](https://docktail.org/docs/).

## DockTail Cloud (optional)

[DockTail Cloud](https://docktail.org/cloud/) is a hosted dashboard for DockTail on several machines. It tells you which layer broke: the container, the Tailscale exposure or the host. Set `DOCKTAIL_CLOUD_KEY` on the same container to turn it on; without the key nothing is sent.

## Links

- Documentation: https://docktail.org/docs/
- Source, issues and releases: https://github.com/marvinvr/docktail
- DockTail Cloud: https://docktail.org/cloud/

License: [AGPL v3](https://github.com/marvinvr/docktail/blob/main/LICENSE). By [@marvinvr](https://marvinvr.ch).
