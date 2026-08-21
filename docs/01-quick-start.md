## Quick Start

Add DockTail to your Docker Compose file alongside the service you want to expose:

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

This assumes a Linux Docker host that is already connected to Tailscale and allowed to advertise services. If it is not, continue with [Installation](02-installation.md#installation) and [Tailscale Admin Setup](03-tailscale-admin.md#tailscale-admin-setup). On macOS and Windows the host's Tailscale daemon cannot be shared with containers, so use the [Tailscale Sidecar](02-installation.md#tailscale-sidecar) setup instead. Rootless Docker on Linux needs an extra operator step; see [Rootless Docker](02-installation.md#rootless-docker).
