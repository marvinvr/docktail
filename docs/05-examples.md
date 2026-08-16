## Examples

These examples show the labels you add to application containers. They assume DockTail itself is already running on the same Docker host.

### Web Application

```yaml
services:
  nginx:
    image: nginx:latest
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=web"
      - "docktail.service.port=80"
```

Access it at `http://web.your-tailnet.ts.net`.

### Service With A Description

Add a description to label the service in the Tailscale admin panel. Requires API credentials (OAuth or API key), which DockTail syncs to the Service definition's `comment`.

```yaml
services:
  linkding:
    image: sissbruecker/linkding:latest
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=linkding"
      - "docktail.service.port=9090"
      - "docktail.service.description=Bookmark Manager"
```

### HTTPS With Auto TLS

```yaml
services:
  api:
    image: myapi:latest
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=api"
      - "docktail.service.port=3000"
      - "docktail.service.service-port=443"
```

Access it at `https://api.your-tailnet.ts.net`.

### HTTPS with TCP (SSH)

```yaml
services:
  forgejo:
    image: codeberg.org/forgejo/forgejo:latest
    labels:
        - "docktail.service.enable=true"
        - "docktail.service.name=forgejo"
        - "docktail.service.port=3000"
        - "docktail.service.service-port=443"
        - "docktail.service.1.enable=true"
        - "docktail.service.1.name=forgejo"
        - "docktail.service.1.port=2222"
        - "docktail.service.1.protocol=tcp"
        - "docktail.service.1.service-port=22"
```

Access 
- https at `https://forgejo.your-tailnet.ts.net` 
- ssh at `ssh -T -l git forgejo.your-tailnet.ts.net`

### Database Over TCP

```yaml
services:
  postgres:
    image: postgres:16
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=db"
      - "docktail.service.port=5432"
      - "docktail.service.protocol=tcp"
      - "docktail.service.service-port=5432"
```

### Reverse Proxy With Client IPs (PROXY Protocol)

TCP services normally hide the tailnet client address: the backend sees tailscaled's own IP. Set `docktail.service.proxy-protocol` so Tailscale prepends a [PROXY protocol](https://www.haproxy.com/blog/use-the-proxy-protocol-to-preserve-a-clients-ip-address) header. The backend (Traefik, Caddy, HAProxy, nginx, and similar) must be configured to accept it.

```yaml
services:
  traefik:
    image: traefik:latest
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=traefik"
      - "docktail.service.port=443"
      - "docktail.service.protocol=tcp"
      - "docktail.service.service-protocol=tcp"
      - "docktail.service.service-port=443"
      - "docktail.service.proxy-protocol=2"
      - "docktail.service.direct=false"
```

Without this label, access logs, rate limits, and IP allowlists on the reverse proxy only see tailscaled. HTTP/HTTPS DockTail services cannot use this label; Tailscale rejects PROXY protocol for those modes.

### Custom Docker Network

```yaml
services:
  app:
    image: myapp:latest
    networks:
      - backend
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=app"
      - "docktail.service.port=3000"
      - "docktail.service.network=backend"

networks:
  backend:
```

### Legacy Published-Port Mode

```yaml
services:
  app:
    image: myapp:latest
    ports:
      - "8080:3000"
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=app"
      - "docktail.service.port=3000"
      - "docktail.service.direct=false"
```

### Private Service Plus Public Funnel

```yaml
services:
  website:
    image: nginx:latest
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=website"
      - "docktail.service.port=80"
      - "docktail.service.service-port=443"
      - "docktail.funnel.enable=true"
      - "docktail.funnel.port=80"
```

Tailnet URL: `https://website.your-tailnet.ts.net`

Public Funnel URL: `https://your-machine.your-tailnet.ts.net`

### Funnel-Only Public Proxy

```yaml
services:
  immich-public-proxy:
    image: ghcr.io/immich-app/immich-public-proxy:latest
    labels:
      - "docktail.funnel.enable=true"
      - "docktail.funnel.port=3000"
      - "docktail.funnel.funnel-port=8443"
```

Access it publicly at `https://your-machine.your-tailnet.ts.net:8443`.

### Public Funnel Mounted At A Path

```yaml
services:
  webhook:
    image: my-webhook:latest
    labels:
      - "docktail.funnel.enable=true"
      - "docktail.funnel.port=3000"
      - "docktail.funnel.funnel-port=443"
      - "docktail.funnel.path=/webhook"
```

Access it publicly at `https://your-machine.your-tailnet.ts.net/webhook`.
