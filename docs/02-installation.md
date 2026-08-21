## Installation

DockTail needs access to the Docker socket and a Tailscale socket. Use the host setup when Tailscale already runs on a Linux Docker host. Use the sidecar setup when the host should not install Tailscale directly, or when the host's Tailscale daemon cannot be shared with containers (macOS and Windows).

### Tailscale On Host

Use this setup when Tailscale is already installed on a Linux Docker host:

```yaml
services:
  docktail:
    image: ghcr.io/marvinvr/docktail:latest
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /var/run/tailscale:/var/run/tailscale
    environment:
      - TAILSCALE_OAUTH_CLIENT_ID=${TAILSCALE_OAUTH_CLIENT_ID}
      - TAILSCALE_OAUTH_CLIENT_SECRET=${TAILSCALE_OAUTH_CLIENT_SECRET}
```

Mount `/var/run/tailscale` as a directory rather than mounting the socket file directly. When `tailscaled` restarts, it recreates the socket with a new inode; a directory mount stays in sync.

The host setup only works on Linux. On macOS and Windows the Tailscale app does not expose a Unix socket at `/var/run/tailscale` (the local API is served over a localhost TCP port instead), and Docker Desktop runs containers in a virtual machine that cannot mount host Unix sockets anyway. If DockTail logs `dial unix /var/run/tailscale/tailscaled.sock: connect: no such file or directory`, switch to the [Tailscale Sidecar](#tailscale-sidecar) setup below.

The host machine must advertise a tag that matches your ACL auto-approvers:

```bash
sudo tailscale up --advertise-tags=tag:server --reset
```

The `--reset` flag briefly drops the Tailscale connection. If you are connected through SSH over Tailscale, your session may be interrupted until Tailscale reconnects.

### Rootless Docker

When Docker runs rootless, the DockTail container is your user, not root. Host `tailscaled` then rejects serve-config writes unless that user is the Tailscale operator:

```bash
sudo tailscale set --operator=$USER
```

`tailscale set --operator` does not drop the connection. The node still needs the ACL tag from the host setup above. If it is not tagged yet:

```bash
sudo tailscale up --advertise-tags=tag:server --operator=$USER --reset
```

`--operator` lets that Unix user manage `tailscaled` (serve, Funnel, `up`/`down`). That is required for the host setup with rootless Docker. If you do not want to grant the Docker user that access, use the [Tailscale Sidecar](#tailscale-sidecar) instead.

Rootless Docker also changes two paths:

**Docker socket.** Mount the user socket, not `/var/run/docker.sock`. Replace `1000` with your UID (`id -u`):

```yaml
volumes:
  - /run/user/1000/docker.sock:/var/run/docker.sock:ro
  - /var/run/tailscale:/var/run/tailscale
```

**Container reachability.** Default direct mode tells host `tailscaled` to connect to the container IP. Those IPs are often unreachable from the host in rootless Docker. If the service advertises but does not connect:

- Set `docktail.service.direct=false` and publish the container port, so Tailscale proxies to `127.0.0.1:<host-port>`.
- Or use the [Tailscale Sidecar](#tailscale-sidecar) on the same Docker network as the app. Prefer that over `network_mode: host` on rootless Docker.

### Tailscale Sidecar

Use this setup when the host does not run Tailscale directly. It is the required setup on macOS and Windows (Docker Desktop, OrbStack, Colima) and on many NAS devices, because `tailscaled` runs inside the Docker environment and shares its socket with DockTail through a named volume instead of a host mount:

```yaml
services:
  tailscale:
    image: tailscale/tailscale:latest
    hostname: docktail-host
    environment:
      - TS_AUTHKEY=${TAILSCALE_AUTH_KEY}
      - TS_EXTRA_ARGS=--advertise-tags=tag:server
      - TS_STATE_DIR=/var/lib/tailscale
      - TS_SOCKET=/var/run/tailscale/tailscaled.sock
    volumes:
      - tailscale-state:/var/lib/tailscale
      - tailscale-socket:/var/run/tailscale
      - /dev/net/tun:/dev/net/tun
    cap_add:
      - NET_ADMIN
      - SYS_MODULE
    network_mode: host
    restart: unless-stopped

  docktail:
    image: ghcr.io/marvinvr/docktail:latest
    depends_on:
      - tailscale
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - tailscale-socket:/var/run/tailscale
    environment:
      - TAILSCALE_OAUTH_CLIENT_ID=${TAILSCALE_OAUTH_CLIENT_ID}
      - TAILSCALE_OAUTH_CLIENT_SECRET=${TAILSCALE_OAUTH_CLIENT_SECRET}

volumes:
  tailscale-state:
  tailscale-socket:
```

Set `TAILSCALE_AUTH_KEY` to authenticate the Tailscale container. Generate it in the Tailscale Admin Console under Settings -> Keys. The sidecar should advertise `tag:server` so it can satisfy the ACL auto-approver example below.

The sidecar uses `network_mode: host` so it can reach container IPs on any Docker network. On Docker Desktop this requires enabling host networking under Settings -> Resources -> Network. Alternatively, remove `network_mode: host` and attach the sidecar to the same Docker network as the containers you expose. On rootless Docker, prefer the shared-network form; host networking is limited in that mode.
