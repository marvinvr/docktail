# 🍸 DockTail

**Unleash your containers as Tailscale Services.**

<p align="center">
  <img src="assets/header.jpeg" alt="DockTail Header" width="100%">
</p>

DockTail watches Docker containers, reads `docktail.*` labels, and exposes matching containers as Tailscale Services. App containers do not need published Docker ports by default; DockTail proxies directly to their Docker network IPs.

## Why DockTail?

DockTail uses native Tailscale Services, not per-container Tailscale devices.

| | DockTail | TSDProxy | ScaleTail | tsbridge | Plain Services |
|---|---:|---:|---:|---:|---:|
| Native Tailscale Services | ✅ | ❌ | ❌ | ❌ | ✅ |
| Configured via Docker labels | ✅ | ✅ | ❌ | ✅ | ❌ |
| Apps do not consume separate Tailscale device slots | ✅ | ❌ | ❌ | ❌ | ✅ |
| No app port publishing | ✅ | ⚠️¹ | ⚠️² | ⚠️¹ | ⚠️³ |
| Automatic Docker reconciliation | ✅ | ✅ | ❌ | ✅ | ❌ |
| Low manual setup after install | ✅ | ✅ | ⚠️⁴ | ✅ | ❌ |

- ¹ Depends on proxy and Docker network setup.
- ² Depends on the sidecar template and app network setup.
- ³ You configure how the service host reaches the backend yourself.
- ⁴ ScaleTail is template-based, so each app usually starts from its own Compose recipe.

## Features

- Automatic Docker container discovery through labels.
- Automatic Tailscale service creation with OAuth or API key credentials.
- HTTP, HTTPS, TCP, and TLS-terminated TCP support.
- Tailscale HTTPS with automatic certificates.
- Tailscale Funnel for public internet access.
- Multiple Tailscale services from one container.
- Automatic reconciliation when containers restart or IPs change.
- Optional cleanup of unused Tailscale service definitions (opt-in, safe with multiple instances).
- Stateless Docker container runtime.

## Quick Start

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

```bash
docker compose up -d
curl http://myapp.your-tailnet.ts.net
```

This assumes the Docker host is connected to Tailscale and allowed to advertise services. See the full docs for host setup, sidecar setup, OAuth permissions, ACLs, labels, Funnel, and examples.

For Docker secrets or other mounted secret files, set `FILE__TAILSCALE_OAUTH_CLIENT_ID` / `FILE__TAILSCALE_OAUTH_CLIENT_SECRET` or `TAILSCALE_OAUTH_CLIENT_ID_FILE` / `TAILSCALE_OAUTH_CLIENT_SECRET_FILE` to the mounted file paths instead of putting the values directly in the environment.

## Common Examples

Expose an app with Tailscale HTTPS:

```yaml
labels:
  - "docktail.service.enable=true"
  - "docktail.service.name=api"
  - "docktail.service.port=3000"
  - "docktail.service.service-port=443"
```

Expose a database over TCP:

```yaml
labels:
  - "docktail.service.enable=true"
  - "docktail.service.name=db"
  - "docktail.service.port=5432"
  - "docktail.service.protocol=tcp"
  - "docktail.service.service-port=5432"
```

Expose a service publicly with Tailscale Funnel:

```yaml
labels:
  - "docktail.funnel.enable=true"
  - "docktail.funnel.port=3000"
  - "docktail.funnel.funnel-port=8443"
```

Mount an HTTP(S) Funnel at a path:

```yaml
labels:
  - "docktail.funnel.enable=true"
  - "docktail.funnel.port=3000"
  - "docktail.funnel.path=/webhook"
```

## Documentation

- Human docs: https://docktail.org/docs/
- Markdown docs: https://docktail.org/docs.md
- LLM guide: https://docktail.org/llms.txt
- Full LLM docs: https://docktail.org/llms-full.txt

The canonical documentation source lives in [`docs/`](docs/). Website docs are generated from those Markdown files.

## Build From Source

```bash
go build -o docktail .
docker build -t docktail:latest .
```

## Links

- [Tailscale Services Documentation](https://tailscale.com/kb/1552/tailscale-services)
- [Tailscale Funnel Documentation](https://tailscale.com/kb/1311/tailscale-funnel)
- [Tailscale Service Configuration Reference](https://tailscale.com/kb/1589/tailscale-services-configuration-file)
- [Docker SDK for Go](https://docs.docker.com/engine/api/sdk/)

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=marvinvr/docktail&type=date&legend=top-left)](https://www.star-history.com/#marvinvr/docktail&type=date&legend=top-left)

## License

AGPL v3

----
By [@marvinvr](https://marvinvr.ch)
